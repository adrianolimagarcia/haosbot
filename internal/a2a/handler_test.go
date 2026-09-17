package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/pyjson"
)

const taskSendBody = `{"jsonrpc":"2.0","id":"1","method":"tasks/send","params":{"message":{"text":"hello"}}}`

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// gateProvider never completes a model call on its own: every call waits for the
// context it was handed to be cancelled, or for the test to release it. That
// makes the things the A2A handler is responsible for — forwarding the request's
// cancellation, applying the configured per-request bound, and completing a task
// while a peer is reading it — directly observable.
type gateProvider struct {
	entered   chan time.Duration
	cancelled chan error

	mu    sync.Mutex
	gates []chan struct{}
	once  sync.Once
}

func newGateProvider() *gateProvider {
	return &gateProvider{
		entered:   make(chan time.Duration, 64),
		cancelled: make(chan error, 64),
	}
}

func (p *gateProvider) Chat(ctx context.Context, _ provider.ChatRequest) (*core.Response, error) {
	gate := make(chan struct{})
	p.mu.Lock()
	p.gates = append(p.gates, gate)
	p.mu.Unlock()

	remaining := time.Duration(-1)
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	select {
	case p.entered <- remaining:
	default:
	}

	select {
	case <-ctx.Done():
		err := ctx.Err()
		select {
		case p.cancelled <- err:
		default:
		}
		return nil, err
	case <-gate:
		return &core.Response{Content: "done", FinishReason: core.FinishStop}, nil
	}
}

func (p *gateProvider) Name() string { return "gate-test" }

// releaseAll completes every model call that is waiting right now.
func (p *gateProvider) releaseAll() {
	p.mu.Lock()
	gates := p.gates
	p.gates = nil
	p.mu.Unlock()
	for _, gate := range gates {
		close(gate)
	}
}

// releaseOnce unblocks whatever is still waiting, so a failing test can still
// tear its HTTP server down.
func (p *gateProvider) releaseOnce() { p.once.Do(p.releaseAll) }

// memTranscript / memStore are the smallest TranscriptStore that a real
// agent.Loop can drive, so the tests exercise the production loop rather than a
// stub of it.
type memTranscript struct {
	key      string
	messages []core.Message
}

func (m *memTranscript) Key() string              { return m.key }
func (m *memTranscript) Messages() []core.Message { return m.messages }
func (m *memTranscript) AddMessage(msg core.Message) {
	m.messages = append(m.messages, msg)
}
func (m *memTranscript) Clear()      { m.messages = nil }
func (m *memTranscript) Save() error { return nil }

type memStore struct {
	mu       sync.Mutex
	sessions map[string]*memTranscript
}

func newMemStore() *memStore { return &memStore{sessions: map[string]*memTranscript{}} }

func (s *memStore) Open(key string) (agent.Transcript, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.sessions[key]
	if !ok {
		t = &memTranscript{key: key}
		s.sessions[key] = t
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// taskRun is one in-flight tasks/send request driven through the real HTTP
// handler.
type taskRun struct {
	cancel    context.CancelFunc
	entered   <-chan time.Duration
	cancelled <-chan error
	responded chan struct{}
}

// newTestLoop builds a real agent.Loop around the given provider, so the tests
// drive the production loop rather than a stub of it.
func newTestLoop(t *testing.T, prov provider.Provider) *agent.Loop {
	t.Helper()

	messageBus := bus.New(bus.Options{})
	t.Cleanup(messageBus.Close)

	loop, err := agent.NewLoop(agent.LoopConfig{
		Bus:          messageBus,
		Store:        newMemStore(),
		Provider:     prov,
		Workspace:    t.TempDir(),
		SystemPrompt: "TEST SYSTEM PROMPT",
	})
	if err != nil {
		t.Fatalf("build agent loop: %v", err)
	}
	return loop
}

// startTask posts one tasks/send request to a real httptest server. The agent's
// provider blocks until its context is cancelled, so the caller can decide when
// (and whether) the work stops.
func startTask(t *testing.T, cfg *config.Config, prov *gateProvider) *taskRun {
	t.Helper()

	loop := newTestLoop(t, prov)

	mux := http.NewServeMux()
	NewHandler(cfg, loop).RegisterRoutes(mux)
	srv := httptest.NewServer(mux)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/a2a", strings.NewReader(taskSendBody))
	if err != nil {
		cancel()
		srv.Close()
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	responded := make(chan struct{})
	go func() {
		defer close(responded)
		resp, err := srv.Client().Do(req)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	t.Cleanup(func() {
		cancel()
		prov.releaseOnce()
		srv.Close()
	})

	return &taskRun{cancel: cancel, entered: prov.entered, cancelled: prov.cancelled, responded: responded}
}

func fetchAgentCard(t *testing.T, cfg *config.Config) AgentCard {
	t.Helper()

	mux := http.NewServeMux()
	NewHandler(cfg, nil).RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("GET agent card: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent card status=%d want 200", resp.StatusCode)
	}

	var card AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatalf("decode agent card: %v", err)
	}
	return card
}

// fetchAgentCardRaw returns the agent card response body verbatim, so a test can
// compare it byte for byte instead of comparing decoded fields.
func fetchAgentCardRaw(t *testing.T, cfg *config.Config) []byte {
	t.Helper()

	mux := http.NewServeMux()
	NewHandler(cfg, nil).RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("GET agent card: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent card status=%d want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read agent card: %v", err)
	}
	return body
}

// ---------------------------------------------------------------------------
// Cancellation
// ---------------------------------------------------------------------------

// Regression: the task context used to be built from context.Background()
// (handler.go:220). A client that disconnected therefore did not stop the turn:
// the agent kept calling the model and held its concurrency slot until the full
// 120s bound elapsed.
func TestTaskSendStopsWhenClientDisconnects(t *testing.T) {
	prov := newGateProvider()
	run := startTask(t, config.DefaultConfig(), prov)

	select {
	case <-run.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent never reached the model provider")
	}

	// The peer goes away: browser tab closed, proxy timeout, client deadline.
	run.cancel()

	select {
	case err := <-run.cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("provider observed %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client disconnect did not cancel the in-flight task: the handler is " +
			"working on a context that is not derived from the incoming request")
	}

	// The turn must also unwind promptly instead of occupying its slot until
	// the 120s bound.
	select {
	case <-run.responded:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not finish the cancelled task promptly")
	}
}

// The upper bound must survive the switch to r.Context(): a task is still
// bounded by api.timeout when the client stays connected.
func TestTaskSendDeadlineComesFromConfiguredAPITimeout(t *testing.T) {
	for _, tc := range []struct {
		seconds float64
		min     time.Duration
		max     time.Duration
	}{
		{seconds: 5, min: 1 * time.Second, max: 20 * time.Second},
		{seconds: 60, min: 30 * time.Second, max: 90 * time.Second},
	} {
		t.Run(fmt.Sprintf("api.timeout=%gs", tc.seconds), func(t *testing.T) {
			prov := newGateProvider()
			cfg := config.DefaultConfig()
			cfg.API.Timeout = pyjson.Float(tc.seconds)

			run := startTask(t, cfg, prov)

			var remaining time.Duration
			select {
			case remaining = <-run.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the agent never reached the model provider")
			}

			if remaining <= 0 {
				t.Fatal("the model call was given no deadline: the per-request bound is not applied")
			}
			if remaining < tc.min || remaining > tc.max {
				t.Fatalf("model call deadline is %s, want it bounded by the configured api.timeout of %gs (%s..%s)",
					remaining, tc.seconds, tc.min, tc.max)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Agent card
// ---------------------------------------------------------------------------

func TestAgentCardAdvertisesConfiguredPort(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.API.Host = "127.0.0.1"
	cfg.API.Port = 9123

	card := fetchAgentCard(t, cfg)
	if want := "http://127.0.0.1:9123/a2a"; card.URL != want {
		t.Fatalf("agent card url=%q want %q", card.URL, want)
	}
}

// Regression: when api.port was non-positive the handler fell back to a private
// 8765 literal, which disagrees with the loader's own default (8900) and
// advertises a port the gateway is not listening on. The fallback must come
// from the same source as the configured default.
func TestAgentCardPortFallbackMatchesLoaderDefault(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.API.Port = 0

	card := fetchAgentCard(t, cfg)
	want := fmt.Sprintf("http://127.0.0.1:%d/a2a", config.DefaultConfig().API.Port)
	if card.URL != want {
		t.Fatalf("agent card url=%q want %q (the fallback must match the loader's default api.port)", card.URL, want)
	}
}

// preFixCardBody is the agent card body produced by the gateway binary built
// from the commit BEFORE api.publicBaseUrl existed, bound to 127.0.0.1:41877.
// It was captured by running the real binary and is reproduced here byte for
// byte — including the trailing newline json.Encoder appends — so the
// no-regression half of this feature is asserted against observed output rather
// than against a re-derivation of it.
const preFixCardBody = `{"name":"haosbot","description":"Autonomous lightweight infrastructure, shell execution and Python/SQLite agent powered by haosbot","url":"http://127.0.0.1:41877/a2a","version":"1.0.0","capabilities":{"streaming":false,"pushNotifications":false},"skills":[{"id":"exec","name":"Terminal Command Execution","description":"Execute shell commands, monitor system metrics, systemd and disk usage"},{"id":"python_exec","name":"Python SQLite Runner","description":"Execute isolated Python scripts with SQLite, JSON and networking support"},{"id":"file_ops","name":"Workspace File Operations","description":"Read, write and edit files safely inside the agent workspace"}]}` + "\n"

// TestAgentCardUnsetIsByteIdenticalToPreFixOutput is the no-regression test for
// the new key: with api.publicBaseUrl unset the card must be byte-for-byte what
// the pre-fix binary emitted, so adding the key cannot quietly change the
// existing effective-bind-address behaviour.
func TestAgentCardUnsetIsByteIdenticalToPreFixOutput(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.API.Host = "127.0.0.1"
	cfg.API.Port = 41877
	if cfg.API.PublicBaseURL != "" {
		t.Fatalf("precondition: DefaultConfig().API.PublicBaseURL = %q, want empty", cfg.API.PublicBaseURL)
	}

	got := fetchAgentCardRaw(t, cfg)
	if string(got) != preFixCardBody {
		t.Errorf("agent card body changed with api.publicBaseUrl unset.\n got: %s\nwant: %s", got, preFixCardBody)
	}
}

// TestAgentCardPublicBaseURLJoining pins the joining rule for the shapes an
// operator can reasonably configure. The value is a BASE: a trailing slash is
// dropped, and a path component is preserved and prefixed onto "/a2a".
func TestAgentCardPublicBaseURLJoining(t *testing.T) {
	cases := []struct {
		name string
		base string
		want string
	}{
		{"bare host", "https://example.com", "https://example.com/a2a"},
		{"trailing slash", "https://example.com/", "https://example.com/a2a"},
		{"path component", "https://example.com/base", "https://example.com/base/a2a"},
		{"path with trailing slash", "https://example.com/base/", "https://example.com/base/a2a"},
		{"several trailing slashes", "https://example.com/base///", "https://example.com/base/a2a"},
		{"explicit port", "http://127.0.0.1:8900", "http://127.0.0.1:8900/a2a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.API.PublicBaseURL = tc.base

			card := fetchAgentCard(t, cfg)
			if card.URL != tc.want {
				t.Errorf("api.publicBaseUrl=%q: card url=%q want %q", tc.base, card.URL, tc.want)
			}
		})
	}
}

// TestAgentCardPublicBaseURLWinsOverWildcardHost pins that the configured
// public URL is used even when the bind address is a wildcard, which is the
// usual shape behind a reverse proxy: the gateway binds 0.0.0.0 and the proxy
// terminates TLS on a real hostname.
func TestAgentCardPublicBaseURLWinsOverWildcardHost(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.API.Host = "0.0.0.0"
	cfg.API.Port = 41877
	cfg.API.PublicBaseURL = "https://example.com"

	card := fetchAgentCard(t, cfg)
	if want := "https://example.com/a2a"; card.URL != want {
		t.Errorf("card url=%q want %q", card.URL, want)
	}
}

// TestAgentCardPublicBaseURLAndEffectiveBindAddressDoNotFight is the
// interaction test between this feature and the one before it.
//
// cmd/haosbot/runtime.go resolves --host/--port and writes the EFFECTIVE
// address back into cfg.API just before the server starts, because the card is
// built from cfg.API. That write-back must keep working, and the public URL
// must neither clobber it nor be clobbered by it: they answer different
// questions — "what do I bind" and "what do peers call me".
func TestAgentCardPublicBaseURLAndEffectiveBindAddressDoNotFight(t *testing.T) {
	// applyEffectiveBind reproduces cmdGateway's write-back.
	applyEffectiveBind := func(cfg *config.Config, host string, port int) {
		cfg.API.Host = host
		cfg.API.Port = port
	}

	t.Run("unset: the card follows the effective bind address", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.API.Host = "127.0.0.1"
		cfg.API.Port = 8900 // configured, and overridden below
		applyEffectiveBind(cfg, "127.0.0.1", 41877)

		if card := fetchAgentCard(t, cfg); card.URL != "http://127.0.0.1:41877/a2a" {
			t.Errorf("card url=%q want http://127.0.0.1:41877/a2a — the effective-bind fix must still hold", card.URL)
		}
	})

	t.Run("set: the public URL wins and the bind address is untouched", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.API.Host = "127.0.0.1"
		cfg.API.Port = 8900
		cfg.API.PublicBaseURL = "https://example.com/agent"
		applyEffectiveBind(cfg, "127.0.0.1", 41877)

		if card := fetchAgentCard(t, cfg); card.URL != "https://example.com/agent/a2a" {
			t.Errorf("card url=%q want https://example.com/agent/a2a", card.URL)
		}
		// The write-back must still be visible in the config: the listener and
		// every other cfg.API consumer read these two fields.
		if cfg.API.Host != "127.0.0.1" || cfg.API.Port != 41877 {
			t.Errorf("bind address = %s:%d after write-back, want 127.0.0.1:41877",
				cfg.API.Host, cfg.API.Port)
		}
		if cfg.API.PublicBaseURL != "https://example.com/agent" {
			t.Errorf("PublicBaseURL = %q after write-back, want it unchanged", cfg.API.PublicBaseURL)
		}
	})
}

// ---------------------------------------------------------------------------
// Task store: concurrency and retention
// ---------------------------------------------------------------------------

// newTaskServer wires a handler with the given loop onto a real HTTP server.
func newTaskServer(t *testing.T, cfg *config.Config, loop *agent.Loop) (*httptest.Server, *Handler) {
	t.Helper()

	h := NewHandler(cfg, loop)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv, h
}

func postJSON(t *testing.T, client *http.Client, url, body string) []byte {
	t.Helper()

	resp, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return out
}

func getTaskBody(id string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":"2","method":"tasks/get","params":{"id":%q}}`, id)
}

// currentTaskIDs reports the ids currently retained by the store.
func currentTaskIDs(h *Handler) map[string]bool {
	ids := map[string]bool{}
	h.tasks.Range(func(key, _ any) bool {
		if id, ok := key.(string); ok {
			ids[id] = true
		}
		return true
	})
	return ids
}

func publishedTask(h *Handler, id string) *Task {
	if value, ok := h.tasks.Load(id); ok {
		if task, ok := value.(*Task); ok {
			return task
		}
	}
	return nil
}

// beginTask starts one tasks/send through the real JSON-RPC route and waits
// until the agent has reached the model provider, i.e. until the task is
// published and in the "working" state. It returns the task id and a channel
// carrying the (eventual) response body.
func beginTask(t *testing.T, h *Handler, client *http.Client, baseURL string, prov *gateProvider) (string, <-chan []byte) {
	t.Helper()

	known := currentTaskIDs(h)
	response := make(chan []byte, 1)
	go func() {
		resp, err := client.Post(baseURL+"/a2a", "application/json", strings.NewReader(taskSendBody))
		if err != nil {
			response <- nil
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			body = nil
		}
		response <- body
	}()

	select {
	case <-prov.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent never reached the model provider")
	}

	id := ""
	for candidate := range currentTaskIDs(h) {
		if !known[candidate] {
			id = candidate
		}
	}
	if id == "" {
		t.Fatal("no new task was published for the in-flight request")
	}
	return id, response
}

// Regression: handleTaskSend published a *Task into the store and then kept
// mutating that same struct on completion, while handleTaskGet could be
// serialising it. A tasks/get concurrent with a completion therefore read fields
// being written — a data race, and an inconsistent snapshot even without the
// detector.
func TestTaskGetDoesNotRaceWithTaskCompletion(t *testing.T) {
	prov := newGateProvider()
	srv, h := newTaskServer(t, config.DefaultConfig(), newTestLoop(t, prov))
	client := srv.Client()

	var currentID atomic.Value
	stop := make(chan struct{})
	var readers sync.WaitGroup
	var reads atomic.Int64

	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				id, _ := currentID.Load().(string)
				if id == "" {
					time.Sleep(time.Millisecond)
					continue
				}
				resp, err := client.Post(srv.URL+"/a2a", "application/json", strings.NewReader(getTaskBody(id)))
				if err != nil {
					time.Sleep(time.Millisecond)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				reads.Add(1)
			}
		}()
	}
	defer func() {
		close(stop)
		readers.Wait()
	}()

	// Each round widens the window the way production does: the task sits in the
	// store as "working" while a peer polls tasks/get, and is then completed.
	for round := 0; round < 10; round++ {
		id, response := beginTask(t, h, client, srv.URL, prov)
		currentID.Store(id)
		time.Sleep(20 * time.Millisecond)
		prov.releaseAll()
		if body := <-response; body == nil {
			t.Fatal("tasks/send did not answer")
		}
	}

	if reads.Load() == 0 {
		t.Fatal("no tasks/get request completed: the race window was never exercised")
	}
}

// Regression: a published snapshot was the very struct the handler kept writing
// to, so a reader could see a completed status carrying the previous empty
// output. Published snapshots must be immutable.
func TestPublishedTaskIsNeverMutatedInPlace(t *testing.T) {
	prov := newGateProvider()
	srv, h := newTaskServer(t, config.DefaultConfig(), newTestLoop(t, prov))
	client := srv.Client()

	id, response := beginTask(t, h, client, srv.URL, prov)

	working := publishedTask(h, id)
	if working == nil {
		t.Fatal("the in-flight task was not published")
	}
	if working.Status != "working" {
		t.Fatalf("published task status=%q want \"working\"", working.Status)
	}

	prov.releaseAll()
	if body := <-response; body == nil {
		t.Fatal("tasks/send did not answer")
	}

	if working.Status != "working" || working.Output != "" {
		t.Fatalf("a published snapshot was mutated in place: status=%q output=%q",
			working.Status, working.Output)
	}

	completed := publishedTask(h, id)
	if completed == nil {
		t.Fatal("the completed task is missing from the store")
	}
	if completed == working {
		t.Fatal("the completion was written into the published snapshot instead of a new one")
	}
	if completed.Status != "completed" || completed.Output == "" {
		t.Fatalf("stored task status=%q output=%q want a completed task with output",
			completed.Status, completed.Output)
	}
}

// Regression: nothing ever removed a task, so every task ever submitted was
// retained for the lifetime of the process, including its full input and output.
// A burst must not blow past the hard cap even before any TTL elapses.
func TestTaskStoreIsBounded(t *testing.T) {
	// A nil loop keeps each task instantaneous; the subject here is the store.
	srv, h := newTaskServer(t, config.DefaultConfig(), nil)
	client := srv.Client()

	const submitted = 3000
	lastID := ""
	for i := 0; i < submitted; i++ {
		body := postJSON(t, client, srv.URL+"/a2a", taskSendBody)
		var parsed struct {
			Result struct {
				ID string `json:"id"`
			} `json:"result"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("decode tasks/send response %s: %v", body, err)
		}
		if parsed.Result.ID == "" {
			t.Fatalf("tasks/send answered without a task id: %s", body)
		}
		lastID = parsed.Result.ID
	}

	retained := len(currentTaskIDs(h))
	if retained > maxRetainedTasks {
		t.Fatalf("task store retained %d entries after %d tasks, want at most %d",
			retained, submitted, maxRetainedTasks)
	}
	if retained == 0 {
		t.Fatal("task store retained nothing: the bound must not evict live work")
	}

	// The bound must not cost a peer the task it just submitted.
	body := postJSON(t, client, srv.URL+"/a2a", getTaskBody(lastID))
	var parsed struct {
		Result *Task         `json:"result"`
		Error  *JSONRPCError `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode tasks/get response %s: %v", body, err)
	}
	if parsed.Result == nil {
		t.Fatalf("the most recently submitted task was evicted: %s", body)
	}
	if parsed.Result.Status != "completed" {
		t.Fatalf("retained task status=%q want \"completed\"", parsed.Result.Status)
	}
}

// TestTaskStoreNeverExceedsCapDuringBurst is the HARD-bound contract: the
// retained count must never exceed maxRetainedTasks at ANY instant, not merely
// once a burst has been trimmed back.
//
// Regression: publish inserted first and enforced the cap afterwards, and every
// publisher trimmed independently, so N concurrent publishers could hold the
// store at up to cap+N entries until the next trim walked it back — and each of
// those publishers paid for a full walk. The sampler below runs WHILE the burst
// is in flight, so the assertion is about every observed instant rather than
// about the end state, which the amortised implementation already satisfied.
// Observed at the pre-fix revision with these parameters: 1025..1029.
//
// The observable is h.retained rather than a lock-free count of h.tasks.Range:
// sync.Map.Range "does not necessarily correspond to any consistent snapshot of
// the Map's contents", so a concurrent walk can count entries that were never
// live at the same instant. Measured while developing this test: a lock-free
// walk reported 1028 and 1034 for a map that, counted under the store's own
// mutex, never held more than 1024 entries. Asserting on the walk would be
// asserting on an artifact of the iteration, not on the store.
func TestTaskStoreNeverExceedsCapDuringBurst(t *testing.T) {
	// A nil loop keeps each task instantaneous; the subject here is the store.
	h := NewHandler(config.DefaultConfig(), nil)

	const (
		publishers   = 32
		perPublisher = 200
	)

	var (
		wg         sync.WaitGroup
		sampler    sync.WaitGroup
		maxCounted atomic.Int64
	)
	stop := make(chan struct{})

	// The sampler reads the retained count in a tight loop: the overshoot window
	// is short, so it must not do anything expensive between reads.
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if n := h.retained.Load(); n > maxCounted.Load() {
				maxCounted.Store(n)
			}
		}
	}()

	start := make(chan struct{})
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			<-start
			for i := 0; i < perPublisher; i++ {
				h.publish(Task{
					ID:        fmt.Sprintf("burst-%d-%d", p, i),
					Status:    "completed",
					Input:     "burst",
					Output:    "burst",
					CreatedAt: time.Now(),
					UpdatedAt: time.Now(),
				})
			}
		}(p)
	}
	close(start)
	wg.Wait()
	close(stop)
	sampler.Wait()

	// The sampler must have observed the store in its bounded region, or the
	// assertion below would be vacuous.
	if got := maxCounted.Load(); got < taskTrimTarget {
		t.Fatalf("the sampler only ever saw %d retained tasks, want the burst to fill the store "+
			"past the low-water mark of %d: the test did not exercise the bound", got, taskTrimTarget)
	}
	if got := maxCounted.Load(); got > maxRetainedTasks {
		t.Fatalf("the retained count reached %d while %d publishers were publishing (cap %d): "+
			"the bound is amortised, not a ceiling — a publisher inserted first and trimmed afterwards",
			got, publishers, maxRetainedTasks)
	}

	// The end state must be bounded and the counter must not have drifted from
	// the map it counts. Nothing is publishing any more, so the walk below is a
	// true count of the store.
	if got := h.retained.Load(); got > maxRetainedTasks {
		t.Fatalf("retained count is %d after the burst, want at most %d", got, maxRetainedTasks)
	}
	stored := int64(0)
	h.tasks.Range(func(_, _ any) bool { stored++; return true })
	if stored != h.retained.Load() {
		t.Fatalf("the store holds %d tasks but the retained counter says %d", stored, h.retained.Load())
	}
	if stored == 0 {
		t.Fatal("the burst left the store empty: the bound must not evict live work")
	}
}

// A hard cap must not be paid for with the work in flight: with the store at its
// cap, admitting one more task evicts terminal tasks, even though the running
// task is the oldest entry in the store.
func TestTaskCapEvictsTerminalTasksBeforeRunningOnes(t *testing.T) {
	h := NewHandler(config.DefaultConfig(), nil)
	now := time.Now()

	// Seed the store at its cap directly: the OLDEST entry is the running one,
	// so a plain oldest-first policy would evict exactly the wrong task.
	h.tasks.Store("running", &Task{ID: "running", Status: "working", UpdatedAt: now.Add(-time.Hour)})
	for i := 0; i < maxRetainedTasks-1; i++ {
		id := fmt.Sprintf("done-%d", i)
		h.tasks.Store(id, &Task{ID: id, Status: "completed", UpdatedAt: now})
	}
	h.retained.Store(maxRetainedTasks)

	h.publish(Task{ID: "fresh", Status: "working", CreatedAt: now, UpdatedAt: now})

	if _, ok := h.tasks.Load("running"); !ok {
		t.Fatal("the trim evicted an in-flight task while terminal tasks were available to evict")
	}
	if _, ok := h.tasks.Load("fresh"); !ok {
		t.Fatal("the task that triggered the trim was itself evicted")
	}
	if got := h.retained.Load(); got > maxRetainedTasks {
		t.Fatalf("retained count is %d after the admission trim, want at most %d", got, maxRetainedTasks)
	}
}

// When every retained task is still running the cap still has to hold: the
// policy allows in-flight tasks to be dropped as a last resort, and a peer that
// loses its entry gets "Task not found" rather than an unbounded store.
func TestTaskCapHoldsWhenEveryTaskIsRunning(t *testing.T) {
	h := NewHandler(config.DefaultConfig(), nil)
	now := time.Now()

	for i := 0; i < maxRetainedTasks; i++ {
		id := fmt.Sprintf("running-%d", i)
		h.tasks.Store(id, &Task{ID: id, Status: "working", CreatedAt: now, UpdatedAt: now})
	}
	h.retained.Store(maxRetainedTasks)

	h.publish(Task{ID: "fresh", Status: "working", CreatedAt: now, UpdatedAt: now})

	if got := h.retained.Load(); got > maxRetainedTasks {
		t.Fatalf("retained count is %d, want at most %d even when nothing is terminal", got, maxRetainedTasks)
	}
	if _, ok := h.tasks.Load("fresh"); !ok {
		t.Fatal("the task that triggered the trim was itself evicted")
	}
}

// The TTL pass drops terminal tasks once the retention window has passed, and
// leaves work that is still running alone. The test rewinds the sweep clock
// instead of waiting out the real TTL.
func TestTaskStoreSweepsExpiredTerminalTasks(t *testing.T) {
	h := NewHandler(config.DefaultConfig(), nil)
	now := time.Now()

	h.tasks.Store("old-completed", &Task{ID: "old-completed", Status: "completed", UpdatedAt: now.Add(-2 * taskRetentionTTL)})
	h.tasks.Store("fresh-completed", &Task{ID: "fresh-completed", Status: "completed", UpdatedAt: now})
	h.tasks.Store("old-working", &Task{ID: "old-working", Status: "working", UpdatedAt: now.Add(-2 * taskRetentionTTL)})
	h.retained.Store(3)
	h.lastSweepNanos.Store(0) // force the next sweep instead of waiting out taskSweepInterval

	h.sweepTasks(now)

	if _, ok := h.tasks.Load("old-completed"); ok {
		t.Fatal("an expired terminal task was retained")
	}
	if _, ok := h.tasks.Load("fresh-completed"); !ok {
		t.Fatal("a terminal task inside the retention window was swept")
	}
	if _, ok := h.tasks.Load("old-working"); !ok {
		t.Fatal("the TTL pass must not sweep a task that is still working")
	}
}
