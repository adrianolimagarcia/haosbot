package api

import (
 "crypto/rand"
 "encoding/hex"
 "errors"
 "fmt"
 "io"
 "net/http"
 "os"
 "path/filepath"
 "sort"
 "strings"
 "time"

 "github.com/adrianolimagarcia/nanobot-go/internal/config"
 "github.com/adrianolimagarcia/nanobot-go/internal/core"
)

const ( maxWebUIAttachmentBytes=20<<20; maxWebUIAttachmentCount=8; maxWebUIPreviewBytes=128<<10 )

type webUIAttachment struct { ID string `json:"id"`; Name string `json:"name"`; Path string `json:"path"`; MIME string `json:"mime"`; Size int64 `json:"size"`; Preview string `json:"preview,omitempty"`; Binary bool `json:"binary"`; Created string `json:"created_at,omitempty"` }

func (s *Server) handleWebUIAttachment(w http.ResponseWriter,r *http.Request){
 if r.Method==http.MethodPost { r.Body=http.MaxBytesReader(w,r.Body,maxWebUIAttachmentBytes+(1<<20)); if err:=r.ParseMultipartForm(maxWebUIAttachmentBytes);err!=nil{http.Error(w,"invalid or oversized attachment upload",http.StatusBadRequest);return} }
 sessionID:=strings.TrimSpace(r.FormValue("sessionId")); if sessionID==""{sessionID=strings.TrimSpace(r.URL.Query().Get("sessionId"))}; if sessionID==""{sessionID=strings.TrimSpace(r.Header.Get("X-HAOS-Session-ID"))}
 if !webSessionIDPattern.MatchString(sessionID){http.Error(w,"invalid sessionId",http.StatusBadRequest);return}
 switch r.Method {
 case http.MethodGet: items,err:=s.listWebUIAttachments(sessionID);if err!=nil{http.Error(w,"attachment list failed",500);return};writeWebUIJSON(w,map[string]any{"attachments":items,"count":len(items),"limit":maxWebUIAttachmentCount})
 case http.MethodDelete: id:=strings.TrimSpace(r.URL.Query().Get("id"));if !validAttachmentID(id){http.Error(w,"valid attachment id is required",400);return};path,err:=s.resolveWebUIAttachment(sessionID,id);if err!=nil{http.Error(w,"attachment not found",404);return};if err:=os.Remove(path);err!=nil&&!errors.Is(err,os.ErrNotExist){http.Error(w,"attachment delete failed",500);return};w.WriteHeader(http.StatusNoContent)
 case http.MethodPost: s.handleWebUIAttachmentUpload(w,r,sessionID)
 default:w.Header().Set("Allow","GET, POST, DELETE");http.Error(w,"Method not allowed",405)
 }
}

func (s *Server) handleWebUIAttachmentUpload(w http.ResponseWriter,r *http.Request,sessionID string){
 items,err:=s.listWebUIAttachments(sessionID);if err!=nil{http.Error(w,"attachment list failed",500);return};if len(items)>=maxWebUIAttachmentCount{http.Error(w,"attachment limit reached",409);return}
 file,header,err:=r.FormFile("file");if err!=nil{http.Error(w,"file is required",400);return};defer file.Close();data,err:=io.ReadAll(io.LimitReader(file,maxWebUIAttachmentBytes+1));if err!=nil{http.Error(w,"attachment read failed",400);return};if len(data)==0{http.Error(w,"attachment is empty",400);return};if len(data)>maxWebUIAttachmentBytes{http.Error(w,"attachment exceeds 20 MiB",413);return}
 mime:=http.DetectContentType(data[:min(len(data),512)]);if !webUIAttachmentMIMEAllowed(mime){http.Error(w,"unsupported attachment type: "+mime,415);return};if mime=="application/pdf"&&!strings.HasPrefix(string(data),"%PDF-"){http.Error(w,"invalid PDF signature",415);return}
 dir:=filepath.Join(s.webUIMediaRoot(),sessionID);if err:=os.MkdirAll(dir,0o700);err!=nil{http.Error(w,"attachment storage unavailable",500);return};var random [12]byte;if _,err:=rand.Read(random[:]);err!=nil{http.Error(w,"attachment id generation failed",500);return};id:=hex.EncodeToString(random[:]);path:=filepath.Join(dir,id+safeAttachmentExtension(header.Filename,mime));if err:=writeWebUIAtomic(path,data,0o600);err!=nil{http.Error(w,"attachment write failed",500);return};item:=describeWebUIAttachment(path,filepath.Base(header.Filename));item.ID=id;writeWebUIJSON(w,item)
}

func webUIAttachmentMIMEAllowed(mime string)bool{if strings.HasPrefix(mime,"text/"){return true};switch mime{case "image/png","image/jpeg","image/gif","image/webp","application/json","application/xml","application/pdf":return true};return false}
func safeAttachmentExtension(name,mime string)string{ext:=strings.ToLower(filepath.Ext(filepath.Base(name)));if len(ext)>12{ext=""};for _,r:=range ext{if !(r=='.'||r>='a'&&r<='z'||r>='0'&&r<='9'){ext="";break}};if ext!=""{return ext};switch mime{case"image/png":return".png";case"image/jpeg":return".jpg";case"image/gif":return".gif";case"image/webp":return".webp";case"application/pdf":return".pdf";case"application/json":return".json";case"application/xml","text/xml":return".xml"};return".bin"}
func(s *Server)webUIMediaRoot()string{dir:=config.DefaultDataDir();if p:=s.dataDir.Load();p!=nil&&strings.TrimSpace(*p)!=""{dir=*p};return filepath.Join(dir,"webui-media")}
func validAttachmentID(id string)bool{if len(id)!=24{return false};_,err:=hex.DecodeString(id);return err==nil}
func(s *Server)resolveWebUIAttachment(sessionID,id string)(string,error){if !webSessionIDPattern.MatchString(sessionID)||!validAttachmentID(id){return"",errors.New("invalid attachment reference")};dir:=filepath.Join(s.webUIMediaRoot(),sessionID);entries,err:=os.ReadDir(dir);if err!=nil{return"",err};for _,entry:=range entries{if entry.Type().IsRegular()&&strings.TrimSuffix(entry.Name(),filepath.Ext(entry.Name()))==id{return filepath.Join(dir,entry.Name()),nil}};return"",os.ErrNotExist}

func(s *Server)validateWebUIMedia(sessionID string,media []string)([]string,error){if len(media)==0{return nil,nil};if len(media)>maxWebUIAttachmentCount{return nil,fmt.Errorf("too many attachments: %d",len(media))};root,err:=filepath.Abs(filepath.Join(s.webUIMediaRoot(),sessionID));if err!=nil{return nil,err};out:=make([]string,0,len(media));seen:=make(map[string]struct{},len(media));for _,raw:=range media{path,err:=filepath.Abs(strings.TrimSpace(raw));if err!=nil{return nil,errors.New("invalid attachment path")};rel,err:=filepath.Rel(root,path);if err!=nil||rel==".."||strings.HasPrefix(rel,".."+string(os.PathSeparator)){return nil,errors.New("attachment does not belong to this session")};info,err:=os.Lstat(path);if err!=nil{return nil,errors.New("attachment is unavailable")};if !info.Mode().IsRegular()||info.Mode()&os.ModeSymlink!=0{return nil,errors.New("attachment is not a regular file")};if info.Size()<=0||info.Size()>maxWebUIAttachmentBytes{return nil,errors.New("attachment size is invalid")};if _,ok:=seen[path];ok{continue};seen[path]=struct{}{};out=append(out,path)};return out,nil}

func(s *Server)listWebUIAttachments(sessionID string)([]webUIAttachment,error){dir:=filepath.Join(s.webUIMediaRoot(),sessionID);entries,err:=os.ReadDir(dir);if errors.Is(err,os.ErrNotExist){return[]webUIAttachment{},nil};if err!=nil{return nil,err};items:=make([]webUIAttachment,0,len(entries));for _,entry:=range entries{if !entry.Type().IsRegular(){continue};id:=strings.TrimSuffix(entry.Name(),filepath.Ext(entry.Name()));if !validAttachmentID(id){continue};item:=describeWebUIAttachment(filepath.Join(dir,entry.Name()),entry.Name());item.ID=id;items=append(items,item)};sort.Slice(items,func(i,j int)bool{return items[i].Created>items[j].Created});return items,nil}
func describeWebUIAttachment(path,name string)webUIAttachment{item:=webUIAttachment{Name:filepath.Base(name),Path:path,Binary:true};info,err:=os.Stat(path);if err==nil{item.Size=info.Size();item.Created=info.ModTime().UTC().Format(time.RFC3339)};f,err:=os.Open(path);if err!=nil{return item};defer f.Close();head:=make([]byte,512);n,_:=f.Read(head);item.MIME=http.DetectContentType(head[:n]);if strings.HasPrefix(item.MIME,"text/")||item.MIME=="application/json"||item.MIME=="application/xml"{_,_=f.Seek(0,io.SeekStart);data,_:=io.ReadAll(io.LimitReader(f,maxWebUIPreviewBytes+1));if len(data)>maxWebUIPreviewBytes{data=data[:maxWebUIPreviewBytes]};item.Preview=string(data);item.Binary=false}else if item.MIME=="application/pdf"{item.Preview="PDF document"};return item}

func(s *Server)handleWebUISessionContext(w http.ResponseWriter,r *http.Request){if r.Method!=http.MethodGet{w.Header().Set("Allow",http.MethodGet);http.Error(w,"Method not allowed",405);return};key:=strings.TrimSpace(r.URL.Query().Get("key"));if key==""{http.Error(w,"key is required",400);return};store:=s.sessionStore.Load();if store==nil{http.Error(w,"session store unavailable",503);return};sess,err:=store.Open(key);if err!=nil{http.Error(w,err.Error(),500);return};messages:=sess.Messages();chars:=0;toolEvents:=make([]map[string]any,0);for _,m:=range messages{if m.Content.IsText(){chars+=len([]rune(m.Content.Text))}else{for _,b:=range m.Content.Blocks{chars+=len([]rune(b.Text))}};for _,call:=range m.ToolCalls{toolEvents=append(toolEvents,map[string]any{"type":"tool_call","id":call.ID,"name":call.Name,"timestamp":m.Timestamp})};if m.Role==core.RoleTool{toolEvents=append(toolEvents,map[string]any{"type":"tool_result","id":m.ToolCallID,"name":m.Name,"timestamp":m.Timestamp})}};meta:=sess.Metadata();summary,_:=meta["_last_summary"].(map[string]any);writeWebUIJSON(w,map[string]any{"key":key,"messages":len(messages),"approx_chars":chars,"approx_tokens":(chars+3)/4,"context_window_tokens":s.cfg.Agents.Defaults.ContextWindowTokens,"last_archived":sess.LastArchived(),"summary":summary,"activity":toolEvents})}
func cleanupWebUIMedia(root,sessionID string)error{if !webSessionIDPattern.MatchString(sessionID){return errors.New("invalid session id")};return os.RemoveAll(filepath.Join(root,sessionID))}
