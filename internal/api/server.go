package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/a2a"
	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/command"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/netpolicy"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
)

type Server struct {
	cfg       *config.Config
	server    *http.Server
	provider  provider.Provider
	cmdRouter *command.Router
	loop      *agent.Loop
}

func NewServer(cfg *config.Config, prov provider.Provider, loop *agent.Loop) *Server {
	return &Server{
		cfg:       cfg,
		provider:  prov,
		cmdRouter: command.NewRouter(),
		loop:      loop,
	}
}

func (s *Server) SetLoop(loop *agent.Loop) {
	s.loop = loop
}

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
	a2aHandler := a2a.NewHandler(s.cfg, s.loop)
	a2aHandler.RegisterRoutes(mux)

	// GET /health
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","runtime":"haosbot"}`))
	})

	// POST /api/fetch-models. The URL is untrusted input and therefore goes
	// through the same SSRF policy used by network-capable tools.
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

	// POST /api/restart (graceful restart of nanobot agent)
	mux.HandleFunc("/api/restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"restarting","message":"Reiniciando haosbot gateway..."}`))
		go func() {
			time.Sleep(500 * time.Millisecond)
			os.Exit(0) // Systemd Restart=always will immediately restart with fresh state
		}()
	})

	// GET & POST /api/config. GET is redacted; POST is a typed, preserving patch
	// and intentionally requires restart instead of performing a half-hot-reload.
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		configPath := filepath.Join(os.Getenv("HOME"), ".haosbot", "config.json")

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

	// GET /v1/models
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

	// POST /v1/chat/completions
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if !s.checkAuth(r) {
			writeUnauthorized(w)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Model       string           `json:"model"`
			Messages    []map[string]any `json:"messages"`
			Temperature float64          `json:"temperature"`
			MaxTokens   int              `json:"max_tokens"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid JSON request: %v", err), http.StatusBadRequest)
			return
		}

		// Intercept Slash Commands (ex: /status, /help, /skills, /model)
		if len(req.Messages) > 0 {
			lastMsg := req.Messages[len(req.Messages)-1]
			content, _ := lastMsg["content"].(string)
			if s.cmdRouter != nil && command.IsSlashCommand(content) {
				workspace := filepath.Join(os.Getenv("HOME"), ".haosbot", "workspace")
				currentModel := req.Model
				if currentModel == "" {
					currentModel = s.cfg.Agents.Defaults.Model
				}
				reply, handled := s.cmdRouter.Execute(content, currentModel, workspace)
				if handled {
					w.Header().Set("Content-Type", "application/json")
					openaiResp := map[string]any{
						"id":      "chatcmpl-cmd-" + fmt.Sprintf("%d", time.Now().Unix()),
						"object":  "chat.completion",
						"created": time.Now().Unix(),
						"model":   currentModel,
						"choices": []map[string]any{
							{
								"index": 0,
								"message": map[string]any{
									"role":    "assistant",
									"content": reply,
								},
								"finish_reason": "stop",
							},
						},
					}
					_ = json.NewEncoder(w).Encode(openaiResp)
					return
				}
			}
		}

		modelName := req.Model
		if modelName == "" {
			modelName = s.cfg.Agents.Defaults.Model
			if modelName == "" {
				modelName = "deepseek-chat"
			}
		}

		if s.provider == nil {
			http.Error(w, "No LLM provider configured", http.StatusServiceUnavailable)
			return
		}

		var coreMsgs []core.Message
		for _, m := range req.Messages {
			role, _ := m["role"].(string)
			content, _ := m["content"].(string)
			coreMsgs = append(coreMsgs, core.Message{
				Role:    core.Role(role),
				Content: core.TextContent(content),
			})
		}

		ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
		defer cancel()

		var finalReply string

		// If loop is connected, execute the full autonomous Agent turn with Tools, Memory and System Prompts!
		if s.loop != nil && len(coreMsgs) > 0 {
			lastUserMsg := coreMsgs[len(coreMsgs)-1]
			inbound := core.InboundMessage{
				Channel:   "webui",
				SenderID:  "webui_user",
				ChatID:    "webui_session",
				Content:   lastUserMsg.Content.Text,
				Timestamp: time.Now(),
				Metadata:  map[string]any{"source": "webui"},
			}
			out, err := s.loop.ProcessMessage(ctx, inbound)
			if err != nil {
				http.Error(w, fmt.Sprintf("Agent turn error: %v", err), http.StatusInternalServerError)
				return
			}
			if out != nil {
				finalReply = out.Content
			}
		} else {
			// Fallback to direct Chat if loop not attached
			chatResp, err := s.provider.Chat(ctx, provider.ChatRequest{
				Messages:    coreMsgs,
				Model:       modelName,
				Temperature: req.Temperature,
				MaxTokens:   req.MaxTokens,
			})
			if err != nil {
				http.Error(w, fmt.Sprintf("Provider error: %v", err), http.StatusInternalServerError)
				return
			}
			finalReply = chatResp.Content
		}

		w.Header().Set("Content-Type", "application/json")
		openaiResp := map[string]any{
			"id":      "chatcmpl-" + fmt.Sprintf("%d", time.Now().Unix()),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   modelName,
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": finalReply,
					},
					"finish_reason": "stop",
				},
			},
		}
		_ = json.NewEncoder(w).Encode(openaiResp)
	})

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
