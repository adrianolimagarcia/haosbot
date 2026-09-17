package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// AgentCard mirrors the A2A discovery card served at /.well-known/agent-card.json
type AgentCard struct {
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	URL          string            `json:"url"`
	Version      string            `json:"version"`
	Capabilities AgentCapabilities `json:"capabilities"`
	Skills       []AgentSkill      `json:"skills"`
}

type AgentCapabilities struct {
	Streaming         bool `json:"streaming"`
	PushNotifications bool `json:"pushNotifications"`
}

type AgentSkill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// JSONRPCRequest represents an incoming JSON-RPC 2.0 message
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// JSONRPCResponse represents an outgoing JSON-RPC 2.0 message
type JSONRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      any           `json:"id"`
	Result  any           `json:"result,omitempty"`
	Error   *JSONRPCError `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Task represents an A2A Task object
type Task struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"` // "submitted", "working", "completed", "failed"
	Input     string    `json:"input"`
	Output    string    `json:"output,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Task retention policy.
//
// A2A tasks are transient: a peer submits one, reads the reply, and may poll
// tasks/get for a short while afterwards. Nothing ever removed an entry, so the
// store grew without bound — one entry per task, each holding the full input and
// output text, kept for the life of the process. The policy below bounds it:
//
//   - a terminal task (completed/failed) is retained for taskRetentionTTL and
//     then swept;
//   - the store is trimmed back below maxRetainedTasks whenever it exceeds that
//     cap, so a burst that arrives faster than the TTL can expire is bounded
//     oldest-first even before any TTL elapses. The trim is amortised, so
//     concurrent publishes can overshoot the cap by the number of tasks being
//     published at that instant before the next trim pulls the store back;
//   - sweeping is opportunistic (amortised on publish) rather than ticker
//     driven, so the handler owns no goroutine and needs no Close;
//   - the TTL pass runs at most once per taskSweepInterval, so a terminal task
//     lives at least taskRetentionTTL and at most taskRetentionTTL +
//     taskSweepInterval (the hard cap can evict it earlier).
//
// The values are deliberately not configurable: the reference has no A2A
// endpoint and the config schema has no key for them, so inventing one would
// create a second source of truth for a bound whose only job is to stop
// unbounded growth.
const (
	taskRetentionTTL  = 10 * time.Minute
	taskSweepInterval = time.Minute
	maxRetainedTasks  = 1024
	// taskTrimTarget is the low-water mark a cap trim aims for, so a burst costs
	// one store walk per batch instead of one walk per task.
	taskTrimTarget = maxRetainedTasks * 7 / 8
)

// Handler coordinates A2A endpoints and task execution
type Handler struct {
	cfg   *config.Config
	loop  *agent.Loop
	tasks sync.Map

	// retained counts the live entries in tasks, and lastSweepNanos records the
	// last TTL pass. Both exist because sync.Map has no length and the cap has
	// to be enforced without walking the map on every request.
	retained       atomic.Int64
	lastSweepNanos atomic.Int64
}

// defaultAPIPort is the loader's own default for api.port
// (internal/config/schema.go defaultConfigSkeleton, 8900), resolved once and
// lazily. Taking it from the same source the loader uses — instead of a private
// literal — is what stops the fallback from drifting from the port the gateway
// actually binds and advertising an unreachable URL.
var defaultAPIPort = sync.OnceValue(func() int { return config.DefaultConfig().API.Port })

func NewHandler(cfg *config.Config, loop *agent.Loop) *Handler {
	return &Handler{
		cfg:  cfg,
		loop: loop,
	}
}

// terminal reports whether a task reached a final state.
func (t Task) terminal() bool {
	return t.Status == "completed" || t.Status == "failed"
}

// publish stores an immutable snapshot of task and keeps the store bounded.
//
// Snapshots are copies: once published, a *Task is never mutated again. That is
// what lets handleTaskGet serialise a task without a lock and without ever
// observing a half-written one (a completed status carrying the previous empty
// output, or a working status with a stale updatedAt).
func (h *Handler) publish(task Task) {
	snapshot := task
	if _, loaded := h.tasks.Swap(task.ID, &snapshot); !loaded {
		h.retained.Add(1)
	}
	h.sweepTasks(time.Now())
}

func (h *Handler) sweepTasks(now time.Time) {
	// The TTL pass walks the whole store, so it runs at most once per interval.
	// The hard cap below is enforced on every publish regardless.
	last := h.lastSweepNanos.Load()
	if last == 0 || now.UnixNano()-last >= int64(taskSweepInterval) {
		if h.lastSweepNanos.CompareAndSwap(last, now.UnixNano()) {
			h.evictExpiredTasks(now)
		}
	}
	h.enforceTaskCap()
}

func (h *Handler) evictExpiredTasks(now time.Time) {
	h.tasks.Range(func(key, value any) bool {
		task, ok := value.(*Task)
		if !ok {
			return true
		}
		if task.terminal() && now.Sub(task.UpdatedAt) > taskRetentionTTL {
			h.deleteTask(key)
		}
		return true
	})
}

// enforceTaskCap trims the store back to taskTrimTarget once it exceeds its hard
// cap, in a single walk: terminal tasks go first, least recently updated first,
// and in-flight tasks are dropped only when every retained task is still
// running. A peer that loses its entry simply gets "Task not found" — the
// response of the send that owns the task is unaffected.
func (h *Handler) enforceTaskCap() {
	if h.retained.Load() <= maxRetainedTasks {
		return
	}

	type candidate struct {
		key      any
		updated  time.Time
		terminal bool
	}
	candidates := make([]candidate, 0, maxRetainedTasks+1)
	h.tasks.Range(func(key, value any) bool {
		task, ok := value.(*Task)
		if !ok {
			return true
		}
		candidates = append(candidates, candidate{key: key, updated: task.UpdatedAt, terminal: task.terminal()})
		return true
	})

	slices.SortFunc(candidates, func(a, b candidate) int {
		if a.terminal != b.terminal {
			if a.terminal {
				return -1
			}
			return 1
		}
		return a.updated.Compare(b.updated)
	})

	for _, victim := range candidates {
		if h.retained.Load() <= taskTrimTarget {
			return
		}
		h.deleteTask(victim.key)
	}
}

// deleteTask removes an entry, keeping the retained count exact when several
// sweepers race for the same key.
func (h *Handler) deleteTask(key any) {
	if _, loaded := h.tasks.LoadAndDelete(key); loaded {
		h.retained.Add(-1)
	}
}

// RegisterRoutes registers the A2A protocol routes on the provided mux
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// A. Discovery Endpoint
	mux.HandleFunc("/.well-known/agent-card.json", h.handleAgentCard)

	// B. JSON-RPC 2.0 Task Endpoint
	mux.HandleFunc("/a2a", h.handleJSONRPC)
}

func (h *Handler) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	host := h.cfg.API.Host
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	port := h.cfg.API.Port
	if port <= 0 {
		port = defaultAPIPort()
	}

	card := AgentCard{
		Name:        "haosbot",
		Description: "Autonomous lightweight infrastructure, shell execution and Python/SQLite agent powered by haosbot",
		URL:         fmt.Sprintf("http://%s:%d/a2a", host, port),
		Version:     "1.0.0",
		Capabilities: AgentCapabilities{
			Streaming:         false,
			PushNotifications: false,
		},
		Skills: []AgentSkill{
			{
				ID:          "exec",
				Name:        "Terminal Command Execution",
				Description: "Execute shell commands, monitor system metrics, systemd and disk usage",
			},
			{
				ID:          "python_exec",
				Name:        "Python SQLite Runner",
				Description: "Execute isolated Python scripts with SQLite, JSON and networking support",
			},
			{
				ID:          "file_ops",
				Name:        "Workspace File Operations",
				Description: "Read, write and edit files safely inside the agent workspace",
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(card)
}

func (h *Handler) handleJSONRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{
			JSONRPC: "2.0",
			Error:   &JSONRPCError{Code: -32700, Message: "Parse error"},
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")

	switch req.Method {
	case "tasks/send", "tasks/create":
		h.handleTaskSend(r.Context(), w, req)
	case "tasks/get":
		h.handleTaskGet(w, req)
	default:
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &JSONRPCError{Code: -32601, Message: fmt.Sprintf("Method not found: %s", req.Method)},
		})
	}
}

// requestTimeout is the upper bound applied to one A2A task.
//
// It mirrors the reference's per-request bound, `api.timeout`
// (config/schema.py:338 "Per-request timeout in seconds", default 120.0), which
// the reference applies to every API request (api/server.py:478-494). The
// default is therefore the 120s this handler used to hardcode.
func (h *Handler) requestTimeout() time.Duration {
	const fallback = 120 * time.Second
	if h == nil || h.cfg == nil || h.cfg.API.Timeout <= 0 {
		return fallback
	}
	return time.Duration(float64(h.cfg.API.Timeout) * float64(time.Second))
}

// handleTaskSend runs one task. ctx is the INCOMING REQUEST's context: deriving
// the task from context.Background() meant a client that disconnected (or a
// proxy that timed out) left the agent calling the model and holding its
// concurrency slot until the bound elapsed, and broke cancellation
// propagation. The bound is kept, so a peer that stays connected still cannot
// pin a task open forever.
func (h *Handler) handleTaskSend(ctx context.Context, w http.ResponseWriter, req JSONRPCRequest) {
	var params struct {
		Message struct {
			Text string `json:"text"`
		} `json:"message"`
		Input string `json:"input"`
	}

	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}

	inputText := strings.TrimSpace(params.Message.Text)
	if inputText == "" {
		inputText = strings.TrimSpace(params.Input)
	}

	if inputText == "" {
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &JSONRPCError{Code: -32602, Message: "Invalid params: input or message.text is required"},
		})
		return
	}

	taskID := fmt.Sprintf("task-%d", time.Now().UnixNano())
	// task is a local value: every state transition publishes a fresh snapshot,
	// so the copy a reader holds is never written to again.
	task := Task{
		ID:        taskID,
		Status:    "working",
		Input:     inputText,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	h.publish(task)

	// Execute via Agent Loop if available
	var finalOutput string
	if h.loop != nil {
		inbound := core.InboundMessage{
			Channel:   "a2a",
			SenderID:  "a2a_peer",
			ChatID:    taskID,
			Content:   inputText,
			Timestamp: time.Now(),
			Metadata:  map[string]any{"source": "a2a_protocol"},
		}

		ctx, cancel := context.WithTimeout(ctx, h.requestTimeout())
		defer cancel()

		out, err := h.loop.ProcessMessage(ctx, inbound)
		if err != nil {
			task.Status = "failed"
			task.Output = err.Error()
			task.UpdatedAt = time.Now()
			h.publish(task)
			_ = json.NewEncoder(w).Encode(JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &JSONRPCError{Code: -32000, Message: err.Error()},
			})
			return
		}
		if out != nil {
			finalOutput = out.Content
		}
	} else {
		finalOutput = fmt.Sprintf("Haosbot received: %s (agent loop not attached)", inputText)
	}

	task.Status = "completed"
	task.Output = finalOutput
	task.UpdatedAt = time.Now()
	h.publish(task)

	// The reply carries the same snapshot that was just published, so the caller
	// and a concurrent tasks/get can never disagree about this task id.
	_ = json.NewEncoder(w).Encode(JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  task,
	})
}

func (h *Handler) handleTaskGet(w http.ResponseWriter, req JSONRPCRequest) {
	var params struct {
		ID string `json:"id"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}

	if params.ID == "" {
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &JSONRPCError{Code: -32602, Message: "Invalid params: id is required"},
		})
		return
	}

	val, ok := h.tasks.Load(params.ID)
	if !ok {
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &JSONRPCError{Code: -32004, Message: "Task not found"},
		})
		return
	}

	_ = json.NewEncoder(w).Encode(JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  val,
	})
}
