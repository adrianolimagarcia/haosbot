package triggers

import (
	"context"; "crypto/rand"; "encoding/hex"; "encoding/json"; "errors"; "fmt"; "os"; "path/filepath"; "sort"; "strings"; "sync"; "time"
)
const maxAttempts=10
const maxHistory=20
type Executor func(context.Context,Trigger,Delivery)(string,error)
type Service struct {
	mu sync.Mutex; root,storePath,inbox,processing,failed,runs string; executor Executor
	ctx context.Context; cancel context.CancelFunc; wake chan struct{}; wg sync.WaitGroup; running bool
}
func NewService(root string, executor Executor)*Service{ctx,cancel:=context.WithCancel(context.Background());return &Service{root:root,storePath:filepath.Join(root,"triggers.json"),inbox:filepath.Join(root,"inbox"),processing:filepath.Join(root,"processing"),failed:filepath.Join(root,"failed"),runs:filepath.Join(root,"runs"),executor:executor,ctx:ctx,cancel:cancel,wake:make(chan struct{},1)}}
func(s *Service)SetExecutor(executor Executor)error{s.mu.Lock();defer s.mu.Unlock();if s.running{return errors.New("triggers: cannot replace executor while running")};s.executor=executor;return nil}
func(s *Service)ensureDirs()error{for _,p:=range []string{s.root,s.inbox,s.processing,s.failed,s.runs}{if err:=os.MkdirAll(p,0o700);err!=nil{return err}};return nil}
func(s *Service)Start()error{s.mu.Lock();if s.running{s.mu.Unlock();return nil};if err:=s.ensureDirs();err!=nil{s.mu.Unlock();return err};if err:=s.recoverProcessingLocked();err!=nil{s.mu.Unlock();return err};s.running=true;s.wg.Add(1);s.mu.Unlock();go s.loop();return nil}
func(s *Service)Running()bool{s.mu.Lock();defer s.mu.Unlock();return s.running}
func(s *Service)Close(ctx context.Context)error{s.cancel();s.signal();done:=make(chan struct{});go func(){s.wg.Wait();close(done)}();select{case<-done:s.mu.Lock();s.running=false;s.mu.Unlock();return nil;case<-ctx.Done():return ctx.Err()}}
func(s *Service)loop(){defer s.wg.Done();t:=time.NewTicker(500*time.Millisecond);defer t.Stop();for{select{case<-s.ctx.Done():return;case<-s.wake:s.drain();case<-t.C:s.drain()}}}
// drain processes queued deliveries until the inbox is empty, the service is
// shutting down, or one delivery fails. A failure ends the pass: the retry is
// written straight back to the inbox, so continuing the loop would burn every
// remaining attempt back-to-back instead of letting the ticker pace them.
func (s *Service) drain() {
	for {
		if s.ctx.Err() != nil {
			return
		}
		d, tr, ok := s.claimOne()
		if !ok {
			return
		}
		response, err := s.execute(tr, d)
		if s.finish(d, tr, response, err) {
			return
		}
	}
}
func(s *Service)execute(tr Trigger,d Delivery)(string,error){s.mu.Lock();executor:=s.executor;s.mu.Unlock();if executor==nil{return "",errors.New("triggers: no executor configured")};ctx,cancel:=context.WithTimeout(s.ctx,10*time.Minute);defer cancel();return executor(ctx,tr,d)}
func(s *Service)Create(name,channel,chatID,sessionKey string,metadata map[string]any)(Trigger,error){name=strings.TrimSpace(name);channel=strings.TrimSpace(channel);chatID=strings.TrimSpace(chatID);sessionKey=strings.TrimSpace(sessionKey);if name==""||channel==""||chatID==""||sessionKey==""{return Trigger{},errors.New("triggers: name, channel, chat_id and session_key are required")};s.mu.Lock();defer s.mu.Unlock();if err:=s.ensureDirs();err!=nil{return Trigger{},err};items,err:=s.loadLocked();if err!=nil{return Trigger{},err};now:=time.Now().UnixMilli();tr:=Trigger{ID:newID(),Name:name,Enabled:true,Channel:channel,ChatID:chatID,SessionKey:sessionKey,SenderID:"trigger",OriginMetadata:cloneMap(metadata),CreatedAtMS:now,UpdatedAtMS:now};items=append(items,tr);if err:=s.saveLocked(items);err!=nil{return Trigger{},err};return tr,nil}
func(s *Service)List(includeDisabled bool)[]Trigger{s.mu.Lock();defer s.mu.Unlock();items,err:=s.loadLocked();if err!=nil{return nil};out:=make([]Trigger,0,len(items));for _,tr:=range items{if includeDisabled||tr.Enabled{out=append(out,tr)}};sort.SliceStable(out,func(i,j int)bool{return out[i].UpdatedAtMS>out[j].UpdatedAtMS});return out}
func(s *Service)Get(id string)(Trigger,bool){s.mu.Lock();defer s.mu.Unlock();items,err:=s.loadLocked();if err!=nil{return Trigger{},false};for _,tr:=range items{if tr.ID==id{return tr,true}};return Trigger{},false}
func(s *Service)Update(id string,name *string,enabled *bool)(Trigger,error){s.mu.Lock();defer s.mu.Unlock();items,err:=s.loadLocked();if err!=nil{return Trigger{},err};for i:=range items{if items[i].ID!=id{continue};if name!=nil{v:=strings.TrimSpace(*name);if v==""{return Trigger{},errors.New("triggers: name cannot be empty")};items[i].Name=v};if enabled!=nil{items[i].Enabled=*enabled};items[i].UpdatedAtMS=time.Now().UnixMilli();if err:=s.saveLocked(items);err!=nil{return Trigger{},err};return items[i],nil};return Trigger{},os.ErrNotExist}
func(s *Service)Delete(id string)error{s.mu.Lock();defer s.mu.Unlock();items,err:=s.loadLocked();if err!=nil{return err};out:=make([]Trigger,0,len(items));found:=false;for _,tr:=range items{if tr.ID==id{found=true;continue};out=append(out,tr)};if !found{return os.ErrNotExist};return s.saveLocked(out)}
func(s *Service)Enqueue(id,content string)(Delivery,error){content=strings.TrimSpace(content);if content==""{return Delivery{},errors.New("triggers: content is required")};s.mu.Lock();defer s.mu.Unlock();if err:=s.ensureDirs();err!=nil{return Delivery{},err};items,err:=s.loadLocked();if err!=nil{return Delivery{},err};idx:=-1;for i:=range items{if items[i].ID==id{idx=i;break}};if idx<0{return Delivery{},os.ErrNotExist};if !items[idx].Enabled{return Delivery{},errors.New("triggers: trigger is disabled")};d:=Delivery{ID:"tdl_"+newID()+newID(),TriggerID:id,Content:content,CreatedAtMS:time.Now().UnixMilli()};path:=filepath.Join(s.inbox,fmt.Sprintf("%d-%s.json",d.CreatedAtMS,d.ID));if err:=writeJSONAtomic(path,d);err!=nil{return Delivery{},err};items[idx].LastMessage=truncate(content,4000);items[idx].UpdatedAtMS=d.CreatedAtMS;if err:=s.saveLocked(items);err!=nil{_ = os.Remove(path);return Delivery{},err};s.signal();return d,nil}
func(s *Service)claimOne()(Delivery,Trigger,bool){s.mu.Lock();defer s.mu.Unlock();entries,err:=os.ReadDir(s.inbox);if err!=nil||len(entries)==0{return Delivery{},Trigger{},false};sort.Slice(entries,func(i,j int)bool{return entries[i].Name()<entries[j].Name()});for _,entry:=range entries{if entry.IsDir()||!strings.HasSuffix(entry.Name(),".json"){continue};src:=filepath.Join(s.inbox,entry.Name());raw,err:=os.ReadFile(src);if err!=nil{continue};var d Delivery;if json.Unmarshal(raw,&d)!=nil{_ = os.Rename(src,filepath.Join(s.failed,entry.Name()));continue};items,err:=s.loadLocked();if err!=nil{return Delivery{},Trigger{},false};var tr Trigger;found:=false;for _,x:=range items{if x.ID==d.TriggerID{tr=x;found=true;break}};if !found||!tr.Enabled{_ = os.Rename(src,filepath.Join(s.failed,entry.Name()));continue};dst:=filepath.Join(s.processing,entry.Name());if err:=os.Rename(src,dst);err!=nil{continue};d.Path=dst;return d,tr,true};return Delivery{},Trigger{},false}
// finish records the run and decides the delivery's fate. It reports whether
// the delivery failed, which is what ends the current drain pass.
func (s *Service) finish(d Delivery, tr Trigger, response string, runErr error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.loadLocked()
	if err != nil {
		return false
	}
	status := "ok"
	errText := ""
	if runErr != nil {
		status = "error"
		errText = runErr.Error()
	}
	for i := range items {
		if items[i].ID != tr.ID {
			continue
		}
		now := time.Now().UnixMilli()
		items[i].LastRunAtMS = &now
		items[i].LastStatus = status
		items[i].LastError = errText
		items[i].UpdatedAtMS = now
		items[i].RunHistory = append(items[i].RunHistory, RunRecord{RunAtMS: d.CreatedAtMS, Status: status, Error: errText, DeliveryID: d.ID})
		if len(items[i].RunHistory) > maxHistory {
			items[i].RunHistory = append([]RunRecord(nil), items[i].RunHistory[len(items[i].RunHistory)-maxHistory:]...)
		}
		break
	}
	_ = s.saveLocked(items)
	record := map[string]any{"run_id": d.ID, "trigger_id": tr.ID, "trigger_name": tr.Name, "status": status, "created_at_ms": d.CreatedAtMS, "error": errText, "response": response}
	_ = writeJSONAtomic(filepath.Join(s.runs, d.ID+".json"), record)
	if runErr == nil {
		_ = os.Remove(d.Path)
		return false
	}
	d.LastError = errText
	// Shutdown interrupted this delivery. Requeue it without consuming an
	// attempt, otherwise every delivery still queued when the gateway stops
	// would be retried against a canceled context and burned to failed/.
	if s.ctx.Err() != nil {
		requeueDelivery(d, filepath.Join(s.inbox, filepath.Base(d.Path)))
		return true
	}
	d.Attempts++
	if d.Attempts >= maxAttempts {
		requeueDelivery(d, filepath.Join(s.failed, filepath.Base(d.Path)))
		return true
	}
	requeueDelivery(d, filepath.Join(s.inbox, filepath.Base(d.Path)))
	s.signal()
	return true
}

// requeueDelivery writes the delivery to dst and only then drops the processing
// copy. If the write fails the file stays in processing/, where
// recoverProcessingLocked picks it up on the next start, so a failed retry can
// never leave the delivery in no queue at all.
func requeueDelivery(d Delivery, dst string) {
	if err := writeJSONAtomic(dst, d); err != nil {
		return
	}
	_ = os.Remove(d.Path)
}
func(s *Service)recoverProcessingLocked()error{entries,err:=os.ReadDir(s.processing);if err!=nil{return err};for _,e:=range entries{if e.IsDir(){continue};src:=filepath.Join(s.processing,e.Name());dst:=filepath.Join(s.inbox,e.Name());if err:=os.Rename(src,dst);err!=nil{return err}};return nil}
func(s *Service)loadLocked()([]Trigger,error){raw,err:=os.ReadFile(s.storePath);if os.IsNotExist(err){return []Trigger{},nil};if err!=nil{return nil,err};var store struct{Version int `json:"version"`;Triggers []Trigger `json:"triggers"`};if err:=json.Unmarshal(raw,&store);err!=nil{return nil,err};return store.Triggers,nil}
func(s *Service)saveLocked(items []Trigger)error{return writeJSONAtomic(s.storePath,map[string]any{"version":1,"triggers":items})}
func writeJSONAtomic(path string,v any)error{if err:=os.MkdirAll(filepath.Dir(path),0o700);err!=nil{return err};raw,err:=json.MarshalIndent(v,"","  ");if err!=nil{return err};f,err:=os.CreateTemp(filepath.Dir(path),".tmp-*");if err!=nil{return err};name:=f.Name();ok:=false;defer func(){_ = f.Close();if !ok{_ = os.Remove(name)}}();if err:=f.Chmod(0o600);err!=nil{return err};if _,err:=f.Write(raw);err!=nil{return err};if err:=f.Sync();err!=nil{return err};if err:=f.Close();err!=nil{return err};if err:=os.Rename(name,path);err!=nil{return err};ok=true;return nil}
func(s *Service)signal(){select{case s.wake<-struct{}{}:default:}}
func newID()string{var b [4]byte;if _,err:=rand.Read(b[:]);err!=nil{return fmt.Sprintf("%08x",time.Now().UnixNano())};return hex.EncodeToString(b[:])}
func cloneMap(in map[string]any)map[string]any{if len(in)==0{return nil};out:=make(map[string]any,len(in));for k,v:=range in{out[k]=v};return out}
func truncate(s string,n int)string{r:=[]rune(s);if len(r)<=n{return s};return string(r[:n])}
