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

// blockingProvider never completes on its own: it waits for the context it was
// handed to be cancelled and reports both the deadline it was given and the
// cancellation it observed. That makes the two things the A2A handler is
// responsible for — forwarding the request's cancellation and applying the
// configured per-request bound — directly observable.
type blockingProvider struct {
	entered   chan time.Duration
	cancelled chan error
	release   chan struct{}
	once      sync.Once
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{
		entered:   make(chan time.Duration, 4),
		cancelled: make(chan error, 4),
		release:   make(chan struct{}),
	}
}

func (p *blockingProvider) Chat(ctx context.Context, _ provider.ChatRequest) (*core.Response, error) {
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
	case <-p.release:
		return nil, errors.New("test released the blocking provider")
	}

	err := ctx.Err()
	select {
	case p.cancelled <- err:
	default:
	}
	return nil, err
}

func (p *blockingProvider) Name() string { return "blocking-test" }

func (p *blockingProvider) releaseOnce() { p.once.Do(func() { close(p.release) }) }

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

// startTask posts one tasks/send request to a real httptest server. The agent's
// provider blocks until its context is cancelled, so the caller can decide when
// (and whether) the work stops.
func startTask(t *testing.T, cfg *config.Config, prov *blockingProvider) *taskRun {
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

// ---------------------------------------------------------------------------
// Cancellation
// ---------------------------------------------------------------------------

// Regression: the task context used to be built from context.Background()
// (handler.go:220). A client that disconnected therefore did not stop the turn:
// the agent kept calling the model and held its concurrency slot until the full
// 120s bound elapsed.
func TestTaskSendStopsWhenClientDisconnects(t *testing.T) {
	prov := newBlockingProvider()
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
			prov := newBlockingProvider()
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
