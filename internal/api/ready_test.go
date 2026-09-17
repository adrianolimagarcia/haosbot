package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
)

// readyzBody is the readiness contract a probe depends on. The tests decode the
// wire shape rather than the server's internal struct on purpose: a reverse
// proxy or an orchestrator only ever sees the JSON.
type readyzBody struct {
	Status  string            `json:"status"`
	Runtime string            `json:"runtime"`
	Ready   bool              `json:"ready"`
	Checks  map[string]string `json:"checks"`
	Reasons []string          `json:"reasons"`
}

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type readyProvider struct{}

func (readyProvider) Chat(context.Context, provider.ChatRequest) (*core.Response, error) {
	return &core.Response{Content: "ok", FinishReason: core.FinishStop}, nil
}

func (readyProvider) Name() string { return "ready-test" }

type readyTranscript struct {
	key      string
	messages []core.Message
}

func (t *readyTranscript) Key() string               { return t.key }
func (t *readyTranscript) Messages() []core.Message  { return t.messages }
func (t *readyTranscript) AddMessage(m core.Message) { t.messages = append(t.messages, m) }
func (t *readyTranscript) Clear()                    { t.messages = nil }
func (t *readyTranscript) Save() error               { return nil }

type readyStore struct {
	mu       sync.Mutex
	sessions map[string]*readyTranscript
}

func (s *readyStore) Open(key string) (agent.Transcript, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]*readyTranscript{}
	}
	t, ok := s.sessions[key]
	if !ok {
		t = &readyTranscript{key: key}
		s.sessions[key] = t
	}
	return t, nil
}

// newReadyLoop builds a REAL agent.Loop, so the readiness gate is driven by the
// same object the gateway attaches in production rather than by a placeholder.
func newReadyLoop(t *testing.T) *agent.Loop {
	t.Helper()

	messageBus := bus.New(bus.Options{})
	t.Cleanup(messageBus.Close)

	loop, err := agent.NewLoop(agent.LoopConfig{
		Bus:          messageBus,
		Store:        &readyStore{},
		Provider:     readyProvider{},
		Workspace:    t.TempDir(),
		SystemPrompt: "TEST SYSTEM PROMPT",
	})
	if err != nil {
		t.Fatalf("build agent loop: %v", err)
	}
	return loop
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// startGateway runs the real gateway (Start, not a test-only mux) on a loopback
// port and returns its base URL. A probe listener reserves a free port first;
// if another process wins the race for it, Start fails to bind and the attempt
// is retried with a fresh port instead of failing the test.
func startGateway(t *testing.T, s *Server) string {
	t.Helper()

	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		base, err := tryStartGateway(s)
		if err == nil {
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = s.Shutdown(ctx)
			})
			return base
		}
		lastErr = err
	}
	t.Fatalf("could not start the gateway: %v", lastErr)
	return ""
}

func tryStartGateway(s *Server) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		return "", err
	}

	errc := make(chan error, 1)
	go func() { errc <- s.Start(addr) }()

	base := "http://" + addr
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errc:
			return "", err
		default:
		}
		resp, err := http.Get(base + "/health")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return base, nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return "", errors.New("gateway did not answer /health within 5s")
}

func getJSON(t *testing.T, url string) (int, readyzBody, string) {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	var parsed readyzBody
	// A non-JSON body (for example the "404 page not found" the WebUI handler
	// returns for unknown paths) leaves the zero value behind, which is what
	// makes the field assertions below fail loudly.
	_ = json.Unmarshal(raw, &parsed)
	return resp.StatusCode, parsed, string(raw)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// A gateway that has loaded its configuration but has no model provider and no
// agent loop is exactly the startup window an orchestrator must not route to.
func TestReadyzReportsUnavailableWhileRuntimeIsIncomplete(t *testing.T) {
	s := NewServer(config.DefaultConfig(), nil, nil)
	base := startGateway(t, s)

	status, body, raw := getJSON(t, base+"/readyz")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("/readyz status=%d want 503 while the runtime is incomplete: %s", status, raw)
	}
	if body.Ready {
		t.Fatalf("/readyz reported ready=true with no provider and no agent loop: %s", raw)
	}
	if body.Status != "degraded" {
		t.Fatalf("/readyz status=%q want \"degraded\": %s", body.Status, raw)
	}
	if body.Runtime != "haosbot" {
		t.Fatalf("/readyz runtime=%q want \"haosbot\": %s", body.Runtime, raw)
	}
	if got := body.Checks["config"]; got != "ok" {
		t.Fatalf("/readyz checks.config=%q want \"ok\" (config is loaded): %s", got, raw)
	}
	if got := body.Checks["provider"]; got != "missing" {
		t.Fatalf("/readyz checks.provider=%q want \"missing\": %s", got, raw)
	}
	if got := body.Checks["agentLoop"]; got != "missing" {
		t.Fatalf("/readyz checks.agentLoop=%q want \"missing\": %s", got, raw)
	}
	if len(body.Reasons) == 0 {
		t.Fatalf("/readyz reported not-ready without a reason: %s", raw)
	}
}

func TestReadyzReportsReadyWhenRuntimeIsAttached(t *testing.T) {
	s := NewServer(config.DefaultConfig(), readyProvider{}, newReadyLoop(t))
	base := startGateway(t, s)

	status, body, raw := getJSON(t, base+"/readyz")
	if status != http.StatusOK {
		t.Fatalf("/readyz status=%d want 200 with a provider and an agent loop attached: %s", status, raw)
	}
	if !body.Ready {
		t.Fatalf("/readyz reported ready=false for a complete runtime: %s", raw)
	}
	if body.Status != "ok" {
		t.Fatalf("/readyz status=%q want \"ok\": %s", body.Status, raw)
	}
	for _, gate := range []string{"config", "provider", "agentLoop", "process"} {
		if got := body.Checks[gate]; got != "ok" {
			t.Fatalf("/readyz checks.%s=%q want \"ok\": %s", gate, got, raw)
		}
	}
	if len(body.Reasons) != 0 {
		t.Fatalf("/readyz reported reasons while ready: %v", body.Reasons)
	}

	// Readiness must be stable, not a one-off answer: an orchestrator polls it.
	status, _, raw = getJSON(t, base+"/readyz")
	if status != http.StatusOK {
		t.Fatalf("second /readyz probe status=%d want 200: %s", status, raw)
	}
}

// Liveness and readiness answer different questions. /health must keep
// answering while the process is up, even when it is not ready to serve.
func TestReadyzIsSeparateFromLiveness(t *testing.T) {
	s := NewServer(config.DefaultConfig(), nil, nil)
	base := startGateway(t, s)

	if status, _, raw := getJSON(t, base+"/health"); status != http.StatusOK {
		t.Fatalf("/health status=%d want 200: %s", status, raw)
	}
	if status, _, raw := getJSON(t, base+"/readyz"); status == http.StatusOK {
		t.Fatalf("/readyz must not report ready while the runtime is incomplete: %s", raw)
	}
}

// A load balancer or an orchestrator probe usually has no bearer token, so
// /readyz is deliberately outside the protected prefixes in security.go. It
// exposes only coarse lifecycle state. The protected surfaces must stay
// protected.
func TestReadyzIsReachableWithoutCredentials(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.API.APIKey = "secret"
	s := NewServer(cfg, readyProvider{}, newReadyLoop(t))
	base := startGateway(t, s)

	status, _, raw := getJSON(t, base+"/readyz")
	if status != http.StatusOK {
		t.Fatalf("unauthenticated /readyz status=%d want 200: %s", status, raw)
	}

	resp, err := http.Get(base + "/api/config")
	if err != nil {
		t.Fatalf("GET /api/config: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/api/config status=%d want 401 without a bearer token", resp.StatusCode)
	}
}

// The seam for startup work the api package cannot observe (whether the agent
// loop goroutine is really consuming the bus, whether channels finished
// connecting, ...). Taking the gateway out of rotation must show up immediately.
func TestReadyzReflectsProcessOwnerGate(t *testing.T) {
	s := NewServer(config.DefaultConfig(), readyProvider{}, newReadyLoop(t))
	base := startGateway(t, s)

	if status, _, raw := getJSON(t, base+"/readyz"); status != http.StatusOK {
		t.Fatalf("/readyz status=%d want 200 before the owner gate is used: %s", status, raw)
	}

	s.SetReady(false)
	status, body, raw := getJSON(t, base+"/readyz")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("/readyz status=%d want 503 after SetReady(false): %s", status, raw)
	}
	if got := body.Checks["process"]; got != "not_ready" {
		t.Fatalf("/readyz checks.process=%q want \"not_ready\": %s", got, raw)
	}
	if s.Ready() {
		t.Fatal("Server.Ready() reports ready while the process owner gate is closed")
	}

	s.SetReady(true)
	if status, _, raw := getJSON(t, base+"/readyz"); status != http.StatusOK {
		t.Fatalf("/readyz status=%d want 200 after SetReady(true): %s", status, raw)
	}
	if !s.Ready() {
		t.Fatal("Server.Ready() reports not-ready after SetReady(true)")
	}
}
