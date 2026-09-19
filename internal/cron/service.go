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

const (
	maxRunHistory       = 20
	defaultMaxConcurrent = 2
	defaultJobTimeout    = 10 * time.Minute
)

var (
	ErrNotFound  = errors.New("cron: job not found")
	ErrProtected = errors.New("cron: protected system job")
	ErrUnbound   = errors.New("cron: agent job is not bound to a session")
	ErrActive    = errors.New("cron: job is already running")
	ErrLeaseHeld = errors.New("cron: scheduler lease is held by another gateway")
)

type SkippedError struct{ Reason string }

func (e SkippedError) Error() string {
	if e.Reason == "" {
		return "cron: skipped"
	}
	return e.Reason
}

type Service struct {
	mu        sync.Mutex
	storePath string
	runsDir   string
	leasePath string
	executor  Executor
	store     Store
	active    map[string]context.CancelFunc

	ctx           context.Context
	cancel        context.CancelFunc
	wake          chan struct{}
	wg            sync.WaitGroup
	running       bool
	loaded        bool
	dirty         bool
	maxSleep      time.Duration
	maxConcurrent int
	lease         *processLease
}

func NewService(storePath string, executor Executor) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		storePath:      storePath,
		runsDir:        filepath.Join(filepath.Dir(storePath), "runs"),
		leasePath:      filepath.Join(filepath.Dir(storePath), ".gateway.lock"),
		executor:       executor,
		store:          Store{Version: 2},
		active:         map[string]context.CancelFunc{},
		ctx:            ctx,
		cancel:         cancel,
		wake:           make(chan struct{}, 1),
		maxSleep:       5 * time.Minute,
		maxConcurrent:  defaultMaxConcurrent,
	}
}

func filepathDir(path string) string { return filepath.Dir(path) }

func (s *Service) SetExecutor(executor Executor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.active) != 0 {
		return errors.New("cron: cannot replace executor while jobs are active")
	}
	s.executor = executor
	return nil
}

func (s *Service) SetMaxConcurrent(n int) {
	if n < 1 {
		n = 1
	}
	s.mu.Lock()
	s.maxConcurrent = n
	s.mu.Unlock()
	s.signal()
}

func (s *Service) MaxConcurrent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxConcurrent
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
	if s.running {
		s.mu.Unlock()
		return nil
	}
	if !s.loaded {
		if err := s.loadLocked(); err != nil {
			s.mu.Unlock()
			return err
		}
		s.loaded = true
	}
	s.mu.Unlock()

	lease, err := acquireProcessLease(s.leasePath)
	if err != nil {
		return err
	}

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		lease.release()
		return nil
	}
	s.lease = lease
	now := time.Now()
	for i := range s.store.Jobs {
		s.prepareJobOnStartLocked(&s.store.Jobs[i], now)
	}
	if err := s.saveLocked(); err != nil {
		s.lease = nil
		s.mu.Unlock()
		lease.release()
		return err
	}
	s.running = true
	s.wg.Add(1)
	s.mu.Unlock()

	go s.loop()
	s.signal()
	return nil
}

func (s *Service) prepareJobOnStartLocked(job *Job, now time.Time) {
	if !job.Enabled {
		job.State.NextRunAtMS = nil
		return
	}
	normalizeJobPolicy(job)
	if err := validateBoundJob(*job); err != nil {
		job.Enabled = false
		job.State.NextRunAtMS = nil
		job.State.LastStatus = StatusError
		job.State.LastError = err.Error()
		return
	}
	if job.State.NextRunAtMS != nil {
		if *job.State.NextRunAtMS > now.UnixMilli() {
			return
		}
		if shouldFireMisfire(*job, now) {
			// Preserve the persisted schedule instant. dispatchDue will coalesce
			// the downtime into exactly one execution.
			return
		}
		if job.Schedule.Kind == KindAt {
			job.Enabled = false
			job.State.NextRunAtMS = nil
			job.State.LastStatus = StatusSkipped
			job.State.LastError = "cron: missed one-shot skipped by misfire policy"
			return
		}
	}
	next, err := NextRun(job.Schedule, now)
	if err != nil {
		job.Enabled = false
		job.State.NextRunAtMS = nil
		job.State.LastStatus = StatusError
		job.State.LastError = err.Error()
		return
	}
	job.State.NextRunAtMS = next
}

func normalizeJobPolicy(job *Job) {
	if job.MisfirePolicy == "" {
		job.MisfirePolicy = MisfireFireOnce
	}
	if job.TimeoutMS < 0 {
		job.TimeoutMS = 0
	}
}

func shouldFireMisfire(job Job, now time.Time) bool {
	policy := job.MisfirePolicy
	if policy == "" {
		policy = MisfireFireOnce
	}
	if policy != MisfireFireOnce || job.State.NextRunAtMS == nil {
		return false
	}
	if job.MisfireGraceMS <= 0 {
		return true
	}
	return now.UnixMilli()-*job.State.NextRunAtMS <= job.MisfireGraceMS
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
		s.mu.Lock()
		lease := s.lease
		s.lease = nil
		s.running = false
		s.mu.Unlock()
		lease.release()
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
				select { case <-timer.C: default: }
			}
			return
		case <-s.wake:
			if !timer.Stop() {
				select { case <-timer.C: default: }
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
	if s.dirty {
		return time.Second
	}
	if len(s.active) >= s.maxConcurrent {
		return time.Second
	}
	now := time.Now().UnixMilli()
	var earliest int64
	for i := range s.store.Jobs {
		job := &s.store.Jobs[i]
		if !job.Enabled || job.State.NextRunAtMS == nil {
			continue
		}
		if _, ok := s.active[job.ID]; ok {
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
	type dueJob struct {
		idx int
		at  int64
	}
	var candidates []dueJob

	s.mu.Lock()
	if s.dirty {
		if err := s.saveLocked(); err != nil {
			s.mu.Unlock()
			return
		}
	}
	for i := range s.store.Jobs {
		job := &s.store.Jobs[i]
		if !job.Enabled || job.State.NextRunAtMS == nil || *job.State.NextRunAtMS > now {
			continue
		}
		if _, active := s.active[job.ID]; active {
			continue
		}
		candidates = append(candidates, dueJob{idx: i, at: *job.State.NextRunAtMS})
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].at < candidates[j].at })
	capacity := s.maxConcurrent - len(s.active)
	if capacity < 0 {
		capacity = 0
	}
	if len(candidates) > capacity {
		candidates = candidates[:capacity]
	}

	type launch struct {
		ctx context.Context
		job Job
	}
	launches := make([]launch, 0, len(candidates))
	for _, candidate := range candidates {
		job := &s.store.Jobs[candidate.idx]
		ctx, cancel := context.WithCancel(s.ctx)
		s.active[job.ID] = cancel
		job.State.Pending = true
		snapshot := cloneJob(*job)
		snapshot.ScheduledForMS = candidate.at
		snapshot.IdempotencyKey = idempotencyKey(snapshot.ID, candidate.at)
		launches = append(launches, launch{ctx: ctx, job: snapshot})
	}
	s.mu.Unlock()

	for _, item := range launches {
		s.wg.Add(1)
		go func(item launch) {
			defer s.wg.Done()
			s.execute(item.ctx, item.job)
		}(item)
	}
}

func (s *Service) execute(parent context.Context, snapshot Job) {
	start := time.Now()
	if snapshot.ScheduledForMS == 0 {
		snapshot.ScheduledForMS = start.UnixMilli()
	}
	if snapshot.IdempotencyKey == "" {
		snapshot.IdempotencyKey = idempotencyKey(snapshot.ID, snapshot.ScheduledForMS)
	}
	runID := snapshot.IdempotencyKey

	running := RunRecord{
		RunAtMS: start.UnixMilli(), ScheduledForMS: snapshot.ScheduledForMS,
		Status: StatusRunning, RunID: runID, IdempotencyKey: snapshot.IdempotencyKey,
	}
	_ = s.writeRunRecord(snapshot, running, "")

	timeout := defaultJobTimeout
	if snapshot.TimeoutMS > 0 {
		timeout = time.Duration(snapshot.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	ctx = withExecutionContext(ctx)

	result := RunResult{RunID: runID}
	var execErr error
	if s.executor == nil {
		execErr = errors.New("cron: no executor configured")
	} else {
		result, execErr = s.executor(ctx, snapshot, runID)
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
		RunAtMS: start.UnixMilli(), ScheduledForMS: snapshot.ScheduledForMS,
		Status: status, DurationMS: end.Sub(start).Milliseconds(), Error: errText,
		RunID: result.RunID, IdempotencyKey: snapshot.IdempotencyKey,
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
	scheduled := snapshot.ScheduledForMS
	job.State.LastRunAtMS = &last
	job.State.LastScheduledForMS = &scheduled
	job.State.LastStatus = status
	job.State.LastError = errText
	job.State.LastIdempotencyKey = snapshot.IdempotencyKey
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
		next, err := nextRecurringAfter(job.Schedule, end, snapshot.ScheduledForMS)
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

func nextRecurringAfter(schedule Schedule, after time.Time, previousScheduledMS int64) (*int64, error) {
	if schedule.Kind == KindEvery && schedule.EveryMS != nil {
		next := previousScheduledMS + *schedule.EveryMS
		if next <= after.UnixMilli() {
			missed := (after.UnixMilli()-next)/(*schedule.EveryMS) + 1
			next += missed * (*schedule.EveryMS)
		}
		return &next, nil
	}
	next, err := NextRun(schedule, after)
	if err != nil || next == nil || schedule.Kind != KindCron || previousScheduledMS == 0 {
		return next, err
	}
	previousKey := cronWallKey(schedule, time.UnixMilli(previousScheduledMS))
	for next != nil && cronWallKey(schedule, time.UnixMilli(*next)) == previousKey {
		next, err = NextRun(schedule, time.UnixMilli(*next))
		if err != nil {
			return nil, err
		}
	}
	return next, nil
}

func cronWallKey(schedule Schedule, at time.Time) string {
	loc, err := time.LoadLocation(schedule.TZ)
	if err != nil {
		loc = time.UTC
	}
	local := at.In(loc)
	return local.Format("2006-01-02T15:04")
}

func (s *Service) AddJob(job Job) (Job, error) {
	if strings.TrimSpace(job.Name) == "" {
		return Job{}, errors.New("cron: name is required")
	}
	if job.Payload.Kind == "" {
		job.Payload.Kind = PayloadAgentTurn
	}
	if strings.TrimSpace(job.Payload.Message) == "" && job.Payload.Kind == PayloadAgentTurn {
		return Job{}, errors.New("cron: message is required")
	}
	normalizeJobPolicy(&job)
	if job.MisfirePolicy != MisfireFireOnce && job.MisfirePolicy != MisfireSkip {
		return Job{}, fmt.Errorf("cron: invalid misfire policy %q", job.MisfirePolicy)
	}
	if job.MisfireGraceMS < 0 {
		return Job{}, errors.New("cron: misfire grace must be >= 0")
	}
	if job.TimeoutMS < 0 {
		return Job{}, errors.New("cron: timeout must be >= 0")
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
		s.dirty = false
		return Job{}, err
	}
	s.signal()
	return cloneJob(job), nil
}

func (s *Service) UpsertSystemJob(job Job) (Job, error) {
	if job.Payload.Kind != PayloadSystemEvent {
		return Job{}, errors.New("cron: system job requires system_event payload")
	}
	if strings.TrimSpace(job.ID) == "" {
		return Job{}, errors.New("cron: system job id is required")
	}
	normalizeJobPolicy(&job)
	if err := ValidateSchedule(job.Schedule); err != nil {
		return Job{}, err
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.indexLocked(job.ID)
	if idx >= 0 {
		existing := s.store.Jobs[idx]
		job.CreatedAtMS = existing.CreatedAtMS
		job.State = existing.State
	} else {
		job.CreatedAtMS = now.UnixMilli()
	}
	job.Enabled = true
	job.UpdatedAtMS = now.UnixMilli()
	next, err := NextRun(job.Schedule, now)
	if err != nil {
		return Job{}, err
	}
	job.State.NextRunAtMS = next
	if idx >= 0 {
		s.store.Jobs[idx] = cloneJob(job)
	} else {
		s.store.Jobs = append(s.store.Jobs, cloneJob(job))
	}
	if err := s.saveLocked(); err != nil {
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
		_, c.State.Pending = s.active[job.ID]
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
	_, out.State.Pending = s.active[id]
	return out, true
}

func (s *Service) UpdateJob(id string, update Update) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.indexLocked(id)
	if idx < 0 {
		return Job{}, ErrNotFound
	}
	if s.store.Jobs[idx].Payload.Kind == PayloadSystemEvent {
		return Job{}, ErrProtected
	}
	original := cloneJob(s.store.Jobs[idx])
	candidate := cloneJob(original)
	scheduleChanged := false
	if update.Name != nil {
		name := strings.TrimSpace(*update.Name)
		if name == "" {
			return Job{}, errors.New("cron: name cannot be empty")
		}
		candidate.Name = name
	}
	if update.Message != nil {
		message := strings.TrimSpace(*update.Message)
		if message == "" {
			return Job{}, errors.New("cron: message cannot be empty")
		}
		candidate.Payload.Message = message
	}
	if update.Schedule != nil {
		if err := ValidateSchedule(*update.Schedule); err != nil {
			return Job{}, err
		}
		candidate.Schedule = *update.Schedule
		scheduleChanged = true
	}
	if update.DeleteAfterRun != nil {
		candidate.DeleteAfterRun = *update.DeleteAfterRun
	}
	if update.TimeoutMS != nil {
		if *update.TimeoutMS < 0 {
			return Job{}, errors.New("cron: timeout must be >= 0")
		}
		candidate.TimeoutMS = *update.TimeoutMS
	}
	if update.MisfirePolicy != nil {
		policy := strings.TrimSpace(*update.MisfirePolicy)
		if policy != MisfireFireOnce && policy != MisfireSkip {
			return Job{}, fmt.Errorf("cron: invalid misfire policy %q", policy)
		}
		candidate.MisfirePolicy = policy
	}
	if update.MisfireGraceMS != nil {
		if *update.MisfireGraceMS < 0 {
			return Job{}, errors.New("cron: misfire grace must be >= 0")
		}
		candidate.MisfireGraceMS = *update.MisfireGraceMS
	}
	if update.Enabled != nil {
		candidate.Enabled = *update.Enabled
	}
	candidate.UpdatedAtMS = time.Now().UnixMilli()
	if !candidate.Enabled {
		candidate.State.NextRunAtMS = nil
	} else if scheduleChanged || update.Enabled != nil {
		next, err := NextRun(candidate.Schedule, time.Now())
		if err != nil {
			return Job{}, err
		}
		candidate.State.NextRunAtMS = next
	}

	s.store.Jobs[idx] = candidate
	if err := s.saveLocked(); err != nil {
		s.store.Jobs[idx] = original
		s.dirty = false
		return Job{}, err
	}
	s.signal()
	out := cloneJob(candidate)
	_, out.State.Pending = s.active[id]
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
	if _, active := s.active[id]; active {
		return ErrActive
	}
	original := cloneJob(s.store.Jobs[idx])
	s.store.Jobs = append(s.store.Jobs[:idx], s.store.Jobs[idx+1:]...)
	if err := s.saveLocked(); err != nil {
		s.store.Jobs = append(s.store.Jobs, Job{})
		copy(s.store.Jobs[idx+1:], s.store.Jobs[idx:])
		s.store.Jobs[idx] = original
		s.dirty = false
		return err
	}
	s.signal()
	return nil
}

func (s *Service) RunNow(id string, force bool) error {
	s.mu.Lock()
	if s.dirty {
		if err := s.saveLocked(); err != nil {
			s.mu.Unlock()
			return fmt.Errorf("cron: persist previous execution state: %w", err)
		}
	}
	idx := s.indexLocked(id)
	if idx < 0 {
		s.mu.Unlock()
		return ErrNotFound
	}
	if _, ok := s.active[id]; ok {
		s.mu.Unlock()
		return ErrActive
	}
	if len(s.active) >= s.maxConcurrent {
		s.mu.Unlock()
		return errors.New("cron: concurrency limit reached")
	}
	job := cloneJob(s.store.Jobs[idx])
	if !force && !job.Enabled {
		s.mu.Unlock()
		return errors.New("cron: job is disabled")
	}
	if err := validateBoundJob(job); err != nil {
		s.mu.Unlock()
		return err
	}
	scheduled := time.Now().UnixMilli()
	job.ScheduledForMS = scheduled
	job.IdempotencyKey = idempotencyKey(job.ID, scheduled)
	runCtx, cancel := context.WithCancel(s.ctx)
	s.active[id] = cancel
	s.store.Jobs[idx].State.Pending = true
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.execute(runCtx, job)
	}()
	return nil
}

func (s *Service) Cancel(id string) error {
	s.mu.Lock()
	cancel, ok := s.active[id]
	s.mu.Unlock()
	if !ok {
		if _, exists := s.GetJob(id); !exists {
			return ErrNotFound
		}
		return errors.New("cron: job is not running")
	}
	cancel()
	return nil
}

func (s *Service) ActiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
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
		if strings.TrimSpace(job.Payload.SystemEvent) == "" {
			return errors.New("cron: system event name is required")
		}
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
	s.store = Store{Version: 2}
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
	if s.store.Version < 2 {
		s.store.Version = 2
	}
	if s.store.Jobs == nil {
		s.store.Jobs = []Job{}
	}
	for i := range s.store.Jobs {
		normalizeJobPolicy(&s.store.Jobs[i])
	}
	return nil
}

func (s *Service) saveLocked() error {
	s.dirty = true
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
	s.dirty = false
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
		"scheduled_for_ms": run.ScheduledForMS,
		"duration_ms": run.DurationMS,
		"error": run.Error,
		"response": response,
		"idempotency_key": run.IdempotencyKey,
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	name := safeRunName(run.RunID) + ".json"
	return atomicWrite(filepath.Join(s.runsDir, name), data, 0o600)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".run-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	ok = true
	return nil
}

func (s *Service) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *Service) ReadRunRecord(runID string) (map[string]any, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("cron: run id is required")
	}
	path := filepath.Join(s.runsDir, safeRunName(runID)+".json")
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Size() > 2<<20 {
		return nil, errors.New("cron: invalid run record")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	if got, _ := record["run_id"].(string); got != runID {
		return nil, errors.New("cron: run record identity mismatch")
	}
	return record, nil
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

func idempotencyKey(jobID string, scheduledForMS int64) string {
	return fmt.Sprintf("%s:%d", jobID, scheduledForMS)
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
