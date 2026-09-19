package session

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

var defaultTransient = NewTransientStore()

func DefaultTransientStore() *TransientStore { return defaultTransient }
func IsTransientKey(key string) bool { return strings.HasPrefix(key, "webui:tmp_") }

type TransientStore struct {
	mu sync.Mutex
	sessions map[string]*TransientSession
}
func NewTransientStore()*TransientStore{return &TransientStore{sessions:map[string]*TransientSession{}}}
func(s *TransientStore)Open(key string)(*TransientSession,error){s.mu.Lock();defer s.mu.Unlock();if v:=s.sessions[key];v!=nil{return v,nil};v:=&TransientSession{key:key,meta:map[string]any{},updatedAt:time.Now()};s.sessions[key]=v;return v,nil}
func(s *TransientStore)Delete(key string){s.mu.Lock();delete(s.sessions,key);s.mu.Unlock()}
func(s *TransientStore)Count()int{s.mu.Lock();defer s.mu.Unlock();return len(s.sessions)}

type TransientSession struct {
	mu sync.Mutex
	key string
	messages []core.Message
	meta map[string]any
	updatedAt time.Time
	lastArchived int
}
func(s *TransientSession)Key()string{return s.key}
func(s *TransientSession)Messages()[]core.Message{s.mu.Lock();defer s.mu.Unlock();out:=make([]core.Message,len(s.messages));copy(out,s.messages);return out}
func(s *TransientSession)AddMessage(m core.Message){s.mu.Lock();defer s.mu.Unlock();if m.Timestamp==""{m.Timestamp=formatNaive(time.Now())};s.messages=append(s.messages,m);s.updatedAt=time.Now()}
func(s *TransientSession)AppendMessagesDurable(ms []core.Message)error{for _,m:=range ms{s.AddMessage(m)};return nil}
func(s *TransientSession)Clear(){s.mu.Lock();s.messages=nil;s.meta=map[string]any{};s.lastArchived=0;s.updatedAt=time.Now();s.mu.Unlock()}
func(s *TransientSession)Save()error{return nil}
func(s *TransientSession)CompactJournal(int64)error{return nil}
func(s *TransientSession)SetMessage(i int,m core.Message)error{s.mu.Lock();defer s.mu.Unlock();if i>=0&&i<len(s.messages){s.messages[i]=m};return nil}
func(s *TransientSession)Metadata()map[string]any{s.mu.Lock();defer s.mu.Unlock();out:=make(map[string]any,len(s.meta));for k,v:=range s.meta{out[k]=v};return out}
func(s *TransientSession)UpdatedAt()time.Time{s.mu.Lock();defer s.mu.Unlock();return s.updatedAt}
func(s *TransientSession)LastArchived()int{s.mu.Lock();defer s.mu.Unlock();return s.lastArchived}
func(s *TransientSession)GetHistory(maxMessages,maxTokens int,extendToUser,includeRuntimeContext bool)[]core.Message{s.mu.Lock();defer s.mu.Unlock();start:=s.lastArchived;if start<0||start>len(s.messages){start=0};out:=append([]core.Message(nil),s.messages[start:]...);if maxMessages>0&&len(out)>maxMessages{out=out[len(out)-maxMessages:]};return out}
func(s *TransientSession)CommitSummaryCheckpoint(summary string,insertAt *int,lastActive *time.Time){s.mu.Lock();defer s.mu.Unlock();boundary:=len(s.messages);if insertAt!=nil{boundary=*insertAt};marker:=core.Message{Role:core.RoleUser,Content:core.TextContent("Continue the active task from the working-memory checkpoint above.")};marker.SetExtra("_hidden_history",json.RawMessage("true"));if boundary>=len(s.messages){s.messages=append(s.messages,marker)}else{s.messages=append(s.messages[:boundary+1],s.messages[boundary:]...);s.messages[boundary]=marker};active:=s.updatedAt;if lastActive!=nil{active=*lastActive};s.meta["_last_summary"]=map[string]any{"text":summary,"last_active":active.Format(time.RFC3339)};s.lastArchived=boundary;s.updatedAt=time.Now()}
