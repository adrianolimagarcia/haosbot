package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
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

// Handler coordinates A2A endpoints and task execution
type Handler struct {
	cfg   *config.Config
	loop  *agent.Loop
	tasks sync.Map
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
	task := &Task{
		ID:        taskID,
		Status:    "working",
		Input:     inputText,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	h.tasks.Store(taskID, task)

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
