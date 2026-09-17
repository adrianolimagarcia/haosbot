package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

var webSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

type agentTurnRequest struct {
	SessionID string `json:"sessionId"`
	Message   string `json:"message"`
}

type agentTurnResponse struct {
	SessionID string `json:"sessionId"`
	Content   string `json:"content"`
}

func (s *Server) registerAgentTurn(mux *http.ServeMux) {
	mux.HandleFunc("/api/agent/turn", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		loop := s.loop.Load()
		if loop == nil {
			http.Error(w, "Agent loop is not available", http.StatusServiceUnavailable)
			return
		}

		var req agentTurnRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid JSON request: %v", err), http.StatusBadRequest)
			return
		}

		sessionID := strings.TrimSpace(req.SessionID)
		if sessionID == "" {
			sessionID = strings.TrimSpace(r.Header.Get("X-HAOS-Session-ID"))
		}
		if !webSessionIDPattern.MatchString(sessionID) {
			http.Error(w, "Invalid or missing sessionId", http.StatusBadRequest)
			return
		}
		message := strings.TrimSpace(req.Message)
		if message == "" {
			http.Error(w, "message is required", http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
		defer cancel()
		out, err := loop.ProcessMessage(ctx, core.InboundMessage{
			Channel:   "webui",
			SenderID:  sessionID,
			ChatID:    sessionID,
			Content:   message,
			Timestamp: time.Now(),
			Metadata: map[string]any{
				"source":     "webui",
				"session_id": sessionID,
			},
		})
		if err != nil {
			http.Error(w, fmt.Sprintf("Agent turn error: %v", err), http.StatusInternalServerError)
			return
		}

		content := ""
		if out != nil {
			content = out.Content
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agentTurnResponse{
			SessionID: sessionID,
			Content:   content,
		})
	})
}
