package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

type remoteTool struct { tools.Base; client *Client; remoteName, localName, description string; schema json.RawMessage }
func (t *remoteTool) Name() string{return t.localName}
func (t *remoteTool) Description() string{return t.description}
func (t *remoteTool) Parameters() json.RawMessage{return t.schema}
func (t *remoteTool) Execute(ctx context.Context,args json.RawMessage)(tools.Result,error){return t.client.Call(ctx,t.remoteName,args)}

// SanitizeToolName produces a deterministic provider-safe identifier. The
// server prefix prevents collisions with builtins and mirrors HAOSBOT's mcp_
// schema partitioning. Invalid runs collapse to one underscore.
func SanitizeToolName(server, name string) string {
	raw:="mcp_"+server+"_"+name; var b strings.Builder; underscore:=false
	for _,r:=range raw {
		ok:=unicode.IsLetter(r)||unicode.IsDigit(r)||r=='_'||r=='-'
		if ok { b.WriteRune(r); underscore=false } else if !underscore { b.WriteByte('_'); underscore=true }
	}
	out:=strings.Trim(b.String(),"_"); if out==""||out=="mcp"{return "mcp_tool"}; if len(out)>64{out=out[:64]}; return out
}

type Manager struct { clients []*Client }

// RegisterConfigured connects enabled stdio MCP servers and registers their
// advertised tools in the existing HAOSBOT registry. Registration order is
// deterministic regardless of map iteration order.
func RegisterConfigured(ctx context.Context, registry *tools.Registry, servers map[string]config.MCPServerConfig) (*Manager,error) {
	m:=&Manager{}; names:=make([]string,0,len(servers)); for n:=range servers{names=append(names,n)}; sort.Strings(names)
	for _,server:=range names {
		cfg:=servers[server]; typ:="stdio"; if cfg.Type!=nil{typ=strings.ToLower(strings.TrimSpace(*cfg.Type))}; if typ!=""&&typ!="stdio"{continue}; if strings.TrimSpace(cfg.Command)==""{continue}
		client:=NewClient(server,cfg); infos,err:=client.ListTools(ctx); if err!=nil{m.Close();return nil,fmt.Errorf("mcp server %q: %w",server,err)}
		allowed:=map[string]bool{}; for _,n:=range cfg.EnabledTools{allowed[n]=true}; sort.Slice(infos,func(i,j int)bool{return infos[i].Name<infos[j].Name})
		seen:=map[string]int{}
		for _,info:=range infos { if len(allowed)>0&&!allowed[info.Name]{continue}; local:=SanitizeToolName(server,info.Name); seen[local]++; if seen[local]>1{local=fmt.Sprintf("%s_%d",local,seen[local])}; schema:=info.InputSchema; if len(schema)==0{schema=json.RawMessage(`{"type":"object","properties":{}}`)}; registry.Register(&remoteTool{client:client,remoteName:info.Name,localName:local,description:info.Description,schema:schema}) }
		m.clients=append(m.clients,client)
	}
	return m,nil
}

func (m *Manager) Close(){if m==nil{return}; for _,c:=range m.clients{_=c.Close()}}
