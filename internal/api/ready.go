package api

import (
	"encoding/json"
	"net/http"
)

// Readiness is deliberately separate from liveness.
//
// /health answers "is the process up?", which is what a supervisor restarting a
// wedged process needs. /readyz answers "may this process receive traffic?",
// which is what a reverse proxy, a systemd unit or a container orchestrator
// needs before it routes a request here. Without that distinction an agent that
// is still assembling its runtime answers 200 and is handed traffic it cannot
// serve.
//
// What /readyz checks (Server.readiness):
//
//   - config:    a configuration is loaded. The gateway dereferences it on every
//     protected route.
//   - provider:  a model provider is attached. Without one
//     /v1/chat/completions answers 503 "No LLM provider configured".
//   - agentLoop: the agent loop is attached. Without one /api/agent/turn answers
//     503 and the A2A endpoint degrades to a stub that never calls a model.
//   - process:   the process owner has not taken the gateway out of rotation
//     through SetReady.
//
// What it does NOT check, because the api package cannot observe it: whether the
// agent loop goroutine is actually consuming the bus, whether channels are
// connected, whether MCP servers are reachable, whether storage is writable, or
// whether the configured model provider is reachable. SetReady is the seam for
// those: a runtime that must gate on them calls SetReady(false) until they are
// up. The reference states the same limitation for its own health endpoint
// (docs/multiple-instances.md:123: "Readiness currently reflects only the
// WebSocket channel").
type readinessReport struct {
	Status  string            `json:"status"`
	Runtime string            `json:"runtime"`
	Ready   bool              `json:"ready"`
	Checks  map[string]string `json:"checks"`
	Reasons []string          `json:"reasons,omitempty"`
}

const (
	readinessOK       = "ok"
	readinessDegraded = "degraded"
)

// registerReady registers the readiness probe next to the liveness endpoint.
//
// Like /health it is deliberately outside the protected prefixes in security.go:
// a load balancer or an orchestrator probe has no bearer token, and the report
// exposes only coarse lifecycle state — no configuration, no secrets, no
// session data. Mutation and model-execution surfaces stay authenticated.
func (s *Server) registerReady(mux *http.ServeMux) {
	mux.HandleFunc("/readyz", s.handleReadyz)
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	report := s.readiness()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	// 503 matches the convention the reference uses for its own readiness
	// endpoint (docs/multiple-instances.md:114-118) and the status the gateway
	// already returns for its own degraded surfaces.
	if !report.Ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(report)
}

// SetReady is the process-owned readiness gate. The api package cannot see the
// parts of startup that live above it (the agent loop goroutine draining the
// bus, channel connections, MCP handshakes), so a runtime that gates on them
// takes the gateway out of rotation with SetReady(false) and puts it back with
// SetReady(true). The zero value means "no override", so a gateway that never
// calls it is governed by the built-in checks alone.
func (s *Server) SetReady(ready bool) {
	s.notReady.Store(!ready)
}

// Ready reports whether /readyz would answer 200 right now.
func (s *Server) Ready() bool {
	return s.readiness().Ready
}

func (s *Server) readiness() readinessReport {
	checks := map[string]string{
		"config":    readinessOK,
		"provider":  readinessOK,
		"agentLoop": readinessOK,
		"process":   readinessOK,
	}

	var reasons []string
	if s == nil || s.cfg == nil {
		checks["config"] = "missing"
		reasons = append(reasons, "no configuration loaded")
	}
	if s == nil || s.provider == nil {
		checks["provider"] = "missing"
		reasons = append(reasons, "no model provider attached")
	}
	if s == nil || s.loop == nil {
		checks["agentLoop"] = "missing"
		reasons = append(reasons, "agent loop not attached")
	}
	if s != nil && s.notReady.Load() {
		checks["process"] = "not_ready"
		reasons = append(reasons, "gateway taken out of rotation by the process owner (SetReady)")
	}

	status := readinessOK
	if len(reasons) > 0 {
		status = readinessDegraded
	}
	return readinessReport{
		Status:  status,
		Runtime: "haosbot",
		Ready:   len(reasons) == 0,
		Checks:  checks,
		Reasons: reasons,
	}
}
