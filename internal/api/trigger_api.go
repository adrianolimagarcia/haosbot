package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"

	triggersruntime "github.com/adrianolimagarcia/nanobot-go/internal/triggers"
)

func (s *Server) handleWebUITriggers(w http.ResponseWriter, r *http.Request) {
	service := s.triggers.Load(); if service == nil { http.Error(w, "trigger service unavailable", http.StatusServiceUnavailable); return }
	switch r.Method {
	case http.MethodGet:
		sessionKey := strings.TrimSpace(r.URL.Query().Get("session_key")); items := service.List(true)
		if sessionKey != "" { filtered := make([]triggersruntime.Trigger, 0); for _, tr := range items { if tr.SessionKey == sessionKey { filtered = append(filtered, tr) } }; items = filtered }
		writeWebUIJSON(w, map[string]any{"running": service.Running(), "triggers": items})
	case http.MethodPost:
		var req struct { Name string `json:"name"`; SessionKey string `json:"session_key"`; Channel string `json:"channel"`; ChatID string `json:"chat_id"` }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil { http.Error(w, "invalid JSON", http.StatusBadRequest); return }
		req.SessionKey = strings.TrimSpace(req.SessionKey); if req.Channel == "" || req.ChatID == "" { channel, chat, ok := strings.Cut(req.SessionKey, ":"); if ok { if req.Channel == "" { req.Channel = channel }; if req.ChatID == "" { req.ChatID = chat } } }
		tr, err := service.Create(req.Name, req.Channel, req.ChatID, req.SessionKey, map[string]any{"source":"webui", "session_id":strings.TrimPrefix(req.SessionKey,"webui:")}); if err != nil { http.Error(w, err.Error(), http.StatusBadRequest); return }
		w.WriteHeader(http.StatusCreated); writeWebUIJSON(w, tr)
	default:
		w.Header().Set("Allow", "GET, POST"); http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleWebUITrigger(w http.ResponseWriter, r *http.Request) {
	service := s.triggers.Load(); if service == nil { http.Error(w, "trigger service unavailable", http.StatusServiceUnavailable); return }
	id := strings.TrimSpace(r.URL.Query().Get("id")); if id == "" { http.Error(w, "id is required", http.StatusBadRequest); return }
	switch r.Method {
	case http.MethodGet:
		tr, ok := service.Get(id); if !ok { http.NotFound(w, r); return }; writeWebUIJSON(w, tr)
	case http.MethodPatch:
		var req struct { Name *string `json:"name"`; Enabled *bool `json:"enabled"` }; if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil { http.Error(w, "invalid JSON", http.StatusBadRequest); return }
		tr, err := service.Update(id, req.Name, req.Enabled); if errors.Is(err, os.ErrNotExist) { http.NotFound(w, r); return }; if err != nil { http.Error(w, err.Error(), http.StatusBadRequest); return }; writeWebUIJSON(w, tr)
	case http.MethodDelete:
		err := service.Delete(id); if errors.Is(err, os.ErrNotExist) { http.NotFound(w, r); return }; if err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return }; w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PATCH, DELETE"); http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleWebUITriggerFire(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { w.Header().Set("Allow", "POST"); http.Error(w, "Method not allowed", http.StatusMethodNotAllowed); return }
	service := s.triggers.Load(); if service == nil { http.Error(w, "trigger service unavailable", http.StatusServiceUnavailable); return }
	id := strings.TrimSpace(r.URL.Query().Get("id")); if id == "" { http.Error(w, "id is required", http.StatusBadRequest); return }
	var req struct { Content string `json:"content"` }; if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil { http.Error(w, "invalid JSON", http.StatusBadRequest); return }
	d, err := service.Enqueue(id, req.Content); if errors.Is(err, os.ErrNotExist) { http.NotFound(w, r); return }; if err != nil { http.Error(w, err.Error(), http.StatusBadRequest); return }
	w.WriteHeader(http.StatusAccepted); writeWebUIJSON(w, map[string]any{"status":"queued", "delivery":d})
}
