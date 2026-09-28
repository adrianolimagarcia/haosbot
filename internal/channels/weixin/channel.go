package weixin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

const (
	channelVersion = "2.4.6"
	appID = "bot"
	maxResponseBytes = 2 << 20
	maxTextRunes = 1800
	messageTypeBot = 2
	messageStateFinish = 2
	itemText = 1
)

type Channel struct {
	*channels.Base
	cfg Config
	client *http.Client

	mu sync.RWMutex
	cursor string
	contextTokens map[string]string
	seen map[string]struct{}
	seenFIFO []string
}

func New(section channels.Section, publisher channels.InboundPublisher) (*Channel,error) {
	cfg,err:=sectionConfig(section); if err!=nil { return nil,err }
	c:=&Channel{
		cfg:cfg,
		client:&http.Client{CheckRedirect:func(*http.Request,[]*http.Request) error{return http.ErrUseLastResponse}},
		contextTokens:make(map[string]string),
		seen:make(map[string]struct{}),
	}
	c.Base=channels.NewBase(c,section,publisher,channels.WithName(ChannelName),channels.WithDisplayName("WeChat / Weixin"))
	return c,nil
}

func (c *Channel) ProgressTransportDefaults()(bool,bool,bool){ return false,false,true }

func (c *Channel) Start(ctx context.Context) error {
	if err:=c.cfg.validateRuntime(); err!=nil { return err }
	c.SetRunning(true); defer c.SetRunning(false)
	_ = c.apiPost(ctx,"ilink/bot/msg/notifystart",map[string]any{},nil,15*time.Second)
	backoff:=time.Second
	for c.IsRunning() && ctx.Err()==nil {
		err:=c.pollOnce(ctx)
		if err==nil { backoff=time.Second; continue }
		if ctx.Err()!=nil { return nil }
		c.Logger().Warn("WeChat long-poll failed; reconnecting","error",err)
		timer:=time.NewTimer(backoff)
		select { case <-ctx.Done(): timer.Stop(); return nil; case <-timer.C: }
		if backoff < 30*time.Second { backoff*=2; if backoff>30*time.Second { backoff=30*time.Second } }
	}
	return nil
}

func (c *Channel) Stop(ctx context.Context) error {
	c.SetRunning(false)
	stopCtx,cancel:=context.WithTimeout(ctx,5*time.Second); defer cancel()
	_ = c.apiPost(stopCtx,"ilink/bot/msg/notifystop",map[string]any{},nil,5*time.Second)
	return nil
}

func (c *Channel) Send(ctx context.Context,msg core.OutboundMessage) error {
	chatID:=strings.TrimSpace(msg.ChatID)
	if chatID=="" || strings.TrimSpace(msg.Content)=="" { return nil }
	c.mu.RLock(); contextToken:=c.contextTokens[chatID]; c.mu.RUnlock()
	if contextToken=="" { return fmt.Errorf("weixin: no fresh context_token for %s; wait for a new inbound message",chatID) }
	parts:=splitText(msg.Content,maxTextRunes)
	for i,part:=range parts {
		clientID:=fmt.Sprintf("haos-%d-%d",time.Now().UnixNano(),i)
		payload:=map[string]any{
			"msg":map[string]any{
				"from_user_id":"","to_user_id":chatID,"client_id":clientID,
				"message_type":messageTypeBot,"message_state":messageStateFinish,
				"item_list":[]any{map[string]any{"type":itemText,"text_item":map[string]any{"text":part}}},
				"context_token":contextToken,
			},
			"base_info":baseInfo(),
		}
		if err:=c.apiPost(ctx,"ilink/bot/sendmessage",payload,nil,20*time.Second); err!=nil { return err }
	}
	return nil
}

type updatesResponse struct {
	Ret int `json:"ret"`
	ErrCode int `json:"errcode"`
	ErrMsg string `json:"errmsg"`
	GetUpdatesBuf string `json:"get_updates_buf"`
	LongPollingTimeoutMS int `json:"longpolling_timeout_ms"`
	Msgs []struct {
		MessageID any `json:"message_id"`
		Seq any `json:"seq"`
		FromUserID string `json:"from_user_id"`
		MessageType int `json:"message_type"`
		ContextToken string `json:"context_token"`
		CreateTimeMS any `json:"create_time_ms"`
		ItemList []struct {
			Type int `json:"type"`
			TextItem struct{ Text string `json:"text"` } `json:"text_item"`
		} `json:"item_list"`
	} `json:"msgs"`
}

func (c *Channel) pollOnce(ctx context.Context) error {
	c.mu.RLock(); cursor:=c.cursor; c.mu.RUnlock()
	payload:=map[string]any{"get_updates_buf":cursor,"base_info":baseInfo()}
	var response updatesResponse
	timeout:=c.cfg.PollTimeout+10*time.Second
	if err:=c.apiPost(ctx,"ilink/bot/getupdates",payload,&response,timeout); err!=nil { return err }
	if response.GetUpdatesBuf!="" { c.mu.Lock(); c.cursor=response.GetUpdatesBuf; c.mu.Unlock() }
	for _,msg:=range response.Msgs {
		if msg.MessageType==messageTypeBot || strings.TrimSpace(msg.FromUserID)=="" { continue }
		id:=firstID(msg.MessageID,msg.Seq,msg.FromUserID,msg.CreateTimeMS)
		if c.wasSeen(id) { continue }
		contentParts:=make([]string,0,len(msg.ItemList))
		for _,item:=range msg.ItemList {
			if item.Type==itemText && strings.TrimSpace(item.TextItem.Text)!="" { contentParts=append(contentParts,strings.TrimSpace(item.TextItem.Text)) }
		}
		content:=strings.Join(contentParts,"\n")
		if content=="" { continue }
		if msg.ContextToken!="" {
			c.mu.Lock(); c.contextTokens[msg.FromUserID]=msg.ContextToken; c.mu.Unlock()
		}
		if !c.IsAllowed(msg.FromUserID) { continue }
		if err:=c.HandleMessage(ctx,channels.InboundRequest{
			SenderID:msg.FromUserID,ChatID:msg.FromUserID,Content:content,
			Metadata:map[string]any{"message_id":id},IsDM:!strings.HasSuffix(msg.FromUserID,"@chatroom"),
		}); err!=nil {
			c.forgetSeen(id)
			return err
		}
	}
	return nil
}

func (c *Channel) apiPost(parent context.Context,endpoint string,input any,output any,timeout time.Duration) error {
	ctx,cancel:=context.WithTimeout(parent,timeout); defer cancel()
	body,err:=json.Marshal(input); if err!=nil{return err}
	req,err:=http.NewRequestWithContext(ctx,http.MethodPost,c.cfg.BaseURL+"/"+strings.TrimLeft(endpoint,"/"),bytes.NewReader(body)); if err!=nil{return err}
	for k,v:=range c.headers(){req.Header.Set(k,v)}
	resp,err:=c.client.Do(req); if err!=nil{return err}
	defer resp.Body.Close()
	data,err:=io.ReadAll(io.LimitReader(resp.Body,maxResponseBytes+1)); if err!=nil{return err}
	if len(data)>maxResponseBytes{return errors.New("weixin: API response exceeded size limit")}
	if resp.StatusCode<200 || resp.StatusCode>=300{return fmt.Errorf("weixin: API returned HTTP %d: %s",resp.StatusCode,strings.TrimSpace(string(data)))}
	var envelope struct{ Ret int `json:"ret"`; ErrCode int `json:"errcode"`; ErrMsg string `json:"errmsg"` }
	if len(data)>0 {
		if err:=json.Unmarshal(data,&envelope); err!=nil{return fmt.Errorf("weixin: decode response: %w",err)}
		if envelope.Ret!=0 || envelope.ErrCode!=0{return fmt.Errorf("weixin: %s failed (ret=%d errcode=%d): %s",endpoint,envelope.Ret,envelope.ErrCode,envelope.ErrMsg)}
		if output!=nil { if err:=json.Unmarshal(data,output); err!=nil{return err} }
	}
	return nil
}

func (c *Channel) headers() map[string]string {
	var raw [4]byte
	if _,err:=rand.Read(raw[:]); err!=nil { binary.BigEndian.PutUint32(raw[:],uint32(time.Now().UnixNano())) }
	uin:=strconv.FormatUint(uint64(binary.BigEndian.Uint32(raw[:])),10)
	headers:=map[string]string{
		"X-WECHAT-UIN":base64.StdEncoding.EncodeToString([]byte(uin)),
		"Content-Type":"application/json",
		"AuthorizationType":"ilink_bot_token",
		"iLink-App-Id":appID,
		"iLink-App-ClientVersion":strconv.Itoa(buildClientVersion(channelVersion)),
		"Authorization":"Bearer "+c.cfg.Token,
	}
	if c.cfg.RouteTag!="" { headers["SKRouteTag"]=c.cfg.RouteTag }
	return headers
}

func buildClientVersion(version string) int {
	parts:=strings.Split(version,".")
	num:=func(i int) int { if i>=len(parts){return 0}; n,_:=strconv.Atoi(parts[i]); return n&0xff }
	return num(0)<<16 | num(1)<<8 | num(2)
}
func baseInfo() map[string]string { return map[string]string{"channel_version":channelVersion,"bot_agent":"haosbot/go"} }

func splitText(text string,maxRunes int) []string {
	text=strings.TrimSpace(text); if text=="" {return nil}
	if utf8.RuneCountInString(text)<=maxRunes{return []string{text}}
	r:=[]rune(text); out:=make([]string,0,(len(r)+maxRunes-1)/maxRunes)
	for len(r)>0 { n:=maxRunes; if len(r)<n{n=len(r)}; out=append(out,string(r[:n])); r=r[n:] }
	return out
}
func firstID(values ...any) string {
	for _,v:=range values { switch x:=v.(type){case string: if x!=""{return x}; case float64:return strconv.FormatInt(int64(x),10); case json.Number: return x.String()} }
	return fmt.Sprintf("anon-%d",time.Now().UnixNano())
}
func (c *Channel) wasSeen(id string) bool {
	c.mu.Lock(); defer c.mu.Unlock()
	if _,ok:=c.seen[id];ok{return true}; c.seen[id]=struct{}{}; c.seenFIFO=append(c.seenFIFO,id)
	if len(c.seenFIFO)>1000 {delete(c.seen,c.seenFIFO[0]);c.seenFIFO=c.seenFIFO[1:]}; return false
}
func (c *Channel) forgetSeen(id string) {
	c.mu.Lock(); defer c.mu.Unlock(); delete(c.seen,id)
	for i,v:=range c.seenFIFO {if v==id{c.seenFIFO=append(c.seenFIFO[:i],c.seenFIFO[i+1:]...);break}}
}
