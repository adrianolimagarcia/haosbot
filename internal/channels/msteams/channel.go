package msteams

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

const maxPayloadBytes=2<<20

type conversationRef struct {
	ServiceURL string
	ConversationID string
	ActivityID string
}

type Channel struct {
	*channels.Base
	cfg Config
	client *http.Client

	mu sync.RWMutex
	server *http.Server
	refs map[string]conversationRef
	token string
	tokenExpires time.Time
	jwks map[string]*rsa.PublicKey
	jwksExpires time.Time
}

func New(section channels.Section,publisher channels.InboundPublisher)(*Channel,error){
	cfg,err:=sectionConfig(section);if err!=nil{return nil,err}
	c:=&Channel{
		cfg:cfg,
		client:&http.Client{Timeout:30*time.Second,CheckRedirect:func(*http.Request,[]*http.Request)error{return http.ErrUseLastResponse}},
		refs:make(map[string]conversationRef),
		jwks:make(map[string]*rsa.PublicKey),
	}
	c.Base=channels.NewBase(c,section,publisher,channels.WithName(ChannelName),channels.WithDisplayName("Microsoft Teams"))
	return c,nil
}

func(c *Channel)Start(ctx context.Context)error{
	if err:=c.cfg.validateRuntime();err!=nil{return err}
	listener,err:=net.Listen("tcp",c.cfg.ListenAddr);if err!=nil{return fmt.Errorf("msteams: listen: %w",err)}
	server:=&http.Server{Handler:c.handler(),ReadHeaderTimeout:5*time.Second,ReadTimeout:30*time.Second,WriteTimeout:15*time.Second,IdleTimeout:60*time.Second}
	c.mu.Lock();c.server=server;c.mu.Unlock()
	c.SetRunning(true)
	defer func(){c.SetRunning(false);c.mu.Lock();c.server=nil;c.mu.Unlock()}()
	done:=make(chan struct{})
	go func(){select{case<-ctx.Done():shutdown,cancel:=context.WithTimeout(context.Background(),5*time.Second);defer cancel();_=server.Shutdown(shutdown);case<-done:}}()
	err=server.Serve(listener);close(done)
	if errors.Is(err,http.ErrServerClosed)||ctx.Err()!=nil{return nil}
	return err
}
func(c *Channel)Stop(ctx context.Context)error{c.SetRunning(false);c.mu.RLock();s:=c.server;c.mu.RUnlock();if s==nil{return nil};return s.Shutdown(ctx)}

func(c *Channel)Send(ctx context.Context,msg core.OutboundMessage)error{
	c.mu.RLock();ref,ok:=c.refs[msg.ChatID];c.mu.RUnlock()
	if !ok{return fmt.Errorf("msteams: conversation ref not found for %s",msg.ChatID)}
	if !c.trustedServiceURL(ref.ServiceURL){return errors.New("msteams: refusing untrusted serviceUrl")}
	token,err:=c.accessToken(ctx);if err!=nil{return err}
	endpoint:=strings.TrimRight(ref.ServiceURL,"/")+"/v3/conversations/"+url.PathEscape(ref.ConversationID)+"/activities"
	payload:=map[string]any{"type":"message","text":msg.Content}
	if c.cfg.ReplyInThread&&ref.ActivityID!=""{payload["replyToId"]=ref.ActivityID}
	body,err:=json.Marshal(payload);if err!=nil{return err}
	req,err:=http.NewRequestWithContext(ctx,http.MethodPost,endpoint,bytes.NewReader(body));if err!=nil{return err}
	req.Header.Set("Authorization","Bearer "+token);req.Header.Set("Content-Type","application/json")
	resp,err:=c.client.Do(req);if err!=nil{return err};defer resp.Body.Close()
	data,_:=io.ReadAll(io.LimitReader(resp.Body,maxPayloadBytes+1))
	if resp.StatusCode<200||resp.StatusCode>=300{return fmt.Errorf("msteams: send HTTP %d: %s",resp.StatusCode,strings.TrimSpace(string(data)))}
	return nil
}

func(c *Channel)handler()http.Handler{
	mux:=http.NewServeMux();mux.HandleFunc(c.cfg.Path,c.handleActivity);return mux
}
func(c *Channel)handleActivity(w http.ResponseWriter,r *http.Request){
	if r.Method!=http.MethodPost{w.Header().Set("Allow","POST");http.Error(w,"Method not allowed",http.StatusMethodNotAllowed);return}
	raw,err:=io.ReadAll(http.MaxBytesReader(w,r.Body,maxPayloadBytes));if err!=nil{http.Error(w,"Invalid payload",http.StatusBadRequest);return}
	var activity map[string]any;if err:=json.Unmarshal(raw,&activity);err!=nil{http.Error(w,"Invalid JSON",http.StatusBadRequest);return}
	if c.cfg.ValidateInboundAuth {
		if err:=c.validateInboundToken(r.Context(),r.Header.Get("Authorization"),activity);err!=nil{c.Logger().Warn("Teams inbound auth failed","error",err);http.Error(w,"Unauthorized",http.StatusUnauthorized);return}
	}
	if err:=c.processActivity(r.Context(),activity);err!=nil{c.Logger().Warn("Teams activity dispatch failed","error",err);http.Error(w,"Temporary dispatch failure",http.StatusServiceUnavailable);return}
	w.Header().Set("Content-Type","application/json");w.WriteHeader(http.StatusOK);_,_=io.WriteString(w,"{}")
}

func(c *Channel)processActivity(ctx context.Context,a map[string]any)error{
	if str(a["type"])!="message"{return nil}
	conv:=obj(a["conversation"]);from:=obj(a["from"]);recipient:=obj(a["recipient"])
	sender:=str(from["aadObjectId"]);if sender==""{sender=str(from["id"])}
	conversationID:=str(conv["id"]);serviceURL:=str(a["serviceUrl"]);activityID:=str(a["id"])
	convType:=str(conv["conversationType"])
	if sender==""||conversationID==""||serviceURL==""{return nil}
	if !c.trustedServiceURL(serviceURL){return nil}
	if rid:=str(recipient["id"]);rid!=""&&rid==str(from["id"]){return nil}
	if convType!=""&&convType!="personal"{return nil}
	text:=sanitizeText(str(a["text"]))
	if text==""{text=strings.TrimSpace(c.cfg.MentionOnlyResponse)}
	if text==""||!c.IsAllowed(sender){return nil}
	c.mu.Lock();c.refs[conversationID]=conversationRef{ServiceURL:serviceURL,ConversationID:conversationID,ActivityID:activityID};c.mu.Unlock()
	return c.HandleMessage(ctx,channels.InboundRequest{SenderID:sender,ChatID:conversationID,Content:text,Metadata:map[string]any{"activity_id":activityID,"conversation_type":"personal"},IsDM:true})
}

var mentionRE=regexp.MustCompile(`(?is)<at\b[^>]*>.*?</at>`)
var tagRE=regexp.MustCompile(`(?s)<[^>]+>`)
func sanitizeText(v string)string{
	v=mentionRE.ReplaceAllString(v," ");v=strings.ReplaceAll(v,"<br>","\n");v=strings.ReplaceAll(v,"<br/>","\n");v=strings.ReplaceAll(v,"<br />","\n");v=tagRE.ReplaceAllString(v," ");v=html.UnescapeString(v)
	return strings.Join(strings.Fields(v)," ")
}

func(c *Channel)trustedServiceURL(raw string)bool{
	u,err:=url.Parse(strings.TrimSpace(raw));if err!=nil||strings.ToLower(u.Scheme)!="https"||u.Hostname()==""||u.User!=nil{return false}
	host:=strings.ToLower(strings.TrimSuffix(u.Hostname(),"."))
	for _,pattern:=range c.cfg.TrustedServiceURLHosts{
		p:=strings.ToLower(strings.TrimSuffix(strings.TrimSpace(pattern),"."))
		if strings.HasPrefix(p,"*."){suffix:=strings.TrimPrefix(p,"*");if strings.HasSuffix(host,suffix)&&host!=strings.TrimPrefix(suffix,"."){return true}}else if host==p{return true}
	}
	return false
}

func(c *Channel)accessToken(ctx context.Context)(string,error){
	c.mu.RLock();if c.token!=""&&time.Until(c.tokenExpires)>time.Minute{t:=c.token;c.mu.RUnlock();return t,nil};c.mu.RUnlock()
	tenant:=c.cfg.TenantID;if tenant==""{tenant="botframework.com"}
	form:=url.Values{"grant_type":{"client_credentials"},"client_id":{c.cfg.AppID},"client_secret":{c.cfg.AppPassword},"scope":{"https://api.botframework.com/.default"}}
	req,err:=http.NewRequestWithContext(ctx,http.MethodPost,"https://login.microsoftonline.com/"+url.PathEscape(tenant)+"/oauth2/v2.0/token",strings.NewReader(form.Encode()));if err!=nil{return "",err}
	req.Header.Set("Content-Type","application/x-www-form-urlencoded")
	resp,err:=c.client.Do(req);if err!=nil{return "",err};defer resp.Body.Close()
	data,err:=io.ReadAll(io.LimitReader(resp.Body,maxPayloadBytes+1));if err!=nil{return "",err}
	if resp.StatusCode<200||resp.StatusCode>=300{return "",fmt.Errorf("msteams: OAuth HTTP %d: %s",resp.StatusCode,strings.TrimSpace(string(data)))}
	var p struct{AccessToken string `json:"access_token"`;ExpiresIn int `json:"expires_in"`};if err:=json.Unmarshal(data,&p);err!=nil{return "",err};if p.AccessToken==""{return "",errors.New("msteams: OAuth returned no access_token")};if p.ExpiresIn<=0{p.ExpiresIn=3600}
	c.mu.Lock();c.token=p.AccessToken;c.tokenExpires=time.Now().Add(time.Duration(p.ExpiresIn)*time.Second);c.mu.Unlock();return p.AccessToken,nil
}

func(c *Channel)validateInboundToken(ctx context.Context,auth string,activity map[string]any)error{
	if !strings.HasPrefix(strings.ToLower(auth),"bearer "){return errors.New("missing bearer token")}
	token:=strings.TrimSpace(auth[len("Bearer "):]);parts:=strings.Split(token,".");if len(parts)!=3{return errors.New("malformed JWT")}
	headerBytes,err:=base64.RawURLEncoding.DecodeString(parts[0]);if err!=nil{return err}
	payloadBytes,err:=base64.RawURLEncoding.DecodeString(parts[1]);if err!=nil{return err}
	sig,err:=base64.RawURLEncoding.DecodeString(parts[2]);if err!=nil{return err}
	var header map[string]any;var claims map[string]any
	if json.Unmarshal(headerBytes,&header)!=nil||json.Unmarshal(payloadBytes,&claims)!=nil{return errors.New("invalid JWT JSON")}
	if str(header["alg"])!="RS256"{return errors.New("unsupported JWT algorithm")}
	kid:=str(header["kid"]);if kid==""{return errors.New("missing JWT kid")}
	keys,err:=c.botFrameworkKeys(ctx);if err!=nil{return err};key:=keys[kid];if key==nil{return fmt.Errorf("unknown JWT kid %s",kid)}
	digest:=sha256.Sum256([]byte(parts[0]+"."+parts[1]))
	if err:=rsa.VerifyPKCS1v15(key,crypto.SHA256,digest[:],sig);err!=nil{return errors.New("invalid JWT signature")}
	now:=time.Now().Unix()
	exp,ok:=numberClaim(claims["exp"]);if !ok||exp<=now{return errors.New("expired JWT")}
	nbf,ok:=numberClaim(claims["nbf"]);if !ok||nbf>now+60{return errors.New("JWT not active")}
	if str(claims["iss"])!="https://api.botframework.com"{return errors.New("invalid JWT issuer")}
	if !audienceContains(claims["aud"],c.cfg.AppID){return errors.New("invalid JWT audience")}
	claimURL:=str(claims["serviceurl"]);if claimURL==""{claimURL=str(claims["serviceUrl"])}
	activityURL:=str(activity["serviceUrl"]);if claimURL!=""&&activityURL!=""&&claimURL!=activityURL{return errors.New("serviceUrl claim mismatch")}
	return nil
}

func(c *Channel)botFrameworkKeys(ctx context.Context)(map[string]*rsa.PublicKey,error){
	c.mu.RLock();if len(c.jwks)>0&&time.Now().Before(c.jwksExpires){out:=c.jwks;c.mu.RUnlock();return out,nil};c.mu.RUnlock()
	var open struct{JWKSURI string `json:"jwks_uri"`}
	if err:=c.getJSON(ctx,"https://login.botframework.com/v1/.well-known/openidconfiguration",&open);err!=nil{return nil,err}
	if open.JWKSURI==""{return nil,errors.New("msteams: OpenID config missing jwks_uri")}
	u,err:=url.Parse(open.JWKSURI);if err!=nil||u.Scheme!="https"||u.Host==""{return nil,errors.New("msteams: invalid jwks_uri")}
	var set struct{Keys []struct{Kid string `json:"kid"`;Kty string `json:"kty"`;N string `json:"n"`;E string `json:"e"`} `json:"keys"`}
	if err:=c.getJSON(ctx,open.JWKSURI,&set);err!=nil{return nil,err}
	out:=make(map[string]*rsa.PublicKey)
	for _,j:=range set.Keys{if j.Kid==""||j.Kty!="RSA"{continue};nBytes,eBytesErr:=base64.RawURLEncoding.DecodeString(j.N);if eBytesErr!=nil{continue};eBytes,eErr:=base64.RawURLEncoding.DecodeString(j.E);if eErr!=nil{continue};e:=0;for _,b:=range eBytes{e=e<<8|int(b)};if e<3{continue};out[j.Kid]=&rsa.PublicKey{N:new(big.Int).SetBytes(nBytes),E:e}}
	if len(out)==0{return nil,errors.New("msteams: JWKS contained no usable RSA keys")}
	c.mu.Lock();c.jwks=out;c.jwksExpires=time.Now().Add(time.Hour);c.mu.Unlock();return out,nil
}
func(c *Channel)getJSON(ctx context.Context,endpoint string,out any)error{req,err:=http.NewRequestWithContext(ctx,http.MethodGet,endpoint,nil);if err!=nil{return err};resp,err:=c.client.Do(req);if err!=nil{return err};defer resp.Body.Close();data,err:=io.ReadAll(io.LimitReader(resp.Body,maxPayloadBytes+1));if err!=nil{return err};if resp.StatusCode<200||resp.StatusCode>=300{return fmt.Errorf("msteams: HTTP %d from metadata endpoint",resp.StatusCode)};return json.Unmarshal(data,out)}

func obj(v any)map[string]any{m,_:=v.(map[string]any);return m}
func str(v any)string{if s,ok:=v.(string);ok{return strings.TrimSpace(s)};return ""}
func numberClaim(v any)(int64,bool){switch n:=v.(type){case float64:return int64(n),true;case json.Number:x,e:=n.Int64();return x,e==nil;case string:x,e:=strconv.ParseInt(n,10,64);return x,e==nil};return 0,false}
func audienceContains(v any,want string)bool{switch a:=v.(type){case string:return a==want;case []any:for _,x:=range a{if str(x)==want{return true}}};return false}
