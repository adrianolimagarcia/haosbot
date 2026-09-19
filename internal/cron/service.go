package cron

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxRunHistory = 20

var (
	ErrNotFound  = errors.New("cron: job not found")
	ErrProtected = errors.New("cron: protected system job")
	ErrUnbound   = errors.New("cron: agent job is not bound to a session")
	ErrActive    = errors.New("cron: job is already running")
)

type SkippedError struct{ Reason string }

func (e SkippedError) Error() string {
	if e.Reason == "" {
		return "cron: skipped"
	}
	return e.Reason
}

type Service struct {
	mu       sync.Mutex
	storePath string
	runsDir   string
	executor  Executor
	store     Store
	active    map[string]bool

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	wg     sync.WaitGroup
	running bool
	loaded  bool
	maxSleep time.Duration
}

func NewService(storePath string, executor Executor) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		storePath: storePath,
		runsDir: filepath.Join(filepath.Dir(storePath), "runs"),
		executor: executor,
		store: Store{Version: 1},
		active: map[string]bool{},
		ctx: ctx,
		cancel: cancel,
		wake: make(chan struct{}, 1),
		maxSleep: 5 * time.Minute,
	}
}

// SetExecutor installs the callback used for agent-turn jobs. Call it before
// Start; replacing an executor while jobs are running is intentionally rejected.
func (s *Service) SetExecutor(executor Executor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.active) != 0 {
		return errors.New("cron: cannot replace executor while jobs are active")
	}
	s.executor = executor
	return nil
}

func (s *Service) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return nil
	}
	if err := s.loadLocked(); err != nil {
		return err
	}
	s.loaded = true
	return nil
}

func (s *Service) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return nil
	}
	if !s.loaded {
		if err := s.loadLocked(); err != nil {
			return err
		}
		s.loaded = true
	}
	now := time.Now()
	for i := range s.store.Jobs {
		job := &s.store.Jobs[i]
		if !job.Enabled {
			job.State.NextRunAtMS = nil
			continue
		}
		if err := validateBoundJob(*job); err != nil {
			job.Enabled = false
			job.State.NextRunAtMS = nil
			job.State.LastStatus = StatusError
			job.State.LastError = err.Error()
			continue
		}
		next, err := NextRun(job.Schedule, now)
		if err != nil {
			job.Enabled = false
			job.State.NextRunAtMS = nil
			job.State.LastStatus = StatusError
			job.State.LastError = err.Error()
			continue
		}
		job.State.NextRunAtMS = next
	}
	if err := s.saveLocked(); err != nil {
		return err
	}
	s.running = true
	s.wg.Add(1)
	go s.loop()
	return nil
}

func (s *Service) Close(ctx context.Context) error {
	s.cancel()
	s.signal()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) loop() {
	defer s.wg.Done()
	for {
		delay := s.nextDelay()
		timer := time.NewTimer(delay)
		select {
		case <-s.ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-s.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			continue
		case <-timer.C:
			s.dispatchDue()
		}
	}
}

func (s *Service) nextDelay() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	var earliest int64
	for i := range s.store.Jobs {
		job := &s.store.Jobs[i]
		if !job.Enabled || job.State.NextRunAtMS == nil || s.active[job.ID] {
			continue
		}
		if earliest == 0 || *job.State.NextRunAtMS < earliest {
			earliest = *job.State.NextRunAtMS
		}
	}
	if earliest == 0 {
		return s.maxSleep
	}
	d := time.Duration(earliest-now) * time.Millisecond
	if d < 0 {
		return 0
	}
	if d > s.maxSleep {
		return s.maxSleep
	}
	return d
}

func (s *Service) dispatchDue() {
	now := time.Now().UnixMilli()
	var due []Job

	s.mu.Lock()
	for i := range s.store.Jobs {
		job := &s.store.Jobs[i]
		if !job.Enabled || job.State.NextRunAtMS == nil || *job.State.NextRunAtMS > now || s.active[job.ID] {
			continue
		}
		s.active[job.ID] = true
		job.State.Pending = true
		due = append(due, cloneJob(*job))
	}
	s.mu.Unlock()

	for _, job := range due {
		s.wg.Add(1)
		go func(j Job) {
			defer s.wg.Done()
			s.execute(j)
		}(job)
	}
}

func (s *Service) execute(snapshot Job) {
	start := time.Now()
	runID := newRunID(snapshot.ID, start.UnixMilli())
	result := RunResult{RunID: runID}
	var execErr error

	if s.executor == nil {
		execErr = errors.New("cron: no executor configured")
	} else {
		result, execErr = s.executor(withExecutionContext(s.ctx), snapshot, runID)
		if result.RunID == "" {
			result.RunID = runID
		}
	}

	status := StatusOK
	errText := ""
	if execErr != nil {
		var skipped SkippedError
		if errors.As(execErr, &skipped) {
			status = StatusSkipped
		} else {
			status = StatusError
		}
		errText = execErr.Error()
	}
	end := time.Now()
	record := RunRecord{
		RunAtMS: start.UnixMilli(), Status: status,
		DurationMS: end.Sub(start).Milliseconds(), Error: errText, RunID: result.RunID,
	}
	_ = s.writeRunRecord(snapshot, record, result.Response)

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, snapshot.ID)

	idx := s.indexLocked(snapshot.ID)
	if idx < 0 {
		s.signal()
		return
	}
	job := &s.store.Jobs[idx]
	job.State.Pending = false
	last := record.RunAtMS
	job.State.LastRunAtMS = &last
	job.State.LastStatus = status
	job.State.LastError = errText
	job.State.RunHistory = append(job.State.RunHistory, record)
	if len(job.State.RunHistory) > maxRunHistory {
		job.State.RunHistory = append([]RunRecord(nil), job.State.RunHistory[len(job.State.RunHistory)-maxRunHistory:]...)
	}
	job.UpdatedAtMS = end.UnixMilli()

	if job.Schedule.Kind == KindAt {
		if job.DeleteAfterRun {
			s.store.Jobs = append(s.store.Jobs[:idx], s.store.Jobs[idx+1:]...)
		} else {
			job.Enabled = false
			job.State.NextRunAtMS = nil
		}
	} else if job.Enabled {
		next, err := NextRun(job.Schedule, end)
		if err != nil {
			job.Enabled = false
			job.State.NextRunAtMS = nil
			job.State.LastStatus = StatusError
			job.State.LastError = err.Error()
		} else {
			job.State.NextRunAtMS = next
		}
	} else {
		job.State.NextRunAtMS = nil
	}
	_ = s.saveLocked()
	s.signal()
}

func (s *Service) AddJob(job Job) (Job, error) {
	if strings.TrimSpace(job.Name) == "" {
		return Job{}, errors.New("cron: name is required")
	}
	if strings.TrimSpace(job.Payload.Message) == "" && job.Payload.Kind == PayloadAgentTurn {
		return Job{}, errors.New("cron: message is required")
	}
	if job.Payload.Kind == "" {
		job.Payload.Kind = PayloadAgentTurn
	}
	if err := validateBoundJob(job); err != nil {
		return Job{}, err
	}
	if err := ValidateSchedule(job.Schedule); err != nil {
		return Job{}, err
	}
	now := time.Now()
	if job.ID == "" {
		job.ID = newID()
	}
	job.Enabled = true
	job.CreatedAtMS = now.UnixMilli()
	job.UpdatedAtMS = job.CreatedAtMS
	next, err := NextRun(job.Schedule, now)
	if err != nil {
		return Job{}, err
	}
	job.State.NextRunAtMS = next

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.indexLocked(job.ID) >= 0 {
		return Job{}, fmt.Errorf("cron: duplicate job id %q", job.ID)
	}
	s.store.Jobs = append(s.store.Jobs, cloneJob(job))
	if err := s.saveLocked(); err != nil {
		s.store.Jobs = s.store.Jobs[:len(s.store.Jobs)-1]
		return Job{}, err
	}
	s.signal()
	return cloneJob(job), nil
}

func (s *Service) ListJobs(includeDisabled bool) []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Job, 0, len(s.store.Jobs))
	for _, job := range s.store.Jobs {
		if !includeDisabled && !job.Enabled {
			continue
		}
		c := cloneJob(job)
		c.State.Pending = s.active[job.ID]
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].State.NextRunAtMS, out[j].State.NextRunAtMS
		if a == nil && b == nil {
			return out[i].Name < out[j].Name
		}
		if a == nil {
			return false
		}
		if b == nil {
			return true
		}
		return *a < *b
	})
	return out
}

func (s *Service) GetJob(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.indexLocked(id)
	if idx < 0 {
		return Job{}, false
	}
	out := cloneJob(s.store.Jobs[idx])
	out.State.Pending = s.active[id]
	return out, true
}

func (s *Service) UpdateJob(id string, update Update) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.indexLocked(id)
	if idx < 0 {
		return Job{}, ErrNotFound
	}
	job := &s.store.Jobs[idx]
	if job.Payload.Kind == PayloadSystemEvent {
		return Job{}, ErrProtected
	}
	scheduleChanged := false
	if update.Name != nil {
		name := strings.TrimSpace(*update.Name)
		if name == "" {
			return Job{}, errors.New("cron: name cannot be empty")
		}
		job.Name = name
	}
	if update.Message != nil {
		message := strings.TrimSpace(*update.Message)
		if message == "" {
			return Job{}, errors.New("cron: message cannot be empty")
		}
		job.Payload.Message = message
	}
	if update.Schedule != nil {
		if err := ValidateSchedule(*update.Schedule); err != nil {
			return Job{}, err
		}
		job.Schedule = *update.Schedule
		scheduleChanged = true
	}
	if update.DeleteAfterRun != nil {
		job.DeleteAfterRun = *update.DeleteAfterRun
	}
	if update.Enabled != nil {
		job.Enabled = *update.Enabled
	}
	job.UpdatedAtMS = time.Now().UnixMilli()
	if !job.Enabled {
		job.State.NextRunAtMS = nil
	} else if scheduleChanged || update.Enabled != nil {
		next, err := NextRun(job.Schedule, time.Now())
		if err != nil {
			return Job{}, err
		}
		job.State.NextRunAtMS = next
	}
	if err := s.saveLocked(); err != nil {
		return Job{}, err
	}
	s.signal()
	out := cloneJob(*job)
	out.State.Pending = s.active[id]
	return out, nil
}

func (s *Service) RemoveJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.indexLocked(id)
	if idx < 0 {
		return ErrNotFound
	}
	if s.store.Jobs[idx].Payload.Kind == PayloadSystemEvent {
		return ErrProtected
	}
	s.store.Jobs = append(s.store.Jobs[:idx], s.store.Jobs[idx+1:]...)
	if err := s.saveLocked(); err != nil {
		return err
	}
	s.signal()
	return nil
}

func (s *Service) RunNow(id string, force bool) error {
	s.mu.Lock()
	idx := s.indexLocked(id)
	if idx < 0 {
		s.mu.Unlock()
		return ErrNotFound
	}
	if s.active[id] {
		s.mu.Unlock()
		return ErrActive
	}
	job := s.store.Jobs[idx]
	if !force && !job.Enabled {
		s.mu.Unlock()
		return errors.New("cron: job is disabled")
	}
	if err := validateBoundJob(job); err != nil {
		s.mu.Unlock()
		return err
	}
	s.active[id] = true
	s.store.Jobs[idx].State.Pending = true
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.execute(job)
	}()
	return nil
}

func (s *Service) indexLocked(id string) int {
	for i := range s.store.Jobs {
		if s.store.Jobs[i].ID == id {
			return i
		}
	}
	return -1
}

func validateBoundJob(job Job) error {
	if job.Payload.Kind == PayloadSystemEvent {
		return nil
	}
	if job.Payload.Kind != PayloadAgentTurn {
		return fmt.Errorf("cron: unsupported payload kind %q", job.Payload.Kind)
	}
	if strings.TrimSpace(job.Payload.SessionKey) == "" ||
		strings.TrimSpace(job.Payload.OriginChannel) == "" ||
		strings.TrimSpace(job.Payload.OriginChatID) == "" {
		return ErrUnbound
	}
	return nil
}

func (s *Service) loadLocked() error {
	s.store = Store{Version: 1}
	data, err := os.ReadFile(s.storePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := json.Unmarshal(data, &s.store); err != nil {
		backup := fmt.Sprintf("%s.corrupt-%d", s.storePath, time.Now().Unix())
		if renameErr := os.Rename(s.storePath, backup); renameErr != nil {
			return fmt.Errorf("cron: corrupt store and backup failed: parse=%v backup=%v", err, renameErr)
		}
		return fmt.Errorf("cron: store was corrupt and preserved at %s: %w", backup, err)
	}
	if s.store.Version == 0 {
		s.store.Version = 1
	}
	if s.store.Jobs == nil {
		s.store.Jobs = []Job{}
	}
	return nil
}

func (s *Service) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.storePath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.store, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.storePath), ".jobs-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.storePath); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(s.storePath)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (s *Service) writeRunRecord(job Job, run RunRecord, response string) error {
	if err := os.MkdirAll(s.runsDir, 0o700); err != nil {
		return err
	}
	record := map[string]any{
		"run_id": run.RunID,
		"job_id": job.ID,
		"job_name": job.Name,
		"session_key": job.Payload.SessionKey,
		"status": run.Status,
		"created_at_ms": run.RunAtMS,
		"duration_ms": run.DurationMS,
		"error": run.Error,
		"response": response,
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	name := safeRunName(run.RunID) + ".json"
	return os.WriteFile(filepath.Join(s.runsDir, name), data, 0o600)
}

func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func newID() string {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}

func newRunID(jobID string, ts int64) string {
	var raw [4]byte
	_, _ = rand.Read(raw[:])
	return fmt.Sprintf("%s:%d:%s", jobID, ts, hex.EncodeToString(raw[:]))
}

func safeRunName(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

type executionContextKey struct{}

func withExecutionContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, executionContextKey{}, true)
}

func InExecution(ctx context.Context) bool {
	v, _ := ctx.Value(executionContextKey{}).(bool)
	return v
}
