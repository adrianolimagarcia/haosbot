package feishu

import(
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

const maxPayloadBytes=3<<20

type Channel struct{
	*channels.Base
	cfg Config
	client *http.Client
	mu sync.Mutex
	server *http.Server
	token string
	tokenExpires time.Time
	seen map[string]struct{}
	seenFIFO []string
}

func New(section channels.Section,publisher channels.InboundPublisher)(*Channel,error){
	cfg,err:=sectionConfig(section);if err!=nil{return nil,err}
	c:=&Channel{cfg:cfg,client:&http.Client{Timeout:30*time.Second,CheckRedirect:func(*http.Request,[]*http.Request)error{return http.ErrUseLastResponse}},seen:map[string]struct{}{}}
	c.Base=channels.NewBase(c,section,publisher,channels.WithName(ChannelName),channels.WithDisplayName("Feishu / Lark"))
	return c,nil
}
func(c *Channel)Start(ctx context.Context)error{
	if err:=c.cfg.validateRuntime();err!=nil{return err}
	l,err:=net.Listen("tcp",c.cfg.ListenAddr);if err!=nil{return fmt.Errorf("feishu: listen: %w",err)}
	s:=&http.Server{Handler:c.handler(),ReadHeaderTimeout:5*time.Second,ReadTimeout:20*time.Second,WriteTimeout:10*time.Second,IdleTimeout:60*time.Second}
	c.mu.Lock();c.server=s;c.mu.Unlock();c.SetRunning(true)
	defer func(){c.SetRunning(false);c.mu.Lock();c.server=nil;c.mu.Unlock()}()
	done:=make(chan struct{});go func(){select{case<-ctx.Done():x,cancel:=context.WithTimeout(context.Background(),5*time.Second);defer cancel();_=s.Shutdown(x);case<-done:}}()
	err=s.Serve(l);close(done);if errors.Is(err,http.ErrServerClosed)||ctx.Err()!=nil{return nil};return err
}
func(c *Channel)Stop(ctx context.Context)error{c.SetRunning(false);c.mu.Lock();s:=c.server;c.mu.Unlock();if s==nil{return nil};return s.Shutdown(ctx)}
func(c *Channel)Send(ctx context.Context,msg core.OutboundMessage)error{
	chat:=strings.TrimSpace(msg.ChatID);if chat==""||strings.TrimSpace(msg.Content)==""{return nil}
	token,err:=c.tenantToken(ctx);if err!=nil{return err}
	contentBytes,_:=json.Marshal(map[string]string{"text":msg.Content})
	payload:=map[string]any{"receive_id":chat,"msg_type":"text","content":string(contentBytes)}
	body,_:=json.Marshal(payload)
	endpoint:=c.cfg.apiBase()+"/open-apis/im/v1/messages?receive_id_type=chat_id"
	req,err:=http.NewRequestWithContext(ctx,http.MethodPost,endpoint,bytes.NewReader(body));if err!=nil{return err}
	req.Header.Set("Authorization","Bearer "+token);req.Header.Set("Content-Type","application/json")
	resp,err:=c.client.Do(req);if err!=nil{return err};defer resp.Body.Close()
	data,_:=io.ReadAll(io.LimitReader(resp.Body,maxPayloadBytes+1))
	if resp.StatusCode<200||resp.StatusCode>=300{return fmt.Errorf("feishu: send HTTP %d: %s",resp.StatusCode,strings.TrimSpace(string(data)))}
	var result struct{Code int `json:"code"`;Msg string `json:"msg"`};if len(data)>0&&json.Unmarshal(data,&result)==nil&&result.Code!=0{return fmt.Errorf("feishu: send API code %d: %s",result.Code,result.Msg)}
	return nil
}

func(c *Channel)handler()http.Handler{mux:=http.NewServeMux();mux.HandleFunc(c.cfg.WebhookPath,c.webhook);return mux}
func(c *Channel)webhook(w http.ResponseWriter,r *http.Request){
	if r.Method!=http.MethodPost{w.Header().Set("Allow","POST");http.Error(w,"Method not allowed",405);return}
	raw,err:=io.ReadAll(http.MaxBytesReader(w,r.Body,maxPayloadBytes));if err!=nil{http.Error(w,"Invalid payload",400);return}
	if c.cfg.SignatureKey!=""&&!validSignature(raw,c.cfg.SignatureKey,r.Header.Get("X-Lark-Request-Timestamp"),r.Header.Get("X-Lark-Request-Nonce"),r.Header.Get("X-Lark-Signature")){http.Error(w,"Invalid signature",401);return}
	var env webhookEnvelope;if err:=json.Unmarshal(raw,&env);err!=nil{http.Error(w,"Invalid JSON",400);return}
	if env.Encrypt!=""{http.Error(w,"Encrypted Feishu webhook payloads are not enabled in this lightweight transport",501);return}
	token:=env.Token;if token==""{token=env.Header.Token};if token!=c.cfg.VerificationToken{http.Error(w,"Invalid verification token",401);return}
	if env.Type=="url_verification"||env.Challenge!=""{w.Header().Set("Content-Type","application/json");_=json.NewEncoder(w).Encode(map[string]string{"challenge":env.Challenge});return}
	if env.Header.EventType!="im.message.receive_v1"{w.WriteHeader(200);return}
	if env.Header.EventID!=""&&c.remember(env.Header.EventID){w.WriteHeader(200);return}
	sender:=strings.TrimSpace(env.Event.Sender.SenderID.OpenID);if sender==""{sender=strings.TrimSpace(env.Event.Sender.SenderID.UserID)}
	if sender==""||!c.IsAllowed(sender){w.WriteHeader(200);return}
	m:=env.Event.Message;if m.ChatID==""||m.MessageType!="text"{w.WriteHeader(200);return}
	var content struct{Text string `json:"text"`};if json.Unmarshal([]byte(m.Content),&content)!=nil||strings.TrimSpace(content.Text)==""{w.WriteHeader(200);return}
	text:=stripMentions(content.Text,env.Event.Message.Mentions)
	if text==""{return}
	if err:=c.HandleMessage(r.Context(),channels.InboundRequest{SenderID:sender,ChatID:m.ChatID,Content:text,Metadata:map[string]any{"event_id":env.Header.EventID,"message_id":m.MessageID,"chat_type":m.ChatType},IsDM:m.ChatType=="p2p"});err!=nil{
		if env.Header.EventID!=""{c.forget(env.Header.EventID)};http.Error(w,"Temporary dispatch failure",503);return
	}
	w.WriteHeader(200)
}

type webhookEnvelope struct{
	Schema string `json:"schema"`
	Type string `json:"type"`
	Token string `json:"token"`
	Challenge string `json:"challenge"`
	Encrypt string `json:"encrypt"`
	Header struct{EventID string `json:"event_id"`;EventType string `json:"event_type"`;Token string `json:"token"`;AppID string `json:"app_id"`} `json:"header"`
	Event struct{
		Sender struct{SenderID struct{OpenID string `json:"open_id"`;UserID string `json:"user_id"`} `json:"sender_id"`} `json:"sender"`
		Message struct{
			MessageID string `json:"message_id"`;ChatID string `json:"chat_id"`;ChatType string `json:"chat_type"`;MessageType string `json:"message_type"`;Content string `json:"content"`
			Mentions []struct{Key string `json:"key"`;Name string `json:"name"`} `json:"mentions"`
		} `json:"message"`
	} `json:"event"`
}

func validSignature(body []byte,key,timestamp,nonce,provided string)bool{
	if timestamp==""||nonce==""||provided==""{return false}
	sum:=sha256.Sum256(append([]byte(timestamp+nonce+key),body...));expected:=hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(strings.TrimSpace(provided))),[]byte(expected))==1
}
func stripMentions(text string,mentions []struct{Key string `json:"key"`;Name string `json:"name"`})string{for _,m:=range mentions{if m.Key!=""{text=strings.ReplaceAll(text,m.Key," ")}};return strings.Join(strings.Fields(text)," ")}

func(c *Channel)tenantToken(ctx context.Context)(string,error){
	c.mu.Lock();if c.token!=""&&time.Until(c.tokenExpires)>time.Minute{t:=c.token;c.mu.Unlock();return t,nil};c.mu.Unlock()
	payload,_:=json.Marshal(map[string]string{"app_id":c.cfg.AppID,"app_secret":c.cfg.AppSecret})
	req,err:=http.NewRequestWithContext(ctx,http.MethodPost,c.cfg.apiBase()+"/open-apis/auth/v3/tenant_access_token/internal",bytes.NewReader(payload));if err!=nil{return "",err};req.Header.Set("Content-Type","application/json")
	resp,err:=c.client.Do(req);if err!=nil{return "",err};defer resp.Body.Close();data,_:=io.ReadAll(io.LimitReader(resp.Body,maxPayloadBytes+1))
	if resp.StatusCode<200||resp.StatusCode>=300{return "",fmt.Errorf("feishu: token HTTP %d",resp.StatusCode)}
	var p struct{Code int `json:"code"`;Msg string `json:"msg"`;Token string `json:"tenant_access_token"`;Expire int `json:"expire"`}
	if err:=json.Unmarshal(data,&p);err!=nil{return "",err};if p.Code!=0||p.Token==""{return "",fmt.Errorf("feishu: token API code %d: %s",p.Code,p.Msg)};if p.Expire<=0{p.Expire=7200}
	c.mu.Lock();c.token=p.Token;c.tokenExpires=time.Now().Add(time.Duration(p.Expire)*time.Second);c.mu.Unlock();return p.Token,nil
}
func(c *Channel)remember(id string)bool{c.mu.Lock();defer c.mu.Unlock();if _,ok:=c.seen[id];ok{return true};c.seen[id]=struct{}{};c.seenFIFO=append(c.seenFIFO,id);if len(c.seenFIFO)>2000{delete(c.seen,c.seenFIFO[0]);c.seenFIFO=c.seenFIFO[1:]};return false}
func(c *Channel)forget(id string){c.mu.Lock();defer c.mu.Unlock();delete(c.seen,id);for i,v:=range c.seenFIFO{if v==id{c.seenFIFO=append(c.seenFIFO[:i],c.seenFIFO[i+1:]...);break}}}

var _=url.PathEscape
