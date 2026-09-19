package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/a2a"
	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/command"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	cronruntime "github.com/adrianolimagarcia/nanobot-go/internal/cron"
	triggersruntime "github.com/adrianolimagarcia/nanobot-go/internal/triggers"
	"github.com/adrianolimagarcia/nanobot-go/internal/netpolicy"
	"github.com/adrianolimagarcia/nanobot-go/internal/observability"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/session"
)

type Server struct {
	cfg       *config.Config
	server    *http.Server
	provider  provider.Provider
	cmdRouter *command.Router

	// loop, bus and dataDir are the runtime handles the request path and /readyz
	// need and cannot derive from the config: the agent loop, the message bus the
	// channels publish into, and the data directory the session store writes
	// under. All three are late-injected seams (the gateway assembles them after
	// the api package is constructed — cmd/haosbot/runtime.go), and all three are
	// atomics so a probe or a request running concurrently with a setter cannot
	// race with it. An unset handle is reported as a failing check rather than
	// silently passing.
	loop    atomic.Pointer[agent.Loop]
	bus     atomic.Pointer[bus.Bus]
	dataDir atomic.Pointer[string]
	metrics atomic.Pointer[observability.Registry]
	sessionStore atomic.Pointer[session.Store]
	scheduler atomic.Pointer[cronruntime.Service]
	triggers atomic.Pointer[triggersruntime.Service]
	webuiMu sync.Mutex

	// notReady is the process-owned readiness override read by /readyz
	// (see ready.go). The zero value means "no override".
	notReady atomic.Bool
}

func NewServer(cfg *config.Config, prov provider.Provider, loop *agent.Loop) *Server {
	s := &Server{
		cfg:       cfg,
		provider:  prov,
		cmdRouter: command.NewRouter(),
	}
	s.loop.Store(loop)
	return s
}

func (s *Server) SetLoop(loop *agent.Loop) {
	s.loop.Store(loop)
}

// SetBus attaches the message bus whose consumers /readyz reports on. The
// gateway owns the bus (it also hands it to the channel manager and the agent
// loop), so it is injected here instead of being rebuilt: a second bus would be
// a second source of truth for the same queue.
func (s *Server) SetBus(b *bus.Bus) { s.bus.Store(b) }

// SetDataDir records the directory the gateway writes runtime data under — the
// parent of the session store's root (cmd/haosbot/runtime.go builds the root as
// <data dir>/sessions). /readyz proves it accepts a write.
func (s *Server) SetDataDir(dir string) { s.dataDir.Store(&dir) }

// SetMetrics attaches the allocation-light diagnostics registry owned by the
// runtime. The endpoint is injected instead of recreated here so it observes
// the same projection workers and memory fabric as the gateway.
func (s *Server) SetMetrics(metrics *observability.Registry) { s.metrics.Store(metrics) }

// SetSessionStore exposes the runtime's canonical session store to the WebUI
// control center. It is the same store used by the agent loop, never a second
// copy of session state.
func (s *Server) SetSessionStore(store *session.Store) { s.sessionStore.Store(store) }

// SetScheduler attaches the gateway-owned automation scheduler used by the
// WebUI management API. The scheduler remains inactive in one-shot/chat modes.
func (s *Server) SetScheduler(scheduler *cronruntime.Service) { s.scheduler.Store(scheduler) }

func (s *Server) SetTriggerService(service *triggersruntime.Service) { s.triggers.Store(service) }

func (s *Server) checkAuth(r *http.Request) bool {
	apiKey := s.cfg.API.APIKey
	if apiKey == "" {
		return true // securityMiddleware restricts no-key mode to loopback peers.
	}

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	return subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) == 1
}

func (s *Server) Start(addr string) error {
	if err := s.validateBindAddr(addr); err != nil {
		return err
	}

	mux := http.NewServeMux()
	s.registerWebUI(mux)
	s.registerReady(mux)
	a2aHandler := a2a.NewHandler(s.cfg, s.loop.Load())
	a2aHandler.RegisterRoutes(mux)

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","runtime":"haosbot"}`))
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if !s.checkAuth(r) {
			writeUnauthorized(w)
			return
		}
		metrics := s.metrics.Load()
		if metrics == nil {
			writeAPIError(w, http.StatusServiceUnavailable, "server_error", "metrics_unavailable", "Runtime metrics are not configured")
			return
		}
		metrics.ServeHTTP(w, r)
	})

	mux.HandleFunc("/api/fetch-models", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			BaseURL string `json:"baseUrl"`
			APIKey  string `json:"apiKey"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		baseURL := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		targetURL := baseURL + "/models"
		policy := netpolicy.Policy{Allowlist: s.cfg.Tools.SSRFWhitelist}
		if _, err := netpolicy.ValidateURL(r.Context(), targetURL, policy); err != nil {
			http.Error(w, fmt.Sprintf("outbound URL blocked: %v", err), http.StatusForbidden)
			return
		}

		httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, targetURL, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		apiKey := strings.TrimSpace(req.APIKey)
		if apiKey == "" {
			apiKey = "no-key"
		}
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		client := netpolicy.NewClient(10*time.Second, policy)
		resp, err := client.Do(httpReq)
		if err != nil {
			http.Error(w, fmt.Sprintf("Falha ao conectar: %v", err), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		limit := netpolicy.MaxResponseBytes(policy)
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			http.Error(w, fmt.Sprintf("Falha ao ler resposta: %v", err), http.StatusBadGateway)
			return
		}
		if int64(len(body)) > limit {
			http.Error(w, "Remote response exceeded size limit", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	})

	mux.HandleFunc("/api/restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"restarting","message":"Reiniciando haosbot gateway..."}`))
		go func() {
			time.Sleep(500 * time.Millisecond)
			os.Exit(0)
		}()
	})

	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		configPath := configTargetPath(s.cfg)

		if r.Method == http.MethodGet {
			view, err := redactedConfig(s.cfg)
			if err != nil {
				http.Error(w, fmt.Sprintf("Config view error: %v", err), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(view)
			return
		}

		if r.Method == http.MethodPost {
			var patch map[string]any
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
				return
			}
			if err := saveConfigPatch(s.cfg, configPath, patch); err != nil {
				http.Error(w, fmt.Sprintf("Invalid configuration: %v", err), http.StatusBadRequest)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":          "saved",
				"restartRequired": true,
				"message":         "Configurações salvas; reinicie o gateway para aplicar todas as mudanças.",
			})
			return
		}

		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})

	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !s.checkAuth(r) {
			writeUnauthorized(w)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		modelName := s.cfg.Agents.Defaults.Model
		if modelName == "" {
			modelName = "deepseek-chat"
		}
		resp := map[string]any{
			"object": "list",
			"data": []map[string]any{
				{
					"id":       modelName,
					"object":   "model",
					"created":  time.Now().Unix(),
					"owned_by": "haosbot",
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	// OpenAI-compatible requests are intentionally stateless. Stateful HAOSBot
	// turns (tools, memory, sessions) use /api/agent/turn instead.
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)

	s.server = &http.Server{
		Addr:              addr,
		Handler:           s.securityMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	return s.server.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.server != nil {
		return s.server.Shutdown(ctx)
	}
	return nil
}
