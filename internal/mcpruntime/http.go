package mcpruntime

import (
 "bytes"
 "context"
 "crypto/sha1"
 "encoding/hex"
 "encoding/json"
 "errors"
 "fmt"
 "io"
 "net/http"
 "regexp"
 "sort"
 "strings"
 "sync"
 "time"

 "github.com/adrianolimagarcia/nanobot-go/internal/config"
 "github.com/adrianolimagarcia/nanobot-go/internal/netpolicy"
 "github.com/adrianolimagarcia/nanobot-go/internal/tools"
)
type Options struct{SSRFWhitelist []string}
type Manager struct{opt Options;mu sync.Mutex;clients []*httpClient}
func NewManager(opt Options)*Manager{return &Manager{opt:opt}}
func(m *Manager)Close()error{m.mu.Lock();for _,c:=range m.clients{c.Close()};m.clients=nil;m.mu.Unlock();return nil}
type toolDef struct{Name string `json:"name"`;Description string `json:"description"`;InputSchema json.RawMessage `json:"inputSchema"`}
type loadResult struct{name string;client *httpClient;defs []toolDef;err error}
func(m *Manager)LoadAndRegister(ctx context.Context,reg *tools.Registry,servers map[string]config.MCPServerConfig)error{
 if len(servers)==0{return nil};names:=make([]string,0,len(servers));for name:=range servers{names=append(names,name)};sort.Strings(names)
 ch:=make(chan loadResult,len(names));var wg sync.WaitGroup
 for _,name:=range names{name:=name;cfg:=servers[name];wg.Add(1);go func(){defer wg.Done();if strings.TrimSpace(cfg.URL)==""{if strings.TrimSpace(cfg.Command)!=""{ch<-loadResult{name:name,err:errors.New("stdio MCP transport is not enabled in this HTTP runtime")};return};ch<-loadResult{name:name,err:errors.New("missing MCP URL")};return};client:=newHTTPClient(name,cfg,m.opt);defs,err:=client.initializeAndList(ctx);if err!=nil{client.Close()};ch<-loadResult{name:name,client:client,defs:defs,err:err}}()}
 wg.Wait();close(ch);var failures []string
 for result:=range ch{if result.err!=nil{failures=append(failures,result.name+": "+result.err.Error());continue};m.mu.Lock();m.clients=append(m.clients,result.client);m.mu.Unlock();enabled:=map[string]bool{};for _,n:=range result.client.cfg.EnabledTools{enabled[n]=true};for _,d:=range result.defs{if len(enabled)>0&&!enabled[d.Name]{continue};reg.Register(&dynamicTool{client:result.client,server:result.name,remote:d.Name,name:toolName(result.name,d.Name),desc:d.Description,schema:d.InputSchema,timeout:toolTimeout(result.client.cfg.ToolTimeout)})}}
 if len(failures)>0{sort.Strings(failures);return errors.New(strings.Join(failures,"; "))};return nil
}
type dynamicTool struct{tools.Base;client *httpClient;server,remote,name,desc string;schema json.RawMessage;timeout time.Duration}
func(t *dynamicTool)Name()string{return t.name}
func(t *dynamicTool)Description()string{if strings.TrimSpace(t.desc)!=""{return t.desc};return"MCP tool "+t.remote+" from "+t.server}
func(t *dynamicTool)Parameters()json.RawMessage{if len(t.schema)==0{return json.RawMessage(`{"type":"object","properties":{}}`)};return t.schema}
func(t *dynamicTool)Execute(ctx context.Context,args json.RawMessage)(tools.Result,error){var params map[string]any;if len(args)>0{if err:=json.Unmarshal(args,&params);err!=nil{return tools.Errf("Error: %v",err),nil}};run,cancel:=context.WithTimeout(ctx,t.timeout);defer cancel();raw,err:=t.client.request(run,"tools/call",map[string]any{"name":t.remote,"arguments":params});if err!=nil{return tools.Errf("Error: %v",err),nil};var out struct{Content []struct{Type string `json:"type"`;Text string `json:"text"`} `json:"content"`;IsError bool `json:"isError"`};if err:=json.Unmarshal(raw,&out);err!=nil{return tools.Errf("Error: invalid MCP result: %v",err),nil};parts:=[]string{};for _,c:=range out.Content{if c.Text!=""{parts=append(parts,c.Text)}};text:=strings.Join(parts,"\n");if out.IsError{return tools.Errf("%s",text),nil};return tools.OK(text),nil}
type httpClient struct{name string;cfg config.MCPServerConfig;opt Options;mu sync.Mutex;next int64;session string;client *http.Client}
func newHTTPClient(name string,cfg config.MCPServerConfig,opt Options)*httpClient{
	policy:=netpolicy.Policy{Allowlist:opt.SSRFWhitelist}
	return &httpClient{name:name,cfg:cfg,opt:opt,client:netpolicy.NewClient(90*time.Second,policy)}
}
func(c *httpClient)Close(){if c.client!=nil{c.client.CloseIdleConnections()}}
func(c *httpClient)initializeAndList(ctx context.Context)([]toolDef,error){run,cancel:=context.WithTimeout(ctx,12*time.Second);defer cancel();if _,err:=c.request(run,"initialize",map[string]any{"protocolVersion":"2025-06-18","capabilities":map[string]any{},"clientInfo":map[string]any{"name":"haosbot","version":"1"}});err!=nil{return nil,err};// A failed notifications/initialized must be reported as itself: swallowing it
// makes the tools/list failure that follows look like the server's fault.
	if err:=c.notify(run,"notifications/initialized",map[string]any{});err!=nil{return nil,err};raw,err:=c.request(run,"tools/list",map[string]any{});if err!=nil{return nil,err};var list struct{Tools []toolDef `json:"tools"`};if err:=json.Unmarshal(raw,&list);err!=nil{return nil,err};return list.Tools,nil}
func(c *httpClient)request(ctx context.Context,method string,params any)(json.RawMessage,error){c.mu.Lock();defer c.mu.Unlock();c.next++;return c.exchange(ctx,map[string]any{"jsonrpc":"2.0","id":c.next,"method":method,"params":params})}
func(c *httpClient)notify(ctx context.Context,method string,params any)error{c.mu.Lock();defer c.mu.Unlock();_,err:=c.exchange(ctx,map[string]any{"jsonrpc":"2.0","method":method,"params":params});return err}
func(c *httpClient)exchange(ctx context.Context,payload any)(json.RawMessage,error){
	policy:=netpolicy.Policy{Allowlist:c.opt.SSRFWhitelist};if _,err:=netpolicy.ValidateURL(ctx,c.cfg.URL,policy);err!=nil{return nil,err};raw,_:=json.Marshal(payload);req,err:=http.NewRequestWithContext(ctx,http.MethodPost,c.cfg.URL,bytes.NewReader(raw));if err!=nil{return nil,err};req.Header.Set("Content-Type","application/json");req.Header.Set("Accept","application/json, text/event-stream");for k,v:=range c.cfg.Headers{req.Header.Set(k,v)};if c.session!=""{req.Header.Set("MCP-Session-Id",c.session)}
	resp,err:=c.client.Do(req);if err!=nil{return nil,err};defer resp.Body.Close();if sid:=strings.TrimSpace(resp.Header.Get("MCP-Session-Id"));sid!=""{c.session=sid};body,err:=io.ReadAll(io.LimitReader(resp.Body,8<<20));if err!=nil{return nil,err};if resp.StatusCode==http.StatusAccepted&&len(bytes.TrimSpace(body))==0{return json.RawMessage(`{}`),nil};if resp.StatusCode<200||resp.StatusCode>299{return nil,fmt.Errorf("MCP HTTP %d: %s",resp.StatusCode,strings.TrimSpace(string(body)))}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")),"text/event-stream"){var chosen []byte;for _,line:=range strings.Split(string(body),"\n"){line=strings.TrimSpace(line);if strings.HasPrefix(line,"data:"){candidate:=[]byte(strings.TrimSpace(strings.TrimPrefix(line,"data:")));var env struct{Result json.RawMessage `json:"result"`;Error any `json:"error"`};if json.Unmarshal(candidate,&env)==nil&&(len(env.Result)>0||env.Error!=nil){chosen=candidate;break};if len(chosen)==0{chosen=candidate}}};if len(chosen)>0{body=chosen}};if len(bytes.TrimSpace(body))==0{return json.RawMessage(`{}`),nil};var env struct{Result json.RawMessage `json:"result"`;Error *struct{Code int `json:"code"`;Message string `json:"message"`} `json:"error"`};if err:=json.Unmarshal(body,&env);err!=nil{return nil,err};if env.Error!=nil{return nil,fmt.Errorf("MCP %d: %s",env.Error.Code,env.Error.Message)};return env.Result,nil
}
func toolTimeout(seconds int)time.Duration{if seconds<=0{seconds=60};if seconds>600{seconds=600};return time.Duration(seconds)*time.Second}
var nameRE=regexp.MustCompile(`[^A-Za-z0-9_-]+`)
func toolName(server,remote string)string{base:="mcp_"+nameRE.ReplaceAllString(server,"_")+"_"+nameRE.ReplaceAllString(remote,"_");if len(base)<=64{return base};sum:=sha1.Sum([]byte(base));return base[:55]+"_"+hex.EncodeToString(sum[:4])}
