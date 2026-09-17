package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// readyRuntime is the runtime a healthy gateway actually has: a real agent loop
// draining a real bus, a provider, a loaded config, and a writable data
// directory. /readyz gates on all of them, so the tests wire them the way
// cmd/haosbot/runtime.go does instead of leaving the new handles unset.
type readyRuntime struct {
	server  *Server
	bus     *bus.Bus
	loop    *agent.Loop
	dataDir string
}

func newReadyRuntime(t *testing.T) *readyRuntime {
	t.Helper()
	return newReadyRuntimeWithConfig(t, config.DefaultConfig())
}

func newReadyRuntimeWithConfig(t *testing.T, cfg *config.Config) *readyRuntime {
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

	// The real consumer goroutine, exactly as the gateway starts it.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = loop.Run(ctx) }()

	// A probe that arrives before the loop goroutine has entered ConsumeInbound
	// is genuinely not ready, so wait for the attachment instead of racing it.
	deadline := time.Now().Add(5 * time.Second)
	for !messageBus.HasConsumer() {
		if time.Now().After(deadline) {
			t.Fatal("the agent loop never attached to the bus")
		}
		time.Sleep(time.Millisecond)
	}

	dataDir := t.TempDir()
	s := NewServer(cfg, readyProvider{}, loop)
	s.SetBus(messageBus)
	s.SetDataDir(dataDir)

	return &readyRuntime{server: s, bus: messageBus, loop: loop, dataDir: dataDir}
}

// newIdleLoop builds a loop around a fresh bus WITHOUT starting Run, which is
// the state the bus gate exists to catch.
func newIdleLoop(t *testing.T) (*agent.Loop, *bus.Bus) {
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
	return loop, messageBus
}

// reasonFor returns the reason whose text starts with "check: ".
func reasonFor(body readyzBody, check string) string {
	for _, reason := range body.Reasons {
		if strings.HasPrefix(reason, check+": ") {
			return reason
		}
	}
	return ""
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
	// The bus and data-directory gates are "missing" too: this server was built
	// without SetBus/SetDataDir, and a gate the server cannot evaluate must fail
	// closed rather than pass by default.
	if got := body.Checks["bus"]; got != "missing" {
		t.Fatalf("/readyz checks.bus=%q want \"missing\" (no bus was attached): %s", got, raw)
	}
	if got := body.Checks["dataDir"]; got != "missing" {
		t.Fatalf("/readyz checks.dataDir=%q want \"missing\" (no data directory was recorded): %s", got, raw)
	}
	if len(body.Reasons) == 0 {
		t.Fatalf("/readyz reported not-ready without a reason: %s", raw)
	}
	for _, gate := range []string{"provider", "agentLoop", "bus", "dataDir"} {
		if reason := reasonFor(body, gate); reason == "" {
			t.Fatalf("/readyz did not name the failing check %q in its reasons: %s", gate, raw)
		}
	}
}

func TestReadyzReportsReadyWhenRuntimeIsAttached(t *testing.T) {
	s := newReadyRuntime(t).server
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
	for _, gate := range []string{"config", "provider", "agentLoop", "process", "bus", "dataDir"} {
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
	s := newReadyRuntimeWithConfig(t, cfg).server
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
	s := newReadyRuntime(t).server
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

// ---------------------------------------------------------------------------
// Gate: the message bus
// ---------------------------------------------------------------------------

// A closed bus is out of service: every publish returns ErrClosed and the loop
// that was draining it has already returned, so channel messages cannot reach
// the agent at all.
func TestReadyzReportsNotReadyWhenTheBusIsClosed(t *testing.T) {
	rt := newReadyRuntime(t)
	base := startGateway(t, rt.server)

	if status, _, raw := getJSON(t, base+"/readyz"); status != http.StatusOK {
		t.Fatalf("/readyz status=%d want 200 while the bus is open: %s", status, raw)
	}

	// Real failure injection: close the bus the server actually holds.
	rt.bus.Close()

	status, body, raw := getJSON(t, base+"/readyz")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("/readyz status=%d want 503 after the bus was closed: %s", status, raw)
	}
	if got := body.Checks["bus"]; got != "closed" {
		t.Fatalf("/readyz checks.bus=%q want \"closed\": %s", got, raw)
	}
	if reason := reasonFor(body, "bus"); reason == "" {
		t.Fatalf("/readyz did not name the failing check in its reasons: %s", raw)
	}
	// The other gates are independent and still healthy.
	if got := body.Checks["dataDir"]; got != "ok" {
		t.Fatalf("/readyz checks.dataDir=%q want \"ok\" (the bus failure is not a data-dir failure): %s", got, raw)
	}
	if rt.server.Ready() {
		t.Fatal("Server.Ready() reports ready while the bus is closed")
	}
}

// A bus nobody drains is the failure the previous revision could not see: the
// loop is attached, so every other gate passes, and messages pile up until the
// queue bound rejects them.
func TestReadyzReportsNotReadyWhenNothingConsumesTheBus(t *testing.T) {
	loop, messageBus := newIdleLoop(t)

	s := NewServer(config.DefaultConfig(), readyProvider{}, loop)
	s.SetBus(messageBus)
	s.SetDataDir(t.TempDir())
	base := startGateway(t, s)

	status, body, raw := getJSON(t, base+"/readyz")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("/readyz status=%d want 503 while nothing consumes the bus: %s", status, raw)
	}
	if got := body.Checks["bus"]; got != "no_consumer" {
		t.Fatalf("/readyz checks.bus=%q want \"no_consumer\": %s", got, raw)
	}
	if reason := reasonFor(body, "bus"); reason == "" {
		t.Fatalf("/readyz did not name the failing check in its reasons: %s", raw)
	}
	for _, gate := range []string{"config", "provider", "agentLoop", "dataDir"} {
		if got := body.Checks[gate]; got != "ok" {
			t.Fatalf("/readyz checks.%s=%q want \"ok\": only the bus gate should be failing: %s", gate, got, raw)
		}
	}

	// The gate must open when the loop actually starts draining, or it is
	// decoration rather than a probe.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = loop.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for !messageBus.HasConsumer() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if status, _, raw := getJSON(t, base+"/readyz"); status != http.StatusOK {
		t.Fatalf("/readyz status=%d want 200 once the loop drains the bus: %s", status, raw)
	}
}

// ---------------------------------------------------------------------------
// Gate: the data directory
// ---------------------------------------------------------------------------

// The write probe must fail where a stat would succeed. A regular file is
// exactly that case: os.Stat reports it as present, while nothing can be created
// inside it.
func TestReadyzDataDirGateIsAWriteNotAStat(t *testing.T) {
	rt := newReadyRuntime(t)
	base := startGateway(t, rt.server)

	// Sanity: the healthy directory passes.
	if status, body, raw := getJSON(t, base+"/readyz"); status != http.StatusOK {
		t.Fatalf("/readyz status=%d want 200 with a writable data directory: %s (checks=%v)", status, raw, body.Checks)
	}

	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("create the file used as a fake data directory: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("the injected path must be stat-able, or the test would not distinguish a write from a stat: %v", err)
	}

	rt.server.SetDataDir(file)

	status, body, raw := getJSON(t, base+"/readyz")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("/readyz status=%d want 503 for a data directory that cannot be written to: %s", status, raw)
	}
	if got := body.Checks["dataDir"]; got != "unwritable" {
		t.Fatalf("/readyz checks.dataDir=%q want \"unwritable\" (a stat-based check would have passed): %s", got, raw)
	}
	if reason := reasonFor(body, "dataDir"); reason == "" {
		t.Fatalf("/readyz did not name the failing check in its reasons: %s", raw)
	}
	if got := body.Checks["bus"]; got != "ok" {
		t.Fatalf("/readyz checks.bus=%q want \"ok\" (the data-dir failure is not a bus failure): %s", got, raw)
	}
	if rt.server.Ready() {
		t.Fatal("Server.Ready() reports ready while the data directory is not writable")
	}
}

// A data directory that does not exist is the other half of the same gate: the
// session store's root is created beneath it, so it cannot be missing.
func TestReadyzReportsNotReadyWhenTheDataDirectoryDoesNotExist(t *testing.T) {
	rt := newReadyRuntime(t)
	base := startGateway(t, rt.server)

	rt.server.SetDataDir(filepath.Join(t.TempDir(), "missing", "data"))

	status, body, raw := getJSON(t, base+"/readyz")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("/readyz status=%d want 503 for a missing data directory: %s", status, raw)
	}
	if got := body.Checks["dataDir"]; got != "unwritable" {
		t.Fatalf("/readyz checks.dataDir=%q want \"unwritable\": %s", got, raw)
	}
}

// A readiness probe runs on a timer for the life of the process: it must not
// leave anything behind in the directory it writes to.
func TestReadyzWriteProbeLeavesNothingBehind(t *testing.T) {
	rt := newReadyRuntime(t)
	base := startGateway(t, rt.server)

	for i := 0; i < 3; i++ {
		if status, _, raw := getJSON(t, base+"/readyz"); status != http.StatusOK {
			t.Fatalf("probe %d status=%d want 200: %s", i, status, raw)
		}
	}

	entries, err := os.ReadDir(rt.dataDir)
	if err != nil {
		t.Fatalf("read the data directory: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the write probe left %d entries behind: %v", len(entries), names)
	}
}

// A probe must answer even when a check does not. The bound is exercised with a
// check that really blocks; the hung filesystem it guards against could not be
// reproduced in a test.
func TestReadyzCheckIsBounded(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	start := time.Now()
	state, err := boundedCheck(func() (string, error) {
		<-release
		return readinessOK, nil
	})
	elapsed := time.Since(start)

	if state != readinessTimeout {
		t.Fatalf("state=%q want %q for a check that never answers", state, readinessTimeout)
	}
	if err == nil {
		t.Fatal("a check that timed out must report an error")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the check was abandoned only after %s, want the %s bound", elapsed, readinessCheckTimeout)
	}
}

// The runtime handles are injected while the server may already be answering
// probes (SetLoop/SetBus/SetDataDir are late-injection seams), so setting them
// must not race with a probe reading them. The assertion is the race detector:
// this test has no other pass/fail condition by design.
func TestReadyzHandlesAreSafeToSetWhileProbing(t *testing.T) {
	s := NewServer(config.DefaultConfig(), readyProvider{}, nil)
	base := startGateway(t, s)

	loop, messageBus := newIdleLoop(t)
	dataDir := t.TempDir()

	stop := make(chan struct{})
	var probes sync.WaitGroup
	probes.Add(1)
	go func() {
		defer probes.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			resp, err := http.Get(base + "/readyz")
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	for i := 0; i < 200; i++ {
		s.SetLoop(loop)
		s.SetBus(messageBus)
		s.SetDataDir(dataDir)
	}
	close(stop)
	probes.Wait()
}
