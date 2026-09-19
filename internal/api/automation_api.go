package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	cronruntime "github.com/adrianolimagarcia/nanobot-go/internal/cron"
)

func (s *Server) handleWebUIAutomations(w http.ResponseWriter, r *http.Request) {
	scheduler := s.scheduler.Load()
	if scheduler == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "scheduler_unavailable", "Automation scheduler is not configured")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeWebUIJSON(w, map[string]any{
			"running": scheduler.Running(),
			"jobs": scheduler.ListJobs(true),
		})
	case http.MethodPost:
		var req struct {
			Name           string               `json:"name"`
			Message        string               `json:"message"`
			SessionKey     string               `json:"session_key"`
			OriginChannel  string               `json:"origin_channel"`
			OriginChatID   string               `json:"origin_chat_id"`
			Schedule       cronruntime.Schedule `json:"schedule"`
			DeleteAfterRun bool                 `json:"delete_after_run"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		req.SessionKey = strings.TrimSpace(req.SessionKey)
		req.OriginChannel = strings.TrimSpace(req.OriginChannel)
		req.OriginChatID = strings.TrimSpace(req.OriginChatID)
		if req.OriginChannel == "" || req.OriginChatID == "" {
			channel, chatID, ok := strings.Cut(req.SessionKey, ":")
			if ok {
				if req.OriginChannel == "" { req.OriginChannel = channel }
				if req.OriginChatID == "" { req.OriginChatID = chatID }
			}
		}
		job, err := scheduler.AddJob(cronruntime.Job{
			Name: strings.TrimSpace(req.Name),
			Schedule: req.Schedule,
			DeleteAfterRun: req.DeleteAfterRun,
			Payload: cronruntime.Payload{
				Kind: cronruntime.PayloadAgentTurn,
				Message: strings.TrimSpace(req.Message),
				SessionKey: req.SessionKey,
				OriginChannel: req.OriginChannel,
				OriginChatID: req.OriginChatID,
			},
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeWebUIJSON(w, job)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleWebUIAutomation(w http.ResponseWriter, r *http.Request) {
	scheduler := s.scheduler.Load()
	if scheduler == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "scheduler_unavailable", "Automation scheduler is not configured")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		job, ok := scheduler.GetJob(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeWebUIJSON(w, job)
	case http.MethodPatch:
		var req struct {
			Name           *string               `json:"name"`
			Enabled        *bool                 `json:"enabled"`
			Schedule       *cronruntime.Schedule `json:"schedule"`
			Message        *string               `json:"message"`
			DeleteAfterRun *bool                 `json:"delete_after_run"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		job, err := scheduler.UpdateJob(id, cronruntime.Update{
			Name: req.Name, Enabled: req.Enabled, Schedule: req.Schedule,
			Message: req.Message, DeleteAfterRun: req.DeleteAfterRun,
		})
		if errors.Is(err, cronruntime.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, cronruntime.ErrProtected) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeWebUIJSON(w, job)
	case http.MethodDelete:
		err := scheduler.RemoveJob(id)
		if errors.Is(err, cronruntime.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, cronruntime.ErrProtected) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PATCH, DELETE")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleWebUIAutomationRun(w http.ResponseWriter, r *http.Request) {
	scheduler := s.scheduler.Load()
	if scheduler == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "scheduler_unavailable", "Automation scheduler is not configured")
		return
	}
	switch r.Method {
	case http.MethodPost:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			http.Error(w, "id is required", http.StatusBadRequest)
			return
		}
		if err := scheduler.RunNow(id, true); err != nil {
			switch {
			case errors.Is(err, cronruntime.ErrNotFound):
				http.NotFound(w, r)
			case errors.Is(err, cronruntime.ErrActive):
				http.Error(w, err.Error(), http.StatusConflict)
			default:
				http.Error(w, err.Error(), http.StatusBadRequest)
			}
			return
		}
		w.WriteHeader(http.StatusAccepted)
		writeWebUIJSON(w, map[string]any{"status": "queued", "id": id})
	case http.MethodGet:
		runID := strings.TrimSpace(r.URL.Query().Get("run_id"))
		record, err := scheduler.ReadRunRecord(runID)
		if err != nil {
			if errors.Is(err, cronruntime.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			if strings.Contains(err.Error(), "no such file") {
				http.NotFound(w, r)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Return only the run detail fields relevant to the operator UI.
		writeWebUIJSON(w, map[string]any{
			"run_id": record["run_id"], "job_id": record["job_id"],
			"status": record["status"], "created_at_ms": record["created_at_ms"],
			"duration_ms": record["duration_ms"], "error": record["error"],
			"response": record["response"],
		})
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
