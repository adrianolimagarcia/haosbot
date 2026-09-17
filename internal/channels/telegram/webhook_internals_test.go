package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

// These tests cover the implementation details the defect probe
// (webhook_test.go) deliberately cannot reference, because that probe must
// compile and run against the pre-fix tree as well. They were added after the
// fix; webhook_test.go itself is unchanged across the before/after runs.

func TestWebhookRoute(t *testing.T) {
	cases := []struct{ path, want string }{
		// The reference passes webhook_path.lstrip("/") as PTB's url_path, and
		// PTB prepends "/" when it is missing: route == "/" + path.lstrip("/").
		{"/telegram", "/telegram"},
		{"telegram", "/telegram"},
		{"//telegram", "/telegram"},
		{"/hook/", "/hook/"},
		{"/", "/"},
		{"", "/"},
	}
	for _, tc := range cases {
		if got := webhookRoute(tc.path); got != tc.want {
			t.Errorf("webhookRoute(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestWebhookReceiver_FailsClosedOnEmptySecret pins the security boundary: an
// empty expectation must never be satisfiable by an empty header. Config
// validation makes an empty secret unreachable in webhook mode, so this is
// defence in depth for the case where the receiver is constructed directly.
func TestWebhookReceiver_FailsClosedOnEmptySecret(t *testing.T) {
	r := &webhookReceiver{route: "/telegram", secret: ""}

	req, err := http.NewRequest(http.MethodPost, "/telegram", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if r.authenticated(req) {
		t.Error("an empty configured secret accepted a request with no secret header")
	}

	req.Header.Set(webhookSecretHeader, "")
	if r.authenticated(req) {
		t.Error("an empty configured secret accepted an empty secret header")
	}

	req.Header.Set(webhookSecretHeader, "anything")
	if r.authenticated(req) {
		t.Error("an empty configured secret accepted a non-empty secret header")
	}
}

func TestWebhookReceiver_ConstantTimeCompareRejectsPrefixes(t *testing.T) {
	r := &webhookReceiver{route: "/telegram", secret: "abcdef"}

	for _, presented := range []string{"abcde", "abcdefg", "abcdeX", "ABCDEF", "abcde "} {
		req, err := http.NewRequest(http.MethodPost, "/telegram", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set(webhookSecretHeader, presented)
		if r.authenticated(req) {
			t.Errorf("secret %q accepted against %q", presented, "abcdef")
		}
	}
}

// TestWebhookMode_GETReturns405 mirrors PTB's SUPPORTED_METHODS = ("POST",).
func TestWebhookMode_GETReturns405(t *testing.T) {
	f := newFakeBotAPI(t, "")
	bus := &recordingPublisher{}
	port := freePort(t)
	ch := newWebhookChannel(t, f, bus, port)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := startChannel(t, ch, ctx)

	waitForWebhookRegistration(t, f)
	url := fmt.Sprintf("http://127.0.0.1:%d/telegram", port)
	waitForListener(t, url)

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", resp.StatusCode)
	}

	cancel()
	_, _ = stop(10 * time.Second)
}

func TestWebhookMode_RejectsOversizedBody(t *testing.T) {
	f := newFakeBotAPI(t, "")
	bus := &recordingPublisher{}
	port := freePort(t)
	ch := newWebhookChannel(t, f, bus, port)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := startChannel(t, ch, ctx)

	waitForWebhookRegistration(t, f)
	url := fmt.Sprintf("http://127.0.0.1:%d/telegram", port)
	waitForListener(t, url)

	huge := `{"update_id":1,"pad":"` + strings.Repeat("x", webhookMaxBodyBytes+1024) + `"}`
	resp := postUpdate(t, url, webhookTestSecret, huge)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body status = %d, want 413", resp.StatusCode)
	}

	time.Sleep(300 * time.Millisecond)
	if got := bus.snapshot(); len(got) != 0 {
		t.Errorf("oversized body was dispatched: %d messages", len(got))
	}

	cancel()
	_, _ = stop(10 * time.Second)
}

func TestWebhookMode_RejectsTrailingData(t *testing.T) {
	f := newFakeBotAPI(t, "")
	bus := &recordingPublisher{}
	port := freePort(t)
	ch := newWebhookChannel(t, f, bus, port)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := startChannel(t, ch, ctx)

	waitForWebhookRegistration(t, f)
	url := fmt.Sprintf("http://127.0.0.1:%d/telegram", port)
	waitForListener(t, url)

	body := webhookUpdateJSON(60, 1001, "first") + webhookUpdateJSON(61, 1002, "second")
	if resp := postUpdate(t, url, webhookTestSecret, body); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("trailing-data status = %d, want 400", resp.StatusCode)
	}

	time.Sleep(300 * time.Millisecond)
	if got := bus.snapshot(); len(got) != 0 {
		t.Errorf("a body with trailing data was dispatched: %d messages", len(got))
	}

	cancel()
	_, _ = stop(10 * time.Second)
}

// TestWebhookMode_StartupErrorIsTerminal pins that a webhook startup failure is
// reported as errWebhookStartup — the marker Start uses to stop retrying and
// return, instead of spinning forever or degrading into long polling.
func TestWebhookMode_StartupErrorIsTerminal(t *testing.T) {
	f := newFakeBotAPI(t, "")

	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	bus := &recordingPublisher{}
	ch := newWebhookChannel(t, f, bus, port)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := startChannel(t, ch, ctx)

	startErr, returned := stop(10 * time.Second)
	if !returned || startErr == nil {
		t.Fatalf("want a terminal startup error, got returned=%v err=%v", returned, startErr)
	}
	if !errors.Is(startErr, errWebhookStartup) {
		t.Errorf("Start error %v is not errWebhookStartup", startErr)
	}

	msg := ch.StartErrorMessage(startErr)
	if !strings.Contains(msg, "webhook") {
		t.Errorf("StartErrorMessage = %q, want it to name webhook mode", msg)
	}
	if !strings.Contains(msg, fmt.Sprintf("%d", port)) {
		t.Errorf("StartErrorMessage = %q, want it to name the port %d", msg, port)
	}
}

// TestWebhookMode_InlineKeyboardsExtendAllowedUpdates guards the shared
// allowedUpdates helper: the webhook transport must register the same
// allowed_updates the polling transport asks for. Duplicating the rule is how
// the two modes drift apart.
func TestWebhookMode_InlineKeyboardsExtendAllowedUpdates(t *testing.T) {
	f := newFakeBotAPI(t, "")
	bus := &recordingPublisher{}
	port := freePort(t)

	sec := channels.NewMapSection(map[string]any{
		"token":                 "TESTTOKEN",
		"mode":                  "webhook",
		"allow_from":            []any{"*"},
		"inlineKeyboards":       true,
		"webhookUrl":            webhookTestURL,
		"webhookSecretToken":    webhookTestSecret,
		"webhookListenHost":     "127.0.0.1",
		"webhookListenPort":     port,
		"webhookPath":           "/telegram",
		"webhookMaxConnections": 4,
	})
	ch, err := New(sec, bus)
	if err != nil {
		t.Fatalf("New channel failed: %v", err)
	}
	ch.SetClient(NewBotClient("TESTTOKEN", "", WithBaseURL(f.srv.URL)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := startChannel(t, ch, ctx)

	waitForWebhookRegistration(t, f)
	calls := f.setWebhookCalls()
	if len(calls) != 1 {
		t.Fatalf("setWebhook called %d times, want 1", len(calls))
	}
	want := []string{"message", "callback_query"}
	if len(calls[0].AllowedUpdates) != len(want) {
		t.Fatalf("allowed_updates = %v, want %v", calls[0].AllowedUpdates, want)
	}
	for i := range want {
		if calls[0].AllowedUpdates[i] != want[i] {
			t.Fatalf("allowed_updates = %v, want %v", calls[0].AllowedUpdates, want)
		}
	}

	cancel()
	_, _ = stop(10 * time.Second)
}

// TestPollingMode_DoesNotRegisterOrBind is the regression guard for the other
// side of the branch: polling must keep deleting the webhook and long-polling,
// and must never bind the webhook listener or call setWebhook.
func TestPollingMode_DoesNotRegisterOrBind(t *testing.T) {
	f := newFakeBotAPI(t, "")
	bus := &recordingPublisher{}
	port := freePort(t)

	sec := channels.NewMapSection(map[string]any{
		"token":             "TESTTOKEN",
		"mode":              "polling",
		"allow_from":        []any{"*"},
		"webhookListenPort": port,
	})
	ch, err := New(sec, bus)
	if err != nil {
		t.Fatalf("New channel failed: %v", err)
	}
	ch.SetClient(NewBotClient("TESTTOKEN", "", WithBaseURL(f.srv.URL)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := startChannel(t, ch, ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && f.deleteWebhookCalls() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	if err, returned := stop(10 * time.Second); !returned || err != nil {
		t.Fatalf("polling Start did not shut down cleanly: returned=%v err=%v", returned, err)
	}

	if n := len(f.setWebhookCalls()); n != 0 {
		t.Errorf("setWebhook called %d time(s) in polling mode, want 0", n)
	}
	if n := f.deleteWebhookCalls(); n == 0 {
		t.Error("polling mode did not delete the webhook before polling")
	}
	if n := f.getUpdatesCalls(); n == 0 {
		t.Error("polling mode never called getUpdates")
	}
	if _, err := (&http.Client{Timeout: 300 * time.Millisecond}).Post(
		fmt.Sprintf("http://127.0.0.1:%d/telegram", port), "application/json", strings.NewReader("{}")); err == nil {
		t.Error("polling mode bound the webhook listener")
	}
}

// syncBuffer is a bytes.Buffer safe to read while the channel's goroutines and
// net/http's error logger are still writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestWebhookMode_NeverLogsTheSecret is the requirement-5 check: the receiver
// must not log the secret token, not even at debug level, including for a
// rejected request that carries it.
func TestWebhookMode_NeverLogsTheSecret(t *testing.T) {
	f := newFakeBotAPI(t, "")
	bus := &recordingPublisher{}
	port := freePort(t)

	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	sec := channels.NewMapSection(map[string]any{
		"token":                 "TESTTOKEN",
		"mode":                  "webhook",
		"allow_from":            []any{"*"},
		"webhookUrl":            webhookTestURL,
		"webhookSecretToken":    webhookTestSecret,
		"webhookListenHost":     "127.0.0.1",
		"webhookListenPort":     port,
		"webhookPath":           "/telegram",
		"webhookMaxConnections": 4,
	})
	ch, err := New(sec, bus, channels.WithLogger(logger))
	if err != nil {
		t.Fatalf("New channel failed: %v", err)
	}
	ch.SetClient(NewBotClient("TESTTOKEN", "", WithBaseURL(f.srv.URL)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := startChannel(t, ch, ctx)

	waitForWebhookRegistration(t, f)
	url := fmt.Sprintf("http://127.0.0.1:%d/telegram", port)
	waitForListener(t, url)

	// Accepted, rejected and malformed requests all carry the header.
	_ = postUpdate(t, url, webhookTestSecret, webhookUpdateJSON(70, 1101, "logged"))
	_ = postUpdate(t, url, "wrong-secret-value", webhookUpdateJSON(71, 1102, "rejected"))
	_ = postUpdate(t, url, webhookTestSecret, "{malformed")

	cancel()
	_, _ = stop(10 * time.Second)

	captured := logs.String()
	if !strings.Contains(captured, "webhook receiver listening") {
		t.Fatalf("no channel logs captured; the assertion below would be vacuous:\n%s", captured)
	}
	if strings.Contains(captured, webhookTestSecret) {
		t.Errorf("the webhook secret appears in the channel log:\n%s", captured)
	}
	if strings.Contains(captured, "wrong-secret-value") {
		t.Errorf("a presented secret appears in the channel log:\n%s", captured)
	}
}

// TestWebhookMode_ConcurrentRequestsAllDispatch exercises the receiver under
// concurrent delivery. Telegram reuses up to webhook_max_connections
// connections and does not serialise them, so a receiver that lost updates or
// raced on shared state would still pass every sequential test above.
func TestWebhookMode_ConcurrentRequestsAllDispatch(t *testing.T) {
	f := newFakeBotAPI(t, "")
	bus := &recordingPublisher{}
	port := freePort(t)
	ch := newWebhookChannel(t, f, bus, port)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := startChannel(t, ch, ctx)

	waitForWebhookRegistration(t, f)
	url := fmt.Sprintf("http://127.0.0.1:%d/telegram", port)
	waitForListener(t, url)

	const n = 24
	var wg sync.WaitGroup
	statuses := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, url,
				strings.NewReader(webhookUpdateJSON(100+i, 2000+i, fmt.Sprintf("msg-%d", i))))
			if err != nil {
				t.Errorf("build request %d: %v", i, err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(secretHeaderName, webhookTestSecret)
			resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
			if err != nil {
				t.Errorf("POST %d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			statuses[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()

	for i, code := range statuses {
		if code != http.StatusOK {
			t.Errorf("request %d status = %d, want 200", i, code)
		}
	}

	recorded := waitForInbound(t, bus, n)
	if len(recorded) != n {
		t.Fatalf("dispatched %d of %d concurrent updates", len(recorded), n)
	}
	seen := make(map[string]bool, n)
	for _, m := range recorded {
		seen[m.Content] = true
	}
	for i := 0; i < n; i++ {
		if !seen[fmt.Sprintf("msg-%d", i)] {
			t.Errorf("update msg-%d was never dispatched", i)
		}
	}

	cancel()
	_, _ = stop(10 * time.Second)
}
