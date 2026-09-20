package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
)

func TestSanitizeToolName(t *testing.T) {
	cases:=map[string]string{
		SanitizeToolName("my server","read/file"):"mcp_my_server_read_file",
		SanitizeToolName("a...b","x y"):"mcp_a_b_x_y",
	}
	for got,want:=range cases{if got!=want{t.Fatalf("got %q want %q",got,want)}}
	long:=SanitizeToolName(strings.Repeat("s",80),strings.Repeat("x",80)); if len(long)!=64{t.Fatalf("len=%d",len(long))}
}

func TestStdioLifecycleListAndCall(t *testing.T) {
	if os.Getenv("HAOSBOT_MCP_HELPER")=="1" { runHelper(); return }
	cfg:=config.MCPServerConfig{Command:os.Args[0],Args:[]string{"-test.run=TestStdioLifecycleListAndCall"},Env:map[string]string{"HAOSBOT_MCP_HELPER":"1"},ToolTimeout:2}
	c:=NewClient("test",cfg); defer c.Close(); ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second); defer cancel()
	list,err:=c.ListTools(ctx); if err!=nil{t.Fatal(err)}; if len(list)!=1||list[0].Name!="echo"{t.Fatalf("tools=%+v",list)}
	res,err:=c.Call(ctx,"echo",json.RawMessage(`{"value":"ok"}`)); if err!=nil{t.Fatal(err)}; if res.Content!="ok"||res.IsError{t.Fatalf("result=%+v",res)}
}

func runHelper() {
	s:=bufio.NewScanner(os.Stdin); w:=bufio.NewWriter(os.Stdout); defer w.Flush()
	for s.Scan(){
		var req struct{ID json.RawMessage `json:"id"`; Method string `json:"method"`; Params map[string]json.RawMessage `json:"params"`}; if json.Unmarshal(s.Bytes(),&req)!=nil{continue}; if len(req.ID)==0{continue}
		var result any
		switch req.Method {
		case "initialize": result=map[string]any{"protocolVersion":protocolVersion,"capabilities":map[string]any{}}
		case "tools/list":
			// A malformed progress notification before the real response must be ignored.
			fmt.Fprintln(w,`{"jsonrpc":"2.0","method":"notifications/progress","params":"bad"}`); w.Flush()
			result=map[string]any{"tools":[]any{map[string]any{"name":"echo","description":"echo","inputSchema":map[string]any{"type":"object"}}}}
		case "tools/call":
			var p struct{Arguments map[string]any `json:"arguments"`}; _=json.Unmarshal(must(req.Params["arguments"],req.Params),&p)
			// Decode the complete params because the compact helper above deliberately
			// avoids depending on protocol structs used by production.
			var all map[string]any; raw,_:=json.Marshal(req.Params); _=json.Unmarshal(raw,&all); args,_:=all["arguments"].(map[string]any); value,_:=args["value"].(string)
			result=map[string]any{"content":[]any{map[string]any{"type":"text","text":value}},"isError":false}
		default: result=map[string]any{}
		}
		id:=string(req.ID); body,_:=json.Marshal(result); fmt.Fprintf(w,"{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":%s}\n",id,body); w.Flush()
	}
	os.Exit(0)
}

func must(v json.RawMessage, _ map[string]json.RawMessage) json.RawMessage { return v }
