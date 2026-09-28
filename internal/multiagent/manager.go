package multiagent

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

type TaskStatus string

const (
	TaskSubmitted TaskStatus = "submitted"
	TaskWorking   TaskStatus = "working"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
	TaskCanceled  TaskStatus = "canceled"
)

type Profile struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Role              string   `json:"role"`
	Instructions      string   `json:"instructions"`
	Model             string   `json:"model,omitempty"`
	Provider          string   `json:"provider,omitempty"`
	Endpoint          string   `json:"endpoint,omitempty"`
	TokenEnv          string   `json:"token_env,omitempty"`
	ToolAllow         []string `json:"tool_allow,omitempty"`
	MemoryScope       string   `json:"memory_scope"`
	MaxTokens         int      `json:"max_tokens,omitempty"`
	MaxToolIterations int      `json:"max_tool_iterations,omitempty"`
	Enabled           bool     `json:"enabled"`
}

type Limits struct {
	MaxDepth         int           `json:"max_depth"`
	MaxParallel      int           `json:"max_parallel"`
	MaxChildren      int           `json:"max_children"`
	MaxTasks         int           `json:"max_tasks"`
	TaskTimeout      time.Duration `json:"-"`
	Retention        time.Duration `json:"-"`
	TaskTimeoutSecs  int           `json:"task_timeout_seconds"`
	RetentionMinutes int           `json:"retention_minutes"`
}

type Task struct {
	ID           string     `json:"id"`
	ParentTaskID string     `json:"parent_task_id,omitempty"`
	RootTaskID   string     `json:"root_task_id"`
	TraceID      string     `json:"trace_id"`
	AgentID      string     `json:"agent_id"`
	RequestedBy  string     `json:"requested_by"`
	Depth        int        `json:"depth"`
	Prompt       string     `json:"prompt"`
	Status       TaskStatus `json:"status"`
	Result       string     `json:"result,omitempty"`
	Error        string     `json:"error,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	StartedAt    time.Time  `json:"started_at,omitempty"`
	CompletedAt  time.Time  `json:"completed_at,omitempty"`
	Deadline     time.Time  `json:"deadline,omitempty"`
	Ancestry     []string   `json:"ancestry,omitempty"`
}

type Event struct {
	Type string `json:"type"`
	Task Task   `json:"task"`
}

type Executor interface {
	Execute(context.Context, Profile, Task) (string, error)
}

type ExecutorFunc func(context.Context, Profile, Task) (string, error)

func (f ExecutorFunc) Execute(ctx context.Context, p Profile, t Task) (string, error) {
	return f(ctx, p, t)
}

type DelegateRequest struct {
	AgentID        string
	Prompt         string
	Wait           bool
	Timeout        time.Duration
	RequestedBy    string
	Detach         bool
}

type executionContextKey struct{}
type executionLeaseContextKey struct{}

type executionLease struct {
	manager *Manager
	mu      sync.Mutex
	held    bool
}

func (l *executionLease) acquire(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held {
		return nil
	}
	select {
	case l.manager.sem <- struct{}{}:
		l.held = true
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *executionLease) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.held {
		return
	}
	<-l.manager.sem
	l.held = false
}

// withYieldedSlot prevents nested synchronous delegation from deadlocking when
// every worker slot is occupied by a parent waiting for its child.
func (m *Manager) withYieldedSlot(ctx context.Context, fn func() (*Task, error)) (*Task, error) {
	lease, _ := ctx.Value(executionLeaseContextKey{}).(*executionLease)
	if lease == nil || lease.manager != m {
		return fn()
	}
	lease.release()
	result, runErr := fn()
	if err := lease.acquire(ctx); err != nil && runErr == nil {
		runErr = err
	}
	return result, runErr
}

type ExecutionMeta struct {
	TaskID     string
	RootTaskID string
	TraceID    string
	AgentID    string
	Depth      int
	Ancestry   []string
}

func WithExecutionMeta(ctx context.Context, meta ExecutionMeta) context.Context {
	meta.Ancestry = append([]string(nil), meta.Ancestry...)
	return context.WithValue(ctx, executionContextKey{}, meta)
}

func ExecutionMetaFromContext(ctx context.Context) (ExecutionMeta, bool) {
	meta, ok := ctx.Value(executionContextKey{}).(ExecutionMeta)
	if !ok {
		return ExecutionMeta{}, false
	}
	meta.Ancestry = append([]string(nil), meta.Ancestry...)
	return meta, true
}

type Manager struct {
	mu          sync.RWMutex
	persistMu   sync.Mutex
	profiles    map[string]Profile
	tasks       map[string]*Task
	cancels     map[string]context.CancelFunc
	done        map[string]chan struct{}
	limits      Limits
	executor    Executor
	sem         chan struct{}
	persistPath string
	sink        func(Event)
}

func NewManager(profiles []Profile, limits Limits, executor Executor, persistPath string, sink func(Event)) (*Manager, error) {
	if executor == nil {
		return nil, errors.New("multiagent: executor is required")
	}
	limits = normalizeLimits(limits)
	m := &Manager{
		profiles:    make(map[string]Profile),
		tasks:       make(map[string]*Task),
		cancels:     make(map[string]context.CancelFunc),
		done:        make(map[string]chan struct{}),
		limits:      limits,
		executor:    executor,
		sem:         make(chan struct{}, limits.MaxParallel),
		persistPath: strings.TrimSpace(persistPath),
		sink:        sink,
	}
	for _, profile := range profiles {
		profile.ID = strings.TrimSpace(profile.ID)
		if profile.ID == "" {
			return nil, errors.New("multiagent: profile id is required")
		}
		if !validProfileID(profile.ID) {
			return nil, fmt.Errorf("multiagent: invalid profile id %q", profile.ID)
		}
		if profile.Name == "" {
			profile.Name = profile.ID
		}
		if profile.MemoryScope == "" {
			profile.MemoryScope = "project"
		}
		if _, exists := m.profiles[profile.ID]; exists {
			return nil, fmt.Errorf("multiagent: duplicate profile %q", profile.ID)
		}
		m.profiles[profile.ID] = profile
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	m.cleanup()
	return m, nil
}

func normalizeLimits(l Limits) Limits {
	if l.MaxDepth <= 0 {
		l.MaxDepth = 3
	}
	if l.MaxParallel <= 0 {
		l.MaxParallel = 4
	}
	if l.MaxChildren <= 0 {
		l.MaxChildren = 8
	}
	if l.MaxTasks <= 0 {
		l.MaxTasks = 1024
	}
	if l.TaskTimeout <= 0 {
		if l.TaskTimeoutSecs > 0 {
			l.TaskTimeout = time.Duration(l.TaskTimeoutSecs) * time.Second
		} else {
			l.TaskTimeout = 5 * time.Minute
		}
	}
	if l.Retention <= 0 {
		if l.RetentionMinutes > 0 {
			l.Retention = time.Duration(l.RetentionMinutes) * time.Minute
		} else {
			l.Retention = time.Hour
		}
	}
	l.TaskTimeoutSecs = int(l.TaskTimeout / time.Second)
	l.RetentionMinutes = int(l.Retention / time.Minute)
	return l
}

func (m *Manager) Limits() Limits { return m.limits }

func (m *Manager) Profiles() []Profile {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Profile, 0, len(m.profiles))
	for _, p := range m.profiles {
		p.ToolAllow = append([]string(nil), p.ToolAllow...)
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (m *Manager) Profile(id string) (Profile, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.profiles[id]
	p.ToolAllow = append([]string(nil), p.ToolAllow...)
	return p, ok
}

func (m *Manager) Delegate(ctx context.Context, req DelegateRequest) (*Task, error) {
	req.AgentID = strings.TrimSpace(req.AgentID)
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.AgentID == "" {
		return nil, errors.New("multiagent: agent is required")
	}
	if req.Prompt == "" {
		return nil, errors.New("multiagent: prompt is required")
	}
	profile, ok := m.Profile(req.AgentID)
	if !ok {
		return nil, fmt.Errorf("multiagent: unknown agent %q", req.AgentID)
	}
	if !profile.Enabled {
		return nil, fmt.Errorf("multiagent: agent %q is disabled", req.AgentID)
	}

	meta, nested := ExecutionMetaFromContext(ctx)
	depth := 1
	parentID := ""
	rootID := ""
	traceID := newID("trace")
	requestedBy := strings.TrimSpace(req.RequestedBy)
	ancestry := []string{}
	if requestedBy == "" {
		requestedBy = "commander"
	}
	if nested {
		depth = meta.Depth + 1
		parentID = meta.TaskID
		rootID = meta.RootTaskID
		if meta.TraceID != "" {
			traceID = meta.TraceID
		}
		if meta.AgentID != "" {
			requestedBy = meta.AgentID
		}
		ancestry = append(ancestry, meta.Ancestry...)
		if meta.AgentID != "" {
			ancestry = append(ancestry, meta.AgentID)
		}
	}
	if depth > m.limits.MaxDepth {
		return nil, fmt.Errorf("multiagent: max delegation depth %d exceeded", m.limits.MaxDepth)
	}
	for _, id := range ancestry {
		if id == req.AgentID {
			return nil, fmt.Errorf("multiagent: delegation cycle rejected (%s already in ancestry)", req.AgentID)
		}
	}

	now := time.Now().UTC()
	timeout := req.Timeout
	if timeout <= 0 || timeout > m.limits.TaskTimeout {
		timeout = m.limits.TaskTimeout
	}
	task := &Task{
		ID:          newID("task"),
		ParentTaskID: parentID,
		TraceID:     traceID,
		AgentID:     req.AgentID,
		RequestedBy: requestedBy,
		Depth:       depth,
		Prompt:      req.Prompt,
		Status:      TaskSubmitted,
		CreatedAt:   now,
		Deadline:    now.Add(timeout),
		Ancestry:    append([]string(nil), ancestry...),
	}
	if rootID == "" {
		task.RootTaskID = task.ID
	} else {
		task.RootTaskID = rootID
	}

	m.mu.Lock()
	if parentID != "" {
		children := 0
		for _, existing := range m.tasks {
			if existing.ParentTaskID == parentID {
				children++
			}
		}
		if children >= m.limits.MaxChildren {
			m.mu.Unlock()
			return nil, fmt.Errorf("multiagent: max children %d reached for task %s", m.limits.MaxChildren, parentID)
		}
	}
	m.cleanupLocked(now)
	if len(m.tasks) >= m.limits.MaxTasks {
		m.mu.Unlock()
		return nil, fmt.Errorf("multiagent: task store capacity %d reached", m.limits.MaxTasks)
	}
	m.tasks[task.ID] = task
	m.done[task.ID] = make(chan struct{})
	m.mu.Unlock()
	m.persist()
	m.emit("submitted", *task)

	baseCtx := ctx
	if req.Detach {
		baseCtx = context.WithoutCancel(ctx)
	}
	runCtx, cancel := context.WithDeadline(baseCtx, task.Deadline)
	m.mu.Lock()
	m.cancels[task.ID] = cancel
	m.mu.Unlock()
	go m.run(runCtx, cancel, task.ID)

	if req.Wait {
		if waited, err := m.Wait(ctx, task.ID); err == nil {
			return waited, nil
		} else {
			return waited, err
		}
	}
	out, _ := m.Get(task.ID)
	return out, nil
}

func (m *Manager) run(ctx context.Context, cancel context.CancelFunc, id string) {
	defer cancel()
	defer func() {
		m.mu.Lock()
		delete(m.cancels, id)
		m.mu.Unlock()
	}()

	lease := &executionLease{manager: m}
	if err := lease.acquire(ctx); err != nil {
		m.finish(id, TaskCanceled, "", err)
		return
	}
	defer lease.release()
	ctx = context.WithValue(ctx, executionLeaseContextKey{}, lease)

	m.mu.Lock()
	task := m.tasks[id]
	if task == nil {
		m.mu.Unlock()
		return
	}
	task.Status = TaskWorking
	task.StartedAt = time.Now().UTC()
	profile := m.profiles[task.AgentID]
	snapshot := *cloneTask(task)
	m.mu.Unlock()
	m.persist()
	m.emit("working", snapshot)

	meta := ExecutionMeta{
		TaskID: task.ID, RootTaskID: task.RootTaskID, TraceID: task.TraceID,
		AgentID: task.AgentID, Depth: task.Depth, Ancestry: append([]string(nil), task.Ancestry...),
	}
	result, err := m.executor.Execute(WithExecutionMeta(ctx, meta), profile, snapshot)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			m.finish(id, TaskCanceled, result, firstErr(err, ctx.Err()))
			return
		}
		m.finish(id, TaskFailed, result, err)
		return
	}
	m.finish(id, TaskCompleted, result, nil)
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func (m *Manager) finish(id string, status TaskStatus, result string, err error) {
	m.mu.Lock()
	task := m.tasks[id]
	if task == nil {
		m.mu.Unlock()
		return
	}
	task.Status = status
	task.Result = result
	task.CompletedAt = time.Now().UTC()
	if err != nil {
		task.Error = err.Error()
	}
	snapshot := *cloneTask(task)
	if ch := m.done[id]; ch != nil {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
	m.mu.Unlock()
	m.persist()
	m.emit(string(status), snapshot)
}

func (m *Manager) Wait(ctx context.Context, id string) (*Task, error) {
	m.mu.RLock()
	task := m.tasks[id]
	ch := m.done[id]
	if task == nil {
		m.mu.RUnlock()
		return nil, fmt.Errorf("multiagent: task %q not found", id)
	}
	if isTerminal(task.Status) {
		out := cloneTask(task)
		m.mu.RUnlock()
		return out, taskError(out)
	}
	m.mu.RUnlock()
	if ch == nil {
		return nil, fmt.Errorf("multiagent: task %q has no completion signal", id)
	}
	select {
	case <-ch:
		out, ok := m.Get(id)
		if !ok {
			return nil, fmt.Errorf("multiagent: task %q disappeared", id)
		}
		return out, taskError(out)
	case <-ctx.Done():
		out, _ := m.Get(id)
		return out, ctx.Err()
	}
}

func taskError(t *Task) error {
	if t == nil {
		return nil
	}
	if t.Status == TaskFailed || t.Status == TaskCanceled {
		if t.Error != "" {
			return errors.New(t.Error)
		}
		return fmt.Errorf("multiagent: task ended as %s", t.Status)
	}
	return nil
}

func (m *Manager) Cancel(id string) (*Task, error) {
	m.mu.RLock()
	task := m.tasks[id]
	cancel := m.cancels[id]
	if task == nil {
		m.mu.RUnlock()
		return nil, fmt.Errorf("multiagent: task %q not found", id)
	}
	terminal := isTerminal(task.Status)
	snapshot := cloneTask(task)
	m.mu.RUnlock()
	if terminal {
		return snapshot, nil
	}
	if cancel != nil {
		cancel()
	}
	out, _ := m.Get(id)
	return out, nil
}

func (m *Manager) Get(id string) (*Task, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	task, ok := m.tasks[id]
	if !ok {
		return nil, false
	}
	return cloneTask(task), true
}

func (m *Manager) List(limit int) []*Task {
	m.cleanup()
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Task, 0, len(m.tasks))
	for _, task := range m.tasks {
		out = append(out, cloneTask(task))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (m *Manager) cleanup() {
	m.mu.Lock()
	m.cleanupLocked(time.Now().UTC())
	m.mu.Unlock()
}

func (m *Manager) cleanupLocked(now time.Time) {
	cutoff := now.Add(-m.limits.Retention)
	for id, task := range m.tasks {
		if isTerminal(task.Status) && !task.CompletedAt.IsZero() && task.CompletedAt.Before(cutoff) {
			delete(m.tasks, id)
			delete(m.cancels, id)
			delete(m.done, id)
		}
	}
	if len(m.tasks) <= m.limits.MaxTasks {
		return
	}
	type row struct{ id string; at time.Time }
	rows := make([]row, 0, len(m.tasks))
	for id, task := range m.tasks {
		if isTerminal(task.Status) {
			rows = append(rows, row{id: id, at: task.CompletedAt})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].at.Before(rows[j].at) })
	for _, row := range rows {
		if len(m.tasks) <= m.limits.MaxTasks {
			break
		}
		delete(m.tasks, row.id)
		delete(m.done, row.id)
		delete(m.cancels, row.id)
	}
}

func isTerminal(status TaskStatus) bool {
	return status == TaskCompleted || status == TaskFailed || status == TaskCanceled
}

func cloneTask(task *Task) *Task {
	if task == nil {
		return nil
	}
	out := *task
	out.Ancestry = append([]string(nil), task.Ancestry...)
	return &out
}

func (m *Manager) emit(kind string, task Task) {
	if m.sink != nil {
		m.sink(Event{Type: kind, Task: *cloneTask(&task)})
	}
}

func (m *Manager) persist() {
	if m.persistPath == "" {
		return
	}
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	m.mu.RLock()
	tasks := make([]*Task, 0, len(m.tasks))
	for _, task := range m.tasks {
		tasks = append(tasks, cloneTask(task))
	}
	m.mu.RUnlock()
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.Before(tasks[j].CreatedAt) })
	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.persistPath), 0o700); err != nil {
		return
	}
	tmp := m.persistPath + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, m.persistPath)
}

func (m *Manager) load() error {
	if m.persistPath == "" {
		return nil
	}
	data, err := os.ReadFile(m.persistPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("multiagent: load tasks: %w", err)
	}
	var tasks []*Task
	if err := json.Unmarshal(data, &tasks); err != nil {
		return fmt.Errorf("multiagent: decode tasks: %w", err)
	}
	now := time.Now().UTC()
	for _, task := range tasks {
		if task == nil || task.ID == "" {
			continue
		}
		if task.Status == TaskSubmitted || task.Status == TaskWorking {
			task.Status = TaskFailed
			task.Error = "runtime restarted before task completed"
			task.CompletedAt = now
		}
		m.tasks[task.ID] = cloneTask(task)
		ch := make(chan struct{})
		if isTerminal(task.Status) {
			close(ch)
		}
		m.done[task.ID] = ch
	}
	return nil
}

func newID(prefix string) string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return prefix + "-" + hex.EncodeToString(raw[:])
	}
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func validProfileID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for i, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			if i == 0 && (r == '-' || r == '_') {
				return false
			}
			continue
		}
		return false
	}
	return true
}
