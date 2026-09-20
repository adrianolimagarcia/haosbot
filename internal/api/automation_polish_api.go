package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	cronruntime "github.com/adrianolimagarcia/nanobot-go/internal/cron"
)

// filterAutomationJobs applies the list filters exposed by
// GET /api/webui/automations. A job matches `status` when either its derived
// lifecycle state (running/active/paused) or its last run status matches, which
// keeps the operator filter useful for both "what is scheduled" and "what just
// happened" queries.
func filterAutomationJobs(jobs []cronruntime.Job, sessionKey, status string) []cronruntime.Job {
	sessionKey = strings.TrimSpace(sessionKey)
	status = strings.TrimSpace(strings.ToLower(status))
	out := make([]cronruntime.Job, 0, len(jobs))
	for _, j := range jobs {
		if sessionKey != "" && j.Payload.SessionKey != sessionKey {
			continue
		}
		if status != "" {
			current := "paused"
			switch {
			case j.State.Pending:
				current = "running"
			case j.Enabled:
				current = "active"
			}
			if current != status && strings.ToLower(j.State.LastStatus) != status {
				continue
			}
		}
		out = append(out, j)
	}
	return out
}

func (s *Server) handleWebUIAutomationFromSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scheduler := s.scheduler.Load()
	if scheduler == nil {
		http.Error(w, "scheduler unavailable", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Name           string               `json:"name"`
		Message        string               `json:"message"`
		SessionKey     string               `json:"session_key"`
		Schedule       cronruntime.Schedule `json:"schedule"`
		DeleteAfterRun bool                 `json:"delete_after_run"`
		TimeoutMS      int64                `json:"timeout_ms"`
		MisfirePolicy  string               `json:"misfire_policy"`
		MisfireGraceMS int64                `json:"misfire_grace_ms"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	req.SessionKey = strings.TrimSpace(req.SessionKey)
	channel, chat, ok := strings.Cut(req.SessionKey, ":")
	if !ok || channel == "" || chat == "" {
		http.Error(w, "invalid session_key", http.StatusBadRequest)
		return
	}
	job, err := scheduler.AddJob(cronruntime.Job{
		Name:           strings.TrimSpace(req.Name),
		Schedule:       req.Schedule,
		DeleteAfterRun: req.DeleteAfterRun,
		TimeoutMS:      req.TimeoutMS,
		MisfirePolicy:  req.MisfirePolicy,
		MisfireGraceMS: req.MisfireGraceMS,
		Payload: cronruntime.Payload{
			Kind:           cronruntime.PayloadAgentTurn,
			Message:        strings.TrimSpace(req.Message),
			SessionKey:     req.SessionKey,
			OriginChannel:  channel,
			OriginChatID:   chat,
			OriginMetadata: map[string]any{"source": "webui", "session_id": chat, "automation_created_from_conversation": true},
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeWebUIJSON(w, automationJobPayload(job))
}

func (s *Server) handleWebUIAutomationHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scheduler := s.scheduler.Load()
	if scheduler == nil {
		http.Error(w, "scheduler unavailable", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	job, ok := scheduler.GetJob(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	status := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("status")))
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	// Newest first, and only allocate what the response can actually carry: the
	// run history of a long-lived job can be large, so pre-sizing by the full
	// slice would allocate for rows the filter drops.
	capacity := limit
	if capacity > len(job.State.RunHistory) {
		capacity = len(job.State.RunHistory)
	}
	history := make([]cronruntime.RunRecord, 0, capacity)
	for i := len(job.State.RunHistory) - 1; i >= 0 && len(history) < limit; i-- {
		run := job.State.RunHistory[i]
		if status != "" && strings.ToLower(run.Status) != status {
			continue
		}
		history = append(history, run)
	}
	writeWebUIJSON(w, map[string]any{
		"job_id":         job.ID,
		"next_run_at_ms": job.State.NextRunAtMS,
		"pending":        job.State.Pending,
		"history":        history,
	})
}
