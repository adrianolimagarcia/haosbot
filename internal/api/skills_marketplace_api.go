package api

import (
 "archive/zip"
 "bytes"
 "context"
 "encoding/base64"
 "encoding/json"
 "errors"
 "fmt"
 "io"
 "net/http"
 "net/url"
 "os"
 "path/filepath"
 "regexp"
 "sort"
 "strings"
 "time"

 "github.com/adrianolimagarcia/nanobot-go/internal/netpolicy"
)

var (
 marketSourceRE=regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?/[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,98}[A-Za-z0-9])?$`)
 marketSkillRE=regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

func(s *Server)handleWebUISkillsMarketplace(w http.ResponseWriter,r *http.Request){
 if r.Method!=http.MethodGet{w.Header().Set("Allow",http.MethodGet);http.Error(w,"Method not allowed",405);return}
 q:=strings.TrimSpace(r.URL.Query().Get("q"));provider:=strings.TrimSpace(r.URL.Query().Get("provider"));if provider==""{provider="skills_sh"}
 var target string
 switch provider{
 case"skills_sh":if q==""{target="https://skills.sh/api/skills/trending/0"}else{target="https://skills.sh/api/search?q="+url.QueryEscape(q)}
 case"skillhub":if q==""{target="https://api.skillhub.cn/api/v1/showcase/trending"}else{target="https://api.skillhub.cn/api/v1/search?q="+url.QueryEscape(q)}
 default:http.Error(w,"unsupported provider",400);return
 }
 raw,status,err:=s.marketGET(r.Context(),target,4<<20);if err!=nil{http.Error(w,err.Error(),status);return}
 var payload any;if err:=json.Unmarshal(raw,&payload);err!=nil{http.Error(w,"invalid marketplace response",502);return}
 writeWebUIJSON(w,map[string]any{"provider":provider,"query":q,"payload":payload,"install_supported":s.cfg.Tools.WebUIAllowRemotePackageInstall})
}

func(s *Server)marketGET(ctx context.Context,target string,limit int64)([]byte,int,error){
 policy:=netpolicy.Policy{Allowlist:append(append([]string(nil),s.cfg.Tools.SSRFWhitelist...),"skills.sh","api.skillhub.cn","api.github.com","github.com","codeload.github.com","raw.githubusercontent.com")}
 if _,err:=netpolicy.ValidateURL(ctx,target,policy);err!=nil{return nil,403,err}
 req,err:=http.NewRequestWithContext(ctx,http.MethodGet,target,nil);if err!=nil{return nil,500,err};req.Header.Set("Accept","application/json");req.Header.Set("User-Agent","HAOSBOT/skills-marketplace")
 resp,err:=netpolicy.NewClient(20*time.Second,policy).Do(req);if err!=nil{return nil,502,err};defer resp.Body.Close()
 raw,err:=io.ReadAll(io.LimitReader(resp.Body,limit+1));if err!=nil{return nil,502,err};if int64(len(raw))>limit{return nil,502,errors.New("remote response exceeded limit")};if resp.StatusCode<200||resp.StatusCode>299{return nil,502,fmt.Errorf("remote HTTP %d",resp.StatusCode)};return raw,200,nil
}

func(s *Server)handleWebUISkillsInstall(w http.ResponseWriter,r *http.Request){
 if r.Method!=http.MethodPost{w.Header().Set("Allow",http.MethodPost);http.Error(w,"Method not allowed",405);return}
 if !s.cfg.Tools.WebUIAllowRemotePackageInstall{http.Error(w,"remote package install is disabled",403);return}
 var in struct{Source string `json:"source"`;Skill string `json:"skill"`}
 if err:=json.NewDecoder(http.MaxBytesReader(w,r.Body,32<<10)).Decode(&in);err!=nil{http.Error(w,"invalid JSON",400);return}
 in.Source=strings.TrimSpace(in.Source);in.Skill=strings.TrimSpace(in.Skill)
 if !marketSourceRE.MatchString(in.Source)||!marketSkillRE.MatchString(in.Skill){http.Error(w,"invalid source or skill id",400);return}
 result,err:=s.installGitHubSkill(r.Context(),in.Source,in.Skill);if err!=nil{http.Error(w,err.Error(),502);return};writeWebUIJSON(w,result)
}

func(s *Server)installGitHubSkill(ctx context.Context,source,skill string)(map[string]any,error){
 workspace:=webUIWorkspace(s.cfg);target:=filepath.Join(workspace,"skills",skill);if _,err:=os.Stat(filepath.Join(target,"SKILL.md"));err==nil{return map[string]any{"installed":true,"already_installed":true,"name":skill,"source":source},nil}
 metaRaw,_,err:=s.marketGET(ctx,"https://api.github.com/repos/"+source,2<<20);if err!=nil{return nil,err};var meta struct{DefaultBranch string `json:"default_branch"`};if json.Unmarshal(metaRaw,&meta)!=nil||meta.DefaultBranch==""{return nil,errors.New("cannot resolve repository default branch")}
 treeRaw,_,err:=s.marketGET(ctx,"https://api.github.com/repos/"+source+"/git/trees/"+url.PathEscape(meta.DefaultBranch)+"?recursive=1",8<<20);if err!=nil{return nil,err}
 var tree struct{Tree []struct{Path,Type,URL string;Size int64 `json:"size"`} `json:"tree"`};if err:=json.Unmarshal(treeRaw,&tree);err!=nil{return nil,err}
 suffix:="/"+skill+"/SKILL.md";root:=""
 candidates:=[]string{};for _,x:=range tree.Tree{if x.Type=="blob"&&(x.Path==skill+"/SKILL.md"||strings.HasSuffix(x.Path,suffix)){candidates=append(candidates,strings.TrimSuffix(x.Path,"/SKILL.md"))}}
 if len(candidates)==0{return nil,errors.New("skill directory with SKILL.md was not found in repository")};sort.Slice(candidates,func(i,j int)bool{return len(candidates[i])<len(candidates[j])});root=candidates[0]
 files:=[]struct{path,url string;size int64}{};var total int64
 for _,x:=range tree.Tree{if x.Type!="blob"||!(x.Path==root||strings.HasPrefix(x.Path,root+"/")){continue};rel:=strings.TrimPrefix(strings.TrimPrefix(x.Path,root),"/");if rel==""{continue};if x.Size>2<<20{return nil,fmt.Errorf("skill file too large: %s",rel)};total+=x.Size;if total>12<<20||len(files)>=256{return nil,errors.New("skill exceeds installation limits")};files=append(files,struct{path,url string;size int64}{rel,x.URL,x.Size})}
 if len(files)==0{return nil,errors.New("skill directory is empty")}
 temp,err:=os.MkdirTemp(filepath.Join(workspace,"skills"),".install-"+skill+"-");if err!=nil{if os.IsNotExist(err){if mk:=os.MkdirAll(filepath.Join(workspace,"skills"),0o755);mk!=nil{return nil,mk};temp,err=os.MkdirTemp(filepath.Join(workspace,"skills"),".install-"+skill+"-")}};if err!=nil{return nil,err};defer os.RemoveAll(temp)
 for _,f:=range files{if strings.Contains(f.path,"..")||filepath.IsAbs(f.path){return nil,errors.New("unsafe skill path")};blobRaw,_,err:=s.marketGET(ctx,f.url,4<<20);if err!=nil{return nil,err};var blob struct{Encoding,Content string};if err:=json.Unmarshal(blobRaw,&blob);err!=nil{return nil,err};if blob.Encoding!="base64"{return nil,errors.New("unsupported GitHub blob encoding")};data,err:=base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content,"\n",""));if err!=nil{return nil,err};dst:=filepath.Join(temp,filepath.FromSlash(f.path));clean,err:=filepath.Abs(dst);if err!=nil{return nil,err};base,_:=filepath.Abs(temp);if rel,err:=filepath.Rel(base,clean);err!=nil||rel==".."||strings.HasPrefix(rel,".."+string(filepath.Separator)){return nil,errors.New("unsafe extracted path")};if err:=os.MkdirAll(filepath.Dir(clean),0o755);err!=nil{return nil,err};if err:=os.WriteFile(clean,data,0o600);err!=nil{return nil,err}}
 if _,err:=os.Stat(filepath.Join(temp,"SKILL.md"));err!=nil{return nil,errors.New("downloaded skill lacks SKILL.md")}
 if err:=os.Rename(temp,target);err!=nil{return nil,err}
 return map[string]any{"installed":true,"already_installed":false,"name":skill,"source":source,"files":len(files)},nil
}

var _=zip.ErrFormat
var _=bytes.MinRead
