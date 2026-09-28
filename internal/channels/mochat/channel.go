package mochat

import(
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

const maxResponseBytes=4<<20

type Channel struct{
	*channels.Base
	cfg Config
	client *http.Client

	mu sync.Mutex
	cursors map[string]int64
	seen map[string]map[string]struct{}
	seenFIFO map[string][]string
	workers map[string]context.CancelFunc
	cold map[string]bool
	wg sync.WaitGroup
}

func New(section channels.Section,publisher channels.InboundPublisher)(*Channel,error){
	cfg,err:=sectionConfig(section);if err!=nil{return nil,err}
	c:=&Channel{cfg:cfg,client:&http.Client{Timeout:90*time.Second,CheckRedirect:func(*http.Request,[]*http.Request)error{return http.ErrUseLastResponse}},cursors:map[string]int64{},seen:map[string]map[string]struct{}{},seenFIFO:map[string][]string{},workers:map[string]context.CancelFunc{},cold:map[string]bool{}}
	c.Base=channels.NewBase(c,section,publisher,channels.WithName(ChannelName),channels.WithDisplayName("MoChat"))
	return c,nil
}

func(c *Channel)Start(ctx context.Context)error{
	if err:=c.cfg.validateRuntime();err!=nil{return err}
	runCtx,cancel:=context.WithCancel(ctx);defer cancel()
	c.SetRunning(true);defer c.SetRunning(false)
	if err:=c.refresh(runCtx);err!=nil{c.Logger().Warn("MoChat initial discovery failed","error",err)}
	ticker:=time.NewTicker(c.cfg.RefreshInterval);defer ticker.Stop()
	for{
		select{
		case<-runCtx.Done():c.stopWorkers();return nil
		case<-ticker.C:if err:=c.refresh(runCtx);err!=nil{c.Logger().Warn("MoChat discovery failed","error",err)}
		}
	}
}
func(c *Channel)Stop(context.Context)error{c.SetRunning(false);c.stopWorkers();return nil}

func(c *Channel)Send(ctx context.Context,msg core.OutboundMessage)error{
	content:=strings.TrimSpace(msg.Content);if content==""{return nil}
	chat:=strings.TrimSpace(msg.ChatID);if chat==""{return errors.New("mochat: empty chat id")}
	path:="/api/claw/sessions/send";body:=map[string]any{"sessionId":chat,"content":content}
	if strings.HasPrefix(chat,"panel:"){id:=strings.TrimPrefix(chat,"panel:");path="/api/claw/groups/panels/send";body=map[string]any{"panelId":id,"content":content};if gid,ok:=msg.Metadata["group_id"].(string);ok&&gid!=""{body["groupId"]=gid}}
	if msg.ReplyTo!=nil&&strings.TrimSpace(*msg.ReplyTo)!=""{body["replyTo"]=strings.TrimSpace(*msg.ReplyTo)}
	var out map[string]any;return c.post(ctx,path,body,&out,c.cfg.WatchTimeout+10*time.Second)
}

func(c *Channel)refresh(ctx context.Context)error{
	sessions,autoSessions:=normalizeTargets(c.cfg.Sessions)
	panels,autoPanels:=normalizeTargets(c.cfg.Panels)
	if autoSessions{
		var data map[string]any
		if err:=c.post(ctx,"/api/claw/sessions/list",map[string]any{},&data,15*time.Second);err==nil{
			for _,item:=range list(data["sessions"]){if id:=field(item,"sessionId");id!=""{sessions=appendUnique(sessions,id)}}
		}else{return err}
	}
	if autoPanels{
		var data map[string]any
		if err:=c.post(ctx,"/api/claw/groups/get",map[string]any{},&data,15*time.Second);err==nil{
			for _,item:=range list(data["panels"]){if n,ok:=number(item["type"]);ok&&n!=0{continue};if id:=firstField(item,"id","_id");id!=""{panels=appendUnique(panels,id)}}
		}else{return err}
	}
	for _,id:=range sessions{c.ensureWorker(ctx,"session",id)}
	for _,id:=range panels{c.ensureWorker(ctx,"panel",id)}
	return nil
}

func(c *Channel)ensureWorker(parent context.Context,kind,id string){
	key:=kind+":"+id
	c.mu.Lock();if _,ok:=c.workers[key];ok{c.mu.Unlock();return};ctx,cancel:=context.WithCancel(parent);c.workers[key]=cancel;c.cold[key]=true;c.mu.Unlock()
	c.wg.Add(1);go func(){defer c.wg.Done();defer func(){c.mu.Lock();delete(c.workers,key);c.mu.Unlock()}();if kind=="session"{c.sessionWorker(ctx,id)}else{c.panelWorker(ctx,id)}}()
}

func(c *Channel)stopWorkers(){
	c.mu.Lock();cancels:=make([]context.CancelFunc,0,len(c.workers));for _,cancel:=range c.workers{cancels=append(cancels,cancel)};c.mu.Unlock()
	for _,cancel:=range cancels{cancel()};c.wg.Wait()
}

func(c *Channel)sessionWorker(ctx context.Context,id string){
	key:="session:"+id
	for ctx.Err()==nil&&c.IsRunning(){
		c.mu.Lock();cursor:=c.cursors[id];c.mu.Unlock()
		var data map[string]any
		err:=c.post(ctx,"/api/claw/sessions/watch",map[string]any{"sessionId":id,"cursor":cursor,"timeoutMs":c.cfg.WatchTimeout.Milliseconds(),"limit":c.cfg.WatchLimit},&data,c.cfg.WatchTimeout+10*time.Second)
		if err!=nil{if ctx.Err()!=nil{return};c.sleepRetry(ctx);continue}
		if n,ok:=number(data["cursor"]);ok{c.mu.Lock();if n>c.cursors[id]{c.cursors[id]=n};cold:=c.cold[key];c.cold[key]=false;c.mu.Unlock();if cold{continue}}
		for _,evt:=range list(data["events"]){
			if field(evt,"type")!="message.add"{continue}
			if seq,ok:=number(evt["seq"]);ok{c.mu.Lock();if seq>c.cursors[id]{c.cursors[id]=seq};c.mu.Unlock()}
			if err:=c.processEvent(ctx,key,id,evt,false);err!=nil{c.Logger().Warn("MoChat session dispatch failed","error",err)}
		}
	}
}

func(c *Channel)panelWorker(ctx context.Context,id string){
	key:="panel:"+id
	for ctx.Err()==nil&&c.IsRunning(){
		var data map[string]any
		err:=c.post(ctx,"/api/claw/groups/panels/messages",map[string]any{"panelId":id,"limit":c.cfg.WatchLimit},&data,20*time.Second)
		if err==nil{
			items:=list(data["messages"])
			c.mu.Lock();cold:=c.cold[key];c.cold[key]=false;c.mu.Unlock()
			for i:=len(items)-1;i>=0;i--{m:=items[i];event:=map[string]any{"payload":m};if gid:=field(data,"groupId");gid!=""{m["groupId"]=gid};if cold{c.remember(key,field(m,"messageId"));continue};if err:=c.processEvent(ctx,key,id,event,true);err!=nil{c.Logger().Warn("MoChat panel dispatch failed","error",err)}}
		}
		select{case<-ctx.Done():return;case<-time.After(c.cfg.RefreshInterval):}
	}
}

func(c *Channel)processEvent(ctx context.Context,key,targetID string,event map[string]any,isPanel bool)error{
	payload,_:=event["payload"].(map[string]any);if payload==nil{return nil}
	author:=field(payload,"author");if author==""||author==c.cfg.AgentUserID||!c.IsAllowed(author){return nil}
	messageID:=field(payload,"messageId");if messageID!=""&&c.remember(key,messageID){return nil}
	content:=normalizeContent(payload["content"]);if content==""{content="[empty message]"}
	chatID:=targetID;if isPanel{chatID="panel:"+targetID}
	meta:=map[string]any{"message_id":messageID};if gid:=field(payload,"groupId");gid!=""{meta["group_id"]=gid}
	return c.HandleMessage(ctx,channels.InboundRequest{SenderID:author,ChatID:chatID,Content:content,Metadata:meta,IsDM:!isPanel})
}

func(c *Channel)post(parent context.Context,path string,input any,output any,timeout time.Duration)error{
	ctx,cancel:=context.WithTimeout(parent,timeout);defer cancel()
	body,err:=json.Marshal(input);if err!=nil{return err}
	req,err:=http.NewRequestWithContext(ctx,http.MethodPost,c.cfg.BaseURL+path,bytes.NewReader(body));if err!=nil{return err}
	req.Header.Set("Content-Type","application/json");req.Header.Set("X-Claw-Token",c.cfg.ClawToken)
	resp,err:=c.client.Do(req);if err!=nil{return err};defer resp.Body.Close()
	data,err:=io.ReadAll(io.LimitReader(resp.Body,maxResponseBytes+1));if err!=nil{return err};if len(data)>maxResponseBytes{return errors.New("mochat: response too large")}
	if resp.StatusCode<200||resp.StatusCode>=300{return fmt.Errorf("mochat: HTTP %d: %s",resp.StatusCode,strings.TrimSpace(string(data)))}
	if len(data)==0{return nil}
	var raw map[string]any;if err:=json.Unmarshal(data,&raw);err!=nil{return err}
	if code,ok:=number(raw["code"]);ok{if code!=200{return fmt.Errorf("mochat: API error code %d: %s",code,field(raw,"message"))};if d,ok:=raw["data"].(map[string]any);ok{raw=d}else{raw=map[string]any{}}}
	if output!=nil{b,_:=json.Marshal(raw);return json.Unmarshal(b,output)};return nil
}
func(c *Channel)sleepRetry(ctx context.Context){select{case<-ctx.Done():case<-time.After(c.cfg.RetryDelay):}}

func(c *Channel)remember(key,id string)bool{if id==""{return false};c.mu.Lock();defer c.mu.Unlock();if c.seen[key]==nil{c.seen[key]=map[string]struct{}{}};if _,ok:=c.seen[key][id];ok{return true};c.seen[key][id]=struct{}{};c.seenFIFO[key]=append(c.seenFIFO[key],id);if len(c.seenFIFO[key])>1000{old:=c.seenFIFO[key][0];c.seenFIFO[key]=c.seenFIFO[key][1:];delete(c.seen[key],old)};return false}
func normalizeTargets(in []string)([]string,bool){var out []string;auto:=false;for _,v:=range in{v=strings.TrimSpace(v);if v=="*"{auto=true}else if v!=""{out=appendUnique(out,v)}};return out,auto}
func appendUnique(in []string,v string)[]string{for _,x:=range in{if x==v{return in}};return append(in,v)}
func list(v any)[]map[string]any{raw,ok:=v.([]any);if !ok{return nil};out:=make([]map[string]any,0,len(raw));for _,x:=range raw{if m,ok:=x.(map[string]any);ok{out=append(out,m)}};return out}
func field(m map[string]any,k string)string{if s,ok:=m[k].(string);ok{return strings.TrimSpace(s)};return ""}
func firstField(m map[string]any,ks ...string)string{for _,k:=range ks{if v:=field(m,k);v!=""{return v}};return ""}
func number(v any)(int64,bool){switch n:=v.(type){case float64:return int64(n),true;case int:return int64(n),true;case int64:return n,true};return 0,false}
func normalizeContent(v any)string{switch x:=v.(type){case string:return strings.TrimSpace(x);case map[string]any:for _,k:=range []string{"text","body","content"}{if s:=field(x,k);s!=""{return s}};b,_:=json.Marshal(x);return string(b);default:if v!=nil{b,_:=json.Marshal(v);return string(b)}};return ""}
