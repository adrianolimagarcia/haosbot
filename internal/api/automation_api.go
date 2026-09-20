package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	cronruntime "github.com/adrianolimagarcia/nanobot-go/internal/cron"
)

func automationJobPayload(job cronruntime.Job) map[string]any {
	raw, _ := json.Marshal(job)
	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)
	state, _ := payload["state"].(map[string]any)
	if state == nil {
		state = map[string]any{}
		payload["state"] = state
	}
	state["pending"] = job.State.Pending
	payload["protected"] = job.Payload.Kind == cronruntime.PayloadSystemEvent
	return payload
}

func automationJobsPayload(jobs []cronruntime.Job) []map[string]any {
	out := make([]map[string]any, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, automationJobPayload(job))
	}
	return out
}

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
			"active": scheduler.ActiveCount(),
			"max_concurrent": scheduler.MaxConcurrent(),
			"jobs": automationJobsPayload(scheduler.ListJobs(true)),
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
			TimeoutMS      int64                `json:"timeout_ms"`
			MisfirePolicy string               `json:"misfire_policy"`
			MisfireGraceMS int64               `json:"misfire_grace_ms"`
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
			TimeoutMS: req.TimeoutMS, MisfirePolicy: req.MisfirePolicy, MisfireGraceMS: req.MisfireGraceMS,
			Payload: cronruntime.Payload{
				Kind: cronruntime.PayloadAgentTurn,
				Message: strings.TrimSpace(req.Message),
				SessionKey: req.SessionKey,
				OriginChannel: req.OriginChannel,
				OriginChatID: req.OriginChatID,
				OriginMetadata: map[string]any{"source":"webui","session_id":strings.TrimPrefix(req.SessionKey, "webui:")},
			},
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeWebUIJSON(w, automationJobPayload(job))
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
		writeWebUIJSON(w, automationJobPayload(job))
	case http.MethodPatch:
		var req struct {
			Name           *string               `json:"name"`
			Enabled        *bool                 `json:"enabled"`
			Schedule       *cronruntime.Schedule `json:"schedule"`
			Message        *string               `json:"message"`
			DeleteAfterRun *bool                 `json:"delete_after_run"`
			TimeoutMS      *int64                `json:"timeout_ms"`
			MisfirePolicy *string               `json:"misfire_policy"`
			MisfireGraceMS *int64               `json:"misfire_grace_ms"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		job, err := scheduler.UpdateJob(id, cronruntime.Update{
			Name: req.Name, Enabled: req.Enabled, Schedule: req.Schedule,
			Message: req.Message, DeleteAfterRun: req.DeleteAfterRun,
			TimeoutMS: req.TimeoutMS, MisfirePolicy: req.MisfirePolicy, MisfireGraceMS: req.MisfireGraceMS,
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
		writeWebUIJSON(w, automationJobPayload(job))
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
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" { http.Error(w, "id is required", http.StatusBadRequest); return }
		if err := scheduler.Cancel(id); err != nil { http.Error(w, err.Error(), http.StatusConflict); return }
		w.WriteHeader(http.StatusNoContent)
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
			"scheduled_for_ms": record["scheduled_for_ms"], "idempotency_key": record["idempotency_key"],
			"duration_ms": record["duration_ms"], "error": record["error"],
			"response": record["response"],
		})
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
