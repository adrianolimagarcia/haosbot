package cliapps

import (
 "context"
 "encoding/json"
 "errors"
 "fmt"
 "os"
 "path/filepath"
 "regexp"
 "strings"
 "time"

 "github.com/adrianolimagarcia/nanobot-go/internal/tools"
)
type Options struct{Workspace string;RunTimeout time.Duration;ExecTool tools.Tool}
type Manifest struct{Name string `json:"name"`;Description string `json:"description"`;EntryPoint string `json:"entry_point"`;Args []string `json:"args"`;WorkingDir string `json:"working_dir"`}
type Tool struct{tools.Base;opt Options}
func New(opt Options)*Tool{if opt.RunTimeout<=0{opt.RunTimeout=60*time.Second};return &Tool{opt:opt}}
func(t *Tool)Name()string{return"run_cli_app"}
func(t *Tool)Description()string{return"Run a CLI App explicitly installed in the workspace registry. Unknown names are rejected and execution reuses HAOSBOT's guarded exec capability."}
func(t *Tool)Parameters()json.RawMessage{return json.RawMessage(`{"type":"object","required":["name"],"properties":{"name":{"type":"string"},"args":{"type":"array","items":{"type":"string"}},"json":{"type":"boolean"},"working_dir":{"type":"string"},"timeout":{"type":"integer","minimum":1,"maximum":600}}}`)}
var nameRE=regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
func(t *Tool)Execute(ctx context.Context,raw json.RawMessage)(tools.Result,error){
 if t.opt.ExecTool==nil{return tools.Errf("Error: guarded exec capability is unavailable"),nil}
 var in struct{Name string `json:"name"`;Args []string `json:"args"`;JSON bool `json:"json"`;WorkingDir string `json:"working_dir"`;Timeout int `json:"timeout"`}
 if err:=json.Unmarshal(raw,&in);err!=nil{return tools.Errf("Error: %v",err),nil}
 if !nameRE.MatchString(in.Name){return tools.Errf("Error: invalid CLI app name"),nil}
 m,err:=t.load(in.Name);if err!=nil{return tools.Errf("Error: %v",err),nil}
 argv:=append([]string(nil),m.Args...);if in.JSON{argv=append(argv,"--json")};argv=append(argv,in.Args...)
 parts:=[]string{quote(m.EntryPoint)};for _,a:=range argv{parts=append(parts,quote(a))}
 wd:=m.WorkingDir;if in.WorkingDir!=""{wd=in.WorkingDir};if wd!=""&&!filepath.IsAbs(wd){wd=filepath.Join(t.opt.Workspace,wd)}
 timeout:=int(t.opt.RunTimeout/time.Second);if timeout<=0{timeout=60};if in.Timeout>0{timeout=in.Timeout};if timeout>600{timeout=600}
 args:=map[string]any{"command":strings.Join(parts," "),"timeout":timeout};if wd!=""{args["working_dir"]=wd};encoded,_:=json.Marshal(args);return t.opt.ExecTool.Execute(ctx,encoded)
}
func(t *Tool)load(name string)(Manifest,error){raw,err:=os.ReadFile(filepath.Join(t.opt.Workspace,".haosbot","cli-apps",name+".json"));if err!=nil{return Manifest{},fmt.Errorf("CLI app %q is not installed",name)};var m Manifest;if err:=json.Unmarshal(raw,&m);err!=nil{return Manifest{},err};if m.Name!=""&&!strings.EqualFold(m.Name,name){return Manifest{},errors.New("CLI app manifest identity mismatch")};if strings.TrimSpace(m.EntryPoint)==""{return Manifest{},errors.New("CLI app manifest has no entry_point")};return m,nil}
func quote(v string)string{return "'"+strings.ReplaceAll(v,"'","'\"'\"'")+"'"}
