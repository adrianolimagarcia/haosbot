// Package mcp implements the Model Context Protocol bridge used by HAOSBOT.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

const protocolVersion = "2025-06-18"

var errTransport = errors.New("mcp: transport closed")

type rpcError struct { Code int `json:"code"`; Message string `json:"message"` }
type response struct { JSONRPC string `json:"jsonrpc"`; ID json.RawMessage `json:"id"`; Result json.RawMessage `json:"result"`; Error *rpcError `json:"error"` }
type request struct { JSONRPC string `json:"jsonrpc"`; ID uint64 `json:"id,omitempty"`; Method string `json:"method"`; Params any `json:"params,omitempty"` }

type ToolInfo struct { Name string `json:"name"`; Description string `json:"description"`; InputSchema json.RawMessage `json:"inputSchema"` }
type listResult struct { Tools []ToolInfo `json:"tools"`; NextCursor string `json:"nextCursor,omitempty"` }
type callResult struct { Content []struct { Type string `json:"type"`; Text string `json:"text"` } `json:"content"`; IsError bool `json:"isError"` }

type Client struct {
	name string
	cfg config.MCPServerConfig
	mu sync.Mutex
	writeMu sync.Mutex
	cmd *exec.Cmd
	stdin io.WriteCloser
	pending map[uint64]chan response
	next atomic.Uint64
	closed bool
	generation uint64
}

func NewClient(name string, cfg config.MCPServerConfig) *Client { return &Client{name:name, cfg:cfg, pending:make(map[uint64]chan response)} }

func normalizeCommand(command string, args []string) (string, []string) {
	command = strings.TrimSpace(command)
	if runtime.GOOS != "windows" { return command, args }
	lower := strings.ToLower(command)
	if strings.HasSuffix(lower, ".cmd") || strings.HasSuffix(lower, ".bat") {
		return "cmd.exe", append([]string{"/d", "/s", "/c", command}, args...)
	}
	return command, args
}

func (c *Client) start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed { return errors.New("mcp: client closed") }
	if c.cmd != nil { return nil }
	command, args := normalizeCommand(c.cfg.Command, append([]string(nil), c.cfg.Args...))
	if command == "" { return errors.New("mcp: stdio server command is empty") }
	cmd := exec.CommandContext(context.Background(), command, args...)
	if c.cfg.CWD != "" { cmd.Dir = c.cfg.CWD }
	cmd.Env = os.Environ()
	keys := make([]string, 0, len(c.cfg.Env)); for k := range c.cfg.Env { keys=append(keys,k) }; sort.Strings(keys)
	for _, k := range keys { cmd.Env = append(cmd.Env, k+"="+c.cfg.Env[k]) }
	stdin, err := cmd.StdinPipe(); if err != nil { return err }
	stdout, err := cmd.StdoutPipe(); if err != nil { _=stdin.Close(); return err }
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil { _=stdin.Close(); return err }
	c.cmd, c.stdin = cmd, stdin
	c.generation++
	gen := c.generation
	go c.readLoop(stdout, gen)
	go c.waitLoop(cmd, gen)
	return nil
}

func (c *Client) waitLoop(cmd *exec.Cmd, gen uint64) { err:=cmd.Wait(); c.failGeneration(gen, fmt.Errorf("%w: %v", errTransport, err)) }

func (c *Client) failGeneration(gen uint64, _ error) {
	c.mu.Lock(); defer c.mu.Unlock()
	if gen != c.generation { return }
	c.cmd=nil; c.stdin=nil
	for id,ch := range c.pending { delete(c.pending,id); close(ch) }
}

func (c *Client) readLoop(r io.Reader, gen uint64) {
	s := bufio.NewScanner(r); s.Buffer(make([]byte,4096), 4<<20)
	for s.Scan() {
		line:=s.Bytes(); var envelope struct { ID json.RawMessage `json:"id"`; Method string `json:"method"` }
		if json.Unmarshal(line,&envelope)!=nil { continue }
		// Notifications (including malformed progress notifications) are intentionally
		// ignored. They must never poison the request/response stream.
		if len(envelope.ID)==0 || string(envelope.ID)=="null" { continue }
		var resp response; if json.Unmarshal(line,&resp)!=nil { continue }
		var id uint64; if json.Unmarshal(resp.ID,&id)!=nil { continue }
		c.mu.Lock(); ch:=c.pending[id]; if ch!=nil { delete(c.pending,id) }; c.mu.Unlock()
		if ch!=nil { ch<-resp; close(ch) }
	}
	c.failGeneration(gen, errTransport)
}

func (c *Client) rpc(ctx context.Context, method string, params any) (json.RawMessage,error) {
	if err:=c.start(ctx); err!=nil { return nil,err }
	id:=c.next.Add(1); ch:=make(chan response,1)
	c.mu.Lock(); if c.cmd==nil { c.mu.Unlock(); return nil,errTransport }; c.pending[id]=ch; stdin:=c.stdin; c.mu.Unlock()
	b,err:=json.Marshal(request{JSONRPC:"2.0",ID:id,Method:method,Params:params}); if err!=nil{return nil,err}; b=append(b,'\n')
	c.writeMu.Lock(); _,err=stdin.Write(b); c.writeMu.Unlock()
	if err!=nil { c.mu.Lock(); delete(c.pending,id); c.mu.Unlock(); return nil,fmt.Errorf("%w: %v",errTransport,err) }
	select {
	case <-ctx.Done(): c.mu.Lock(); delete(c.pending,id); c.mu.Unlock(); return nil,ctx.Err()
	case resp,ok:=<-ch:
		if !ok { return nil,errTransport }
		if resp.Error!=nil { return nil,fmt.Errorf("mcp rpc %s: %d %s",method,resp.Error.Code,resp.Error.Message) }
		return resp.Result,nil
	}
}

func (c *Client) initialize(ctx context.Context) error {
	_,err:=c.rpc(ctx,"initialize",map[string]any{"protocolVersion":protocolVersion,"capabilities":map[string]any{},"clientInfo":map[string]any{"name":"haosbot","version":"1"}}); if err!=nil{return err}
	// initialized is a notification: no id and no response expected.
	b,_:=json.Marshal(map[string]any{"jsonrpc":"2.0","method":"notifications/initialized"}); b=append(b,'\n')
	c.mu.Lock(); stdin:=c.stdin; c.mu.Unlock(); if stdin==nil{return errTransport}; c.writeMu.Lock(); _,err=stdin.Write(b); c.writeMu.Unlock(); return err
}

func (c *Client) withReconnect(ctx context.Context, fn func(context.Context)(json.RawMessage,error)) (json.RawMessage,error) {
	var last error
	for attempt:=0; attempt<2; attempt++ {
		if attempt>0 { c.reset(); select { case <-ctx.Done(): return nil,ctx.Err(); case <-time.After(50*time.Millisecond): } }
		if err:=c.initialize(ctx); err!=nil { last=err; if !errors.Is(err,errTransport){return nil,err}; continue }
		v,err:=fn(ctx); if err==nil{return v,nil}; last=err; if !errors.Is(err,errTransport){return nil,err}
	}
	return nil,last
}

func (c *Client) reset() { c.mu.Lock(); cmd:=c.cmd; stdin:=c.stdin; c.cmd=nil; c.stdin=nil; c.generation++; for id,ch:=range c.pending{delete(c.pending,id);close(ch)}; c.mu.Unlock(); if stdin!=nil{_=stdin.Close()}; if cmd!=nil && cmd.Process!=nil{_=cmd.Process.Kill()} }

func (c *Client) ListTools(ctx context.Context) ([]ToolInfo,error) {
	var out listResult
	raw,err:=c.withReconnect(ctx,func(ctx context.Context)(json.RawMessage,error){return c.rpc(ctx,"tools/list",map[string]any{})}); if err!=nil{return nil,err}
	if err=json.Unmarshal(raw,&out);err!=nil{return nil,err}; return out.Tools,nil
}

func (c *Client) Call(ctx context.Context, name string, args json.RawMessage) (tools.Result,error) {
	var obj map[string]any; if len(args)>0 { if err:=json.Unmarshal(args,&obj);err!=nil{return tools.Result{},err} }; if obj==nil{obj=map[string]any{}}
	timeout:=time.Duration(c.cfg.ToolTimeout)*time.Second; if timeout<=0{timeout=60*time.Second}; callCtx,cancel:=context.WithTimeout(ctx,timeout); defer cancel()
	raw,err:=c.withReconnect(callCtx,func(ctx context.Context)(json.RawMessage,error){return c.rpc(ctx,"tools/call",map[string]any{"name":name,"arguments":obj})}); if err!=nil{return tools.Result{},err}
	var out callResult; if err=json.Unmarshal(raw,&out);err!=nil{return tools.Result{},err}; parts:=make([]string,0,len(out.Content)); for _,p:=range out.Content{if p.Text!=""{parts=append(parts,p.Text)}}; return tools.Result{Content:strings.Join(parts,"\n"),IsError:out.IsError},nil
}

func (c *Client) Close() error { c.mu.Lock(); if c.closed{c.mu.Unlock();return nil}; c.closed=true; c.mu.Unlock(); c.reset(); return nil }
