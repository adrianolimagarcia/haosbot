package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// This file is the defect probe for Telegram webhook mode. It deliberately
// references no symbol introduced by the fix, so the identical file runs both
// before and after: pre-fix it fails with the raw evidence that a channel
// configured for webhook mode registered no webhook and long-polled instead.

// ---------------------------------------------------------------------------
// Fake Bot API
//
// A stand-in for api.telegram.org that records which transport calls the
// channel makes. That recording is the evidence for the defect: a channel
// configured for webhook mode must call setWebhook and must never call
// getUpdates.
// ---------------------------------------------------------------------------

type fakeBotAPI struct {
	srv *httptest.Server

	mu            sync.Mutex
	setWebhook    []SetWebhookParams
	deleteWebhook int
	getUpdates    int
}

func newFakeBotAPI(t *testing.T, firstPollUpdate string) *fakeBotAPI {
	t.Helper()
	return newFakeBotAPIWithSetWebhookError(t, firstPollUpdate, "")
}

// newFakeBotAPIWithSetWebhookError builds a Bot API stand-in whose setWebhook
// answers 400 with setWebhookError. firstPollUpdate, when non-empty, is the
// Update returned by the first getUpdates call.
func newFakeBotAPIWithSetWebhookError(t *testing.T, firstPollUpdate, setWebhookError string) *fakeBotAPI {
	t.Helper()

	f := &fakeBotAPI{}
	mux := http.NewServeMux()

	writeOK := func(w http.ResponseWriter, result any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}

	mux.HandleFunc("/botTESTTOKEN/getMe", func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, map[string]any{
			"id":         9876,
			"is_bot":     true,
			"first_name": "TestBot",
			"username":   "test_bot",
		})
	})

	mux.HandleFunc("/botTESTTOKEN/setMyCommands", func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, true)
	})

	mux.HandleFunc("/botTESTTOKEN/setWebhook", func(w http.ResponseWriter, r *http.Request) {
		var payload SetWebhookParams
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)

		f.mu.Lock()
		f.setWebhook = append(f.setWebhook, payload)
		f.mu.Unlock()

		if setWebhookError != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": false, "error_code": 400, "description": setWebhookError,
			})
			return
		}
		writeOK(w, true)
	})

	mux.HandleFunc("/botTESTTOKEN/deleteWebhook", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deleteWebhook++
		f.mu.Unlock()
		writeOK(w, true)
	})

	var servedOnce sync.Once
	mux.HandleFunc("/botTESTTOKEN/getUpdates", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.getUpdates++
		f.mu.Unlock()

		if firstPollUpdate != "" {
			var result any = []any{}
			servedOnce.Do(func() {
				var parsed any
				if err := json.Unmarshal([]byte(firstPollUpdate), &parsed); err != nil {
					t.Errorf("bad fixture update: %v", err)
					return
				}
				result = []any{parsed}
			})
			writeOK(w, result)
			return
		}

		// Not a hot loop: an unbounded 200 here would let the pre-fix probe spin
		// the CPU while it proves the channel fell back to polling.
		time.Sleep(25 * time.Millisecond)
		writeOK(w, []any{})
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBotAPI) setWebhookCalls() []SetWebhookParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SetWebhookParams(nil), f.setWebhook...)
}

func (f *fakeBotAPI) deleteWebhookCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleteWebhook
}

func (f *fakeBotAPI) getUpdatesCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getUpdates
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

const (
	webhookTestSecret = "s3cret-token_9"
	webhookTestURL    = "https://example.com/telegram"
	// secretHeaderName is Telegram's webhook secret header, spelled out here so
	// this probe does not depend on any constant the fix introduces.
	secretHeaderName = "X-Telegram-Bot-Api-Secret-Token"
)

// freePort reserves a port and releases it, so the channel under test can bind
// it. The window is racy in principle; in practice a failure here is loud.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return port
}

// newWebhookChannel builds a channel whose config asks for webhook mode, wired
// to the fake Bot API.
func newWebhookChannel(t *testing.T, f *fakeBotAPI, bus channels.InboundPublisher, port int) *Channel {
	t.Helper()

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

	ch, err := New(sec, bus)
	if err != nil {
		t.Fatalf("New channel failed: %v", err)
	}
	if ch.cfg.Mode != "webhook" {
		t.Fatalf("precondition: config mode = %q, want webhook", ch.cfg.Mode)
	}
	ch.SetClient(NewBotClient("TESTTOKEN", "", WithBaseURL(f.srv.URL)))
	return ch
}

// startChannel runs Start in the background. The returned function blocks until
// Start returns, or until the timeout elapses, and reports Start's error along
// with whether it returned at all.
//
// "Start is still polling" IS the defect, so a test must be able to tell a real
// startup error apart from a channel that never came back. A helper that
// fabricated an error on timeout would let the pre-fix code pass.
func startChannel(t *testing.T, ch *Channel, ctx context.Context) func(time.Duration) (error, bool) {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- ch.Start(ctx) }()

	var once sync.Once
	var startErr error
	var returned bool

	wait := func(timeout time.Duration) (error, bool) {
		select {
		case err := <-done:
			once.Do(func() { startErr, returned = err, true })
		case <-time.After(timeout):
			once.Do(func() { startErr, returned = nil, false })
		}
		return startErr, returned
	}

	// Unblock Start even if a test forgets to. Registered after the fake API's
	// own cleanup, so it runs first (t.Cleanup is LIFO).
	t.Cleanup(func() { _ = ch.Stop(context.Background()) })
	return wait
}

// waitForWebhookRegistration blocks until the channel has registered its
// webhook, or fails with the transport it actually used.
func waitForWebhookRegistration(t *testing.T, f *fakeBotAPI) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.setWebhookCalls()) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("setWebhook was never called; getUpdates was called %d time(s) — the channel fell back to long polling",
		f.getUpdatesCalls())
}

// waitForListener blocks until something answers on the webhook path.
func waitForListener(t *testing.T, url string) {
	t.Helper()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Post(url, "application/json", strings.NewReader("{}"))
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no HTTP listener ever answered on %s", url)
}

func postUpdate(t *testing.T, url, secret, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set(secretHeaderName, secret)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// webhookUpdateJSON is one Telegram Update, shared by the webhook tests and the
// polling/webhook equivalence test so both feed byte-identical input.
func webhookUpdateJSON(updateID, messageID int, text string) string {
	return fmt.Sprintf(`{
		"update_id": %d,
		"message": {
			"message_id": %d,
			"text": %q,
			"from": {"id": 111, "first_name": "Bob", "username": "bob"},
			"chat": {"id": 555, "type": "private"}
		}
	}`, updateID, messageID, text)
}

// waitForInbound waits for the bus to record at least n inbound messages.
func waitForInbound(t *testing.T, bus *recordingPublisher, n int) []core.InboundMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := bus.snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return bus.snapshot()
}

// ---------------------------------------------------------------------------
// The defect probe
// ---------------------------------------------------------------------------

// TestWebhookMode_RegistersAndServes is the probe for the defect: a channel
// configured with mode "webhook" must register its webhook and serve it, and
// must not long-poll.
func TestWebhookMode_RegistersAndServes(t *testing.T) {
	f := newFakeBotAPI(t, "")
	bus := &recordingPublisher{}
	port := freePort(t)

	ch := newWebhookChannel(t, f, bus, port)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := startChannel(t, ch, ctx)

	waitForWebhookRegistration(t, f)

	calls := f.setWebhookCalls()
	if len(calls) != 1 {
		t.Fatalf("setWebhook called %d times, want 1", len(calls))
	}
	got := calls[0]
	if got.URL != webhookTestURL {
		t.Errorf("setWebhook url = %q, want %q", got.URL, webhookTestURL)
	}
	if got.SecretToken != webhookTestSecret {
		t.Errorf("setWebhook secret_token = %q, want %q", got.SecretToken, webhookTestSecret)
	}
	if got.MaxConnections != 4 {
		t.Errorf("setWebhook max_connections = %d, want 4", got.MaxConnections)
	}
	if !reflect.DeepEqual(got.AllowedUpdates, []string{"message"}) {
		t.Errorf("setWebhook allowed_updates = %v, want [message]", got.AllowedUpdates)
	}
	if got.DropPendingUpdates {
		t.Error("setWebhook drop_pending_updates = true, want false (process pending on startup)")
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/telegram", port)
	waitForListener(t, url)

	resp := postUpdate(t, url, webhookTestSecret, webhookUpdateJSON(10, 501, "Hello via webhook"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST update status = %d, want 200", resp.StatusCode)
	}

	recorded := waitForInbound(t, bus, 1)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 inbound message, got %d", len(recorded))
	}
	if recorded[0].Content != "Hello via webhook" {
		t.Errorf("inbound content = %q, want %q", recorded[0].Content, "Hello via webhook")
	}

	if n := f.getUpdatesCalls(); n != 0 {
		t.Errorf("getUpdates called %d time(s) in webhook mode, want 0", n)
	}

	cancel()
	err, returned := stop(10 * time.Second)
	if !returned {
		t.Fatal("Start did not return after the lifecycle context was cancelled")
	}
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
}

// TestWebhookAndPollingDispatchIdentically pins the requirement that both modes
// funnel the same Update through the same downstream handler.
func TestWebhookAndPollingDispatchIdentically(t *testing.T) {
	update := webhookUpdateJSON(20, 601, "identical dispatch")

	// Polling.
	pollFake := newFakeBotAPI(t, update)
	pollBus := &recordingPublisher{}
	pollSec := channels.NewMapSection(map[string]any{
		"token":      "TESTTOKEN",
		"mode":       "polling",
		"allow_from": []any{"*"},
	})
	pollCh, err := New(pollSec, pollBus)
	if err != nil {
		t.Fatalf("New polling channel: %v", err)
	}
	pollCh.SetClient(NewBotClient("TESTTOKEN", "", WithBaseURL(pollFake.srv.URL)))

	pollCtx, pollCancel := context.WithCancel(context.Background())
	pollStop := startChannel(t, pollCh, pollCtx)
	polled := waitForInbound(t, pollBus, 1)
	pollCancel()
	_, _ = pollStop(10 * time.Second)
	if len(polled) != 1 {
		t.Fatalf("polling mode produced %d inbound messages, want 1", len(polled))
	}

	// Webhook.
	whFake := newFakeBotAPI(t, "")
	whBus := &recordingPublisher{}
	port := freePort(t)
	whCh := newWebhookChannel(t, whFake, whBus, port)

	whCtx, whCancel := context.WithCancel(context.Background())
	defer whCancel()
	whStop := startChannel(t, whCh, whCtx)

	waitForWebhookRegistration(t, whFake)
	url := fmt.Sprintf("http://127.0.0.1:%d/telegram", port)
	waitForListener(t, url)

	if resp := postUpdate(t, url, webhookTestSecret, update); resp.StatusCode != http.StatusOK {
		t.Fatalf("POST update status = %d, want 200", resp.StatusCode)
	}

	webhooked := waitForInbound(t, whBus, 1)
	if len(webhooked) != 1 {
		t.Fatalf("webhook mode produced %d inbound messages, want 1", len(webhooked))
	}

	// Timestamp is wall-clock and cannot match across two runs; every field the
	// dispatcher derives from the Update must.
	polled[0].Timestamp = time.Time{}
	webhooked[0].Timestamp = time.Time{}

	if !reflect.DeepEqual(polled[0], webhooked[0]) {
		t.Errorf("webhook and polling dispatch diverged:\n polling: %+v\n webhook: %+v", polled[0], webhooked[0])
	}

	whCancel()
	if err, returned := whStop(10 * time.Second); !returned || err != nil {
		t.Fatalf("Start returned error after shutdown: returned=%v err=%v", returned, err)
	}
}

// ---------------------------------------------------------------------------
// Authentication (security boundary)
// ---------------------------------------------------------------------------

func TestWebhookMode_RejectsWrongSecret(t *testing.T) {
	runWebhookAuthCase(t, "wrong-secret", "not-the-secret", http.StatusUnauthorized)
}

func TestWebhookMode_RejectsMissingSecret(t *testing.T) {
	runWebhookAuthCase(t, "missing-secret", "", http.StatusUnauthorized)
}

// runWebhookAuthCase starts a webhook channel and posts one update with the
// given secret, asserting the status and that nothing was dispatched.
func runWebhookAuthCase(t *testing.T, name, secret string, wantStatus int) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
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

		resp := postUpdate(t, url, secret, webhookUpdateJSON(30, 701, "should not dispatch"))
		if resp.StatusCode != wantStatus {
			t.Errorf("status = %d, want %d", resp.StatusCode, wantStatus)
		}

		// An unauthenticated request must not reach the dispatcher.
		time.Sleep(400 * time.Millisecond)
		if got := bus.snapshot(); len(got) != 0 {
			t.Errorf("unauthenticated request was dispatched: %+v", got)
		}

		cancel()
		_, _ = stop(10 * time.Second)
	})
}

// ---------------------------------------------------------------------------
// Routing and body handling
// ---------------------------------------------------------------------------

func TestWebhookMode_WrongPathIs404(t *testing.T) {
	f := newFakeBotAPI(t, "")
	bus := &recordingPublisher{}
	port := freePort(t)
	ch := newWebhookChannel(t, f, bus, port)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := startChannel(t, ch, ctx)

	waitForWebhookRegistration(t, f)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForListener(t, base+"/telegram")

	for _, path := range []string{"/", "/nope", "/telegram/extra", "/telegramx"} {
		resp := postUpdate(t, base+path, webhookTestSecret, webhookUpdateJSON(40, 801, "wrong path"))
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s status = %d, want 404", path, resp.StatusCode)
		}
	}

	time.Sleep(400 * time.Millisecond)
	if got := bus.snapshot(); len(got) != 0 {
		t.Errorf("a request on a non-webhook path was dispatched: %+v", got)
	}

	cancel()
	_, _ = stop(10 * time.Second)
}

func TestWebhookMode_MalformedBodyRejected(t *testing.T) {
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

	for _, body := range []string{"", "{", "{not json", `{"update_id": "not a number"`, `[]`, "null", `{"foo":1}`} {
		resp := postUpdate(t, url, webhookTestSecret, body)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("malformed body %q was accepted with 200", body)
		}
	}

	// The listener must still be alive: a bad body must not take the server down.
	if resp := postUpdate(t, url, webhookTestSecret, webhookUpdateJSON(50, 901, "still alive")); resp.StatusCode != http.StatusOK {
		t.Errorf("listener unhealthy after malformed bodies: status %d", resp.StatusCode)
	}

	cancel()
	_, _ = stop(10 * time.Second)
}

// ---------------------------------------------------------------------------
// Shutdown
// ---------------------------------------------------------------------------

func TestWebhookMode_ShutdownDeletesWebhookAndStopsListener(t *testing.T) {
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

	// Shutdown is Stop, not a bare context cancellation: the requirement is that
	// Stop deregisters the webhook and stops the listener.
	if err := ch.Stop(context.Background()); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}

	err, returned := stop(10 * time.Second)
	if !returned {
		t.Fatal("Start did not return after Stop")
	}
	if err != nil {
		t.Fatalf("Start returned error after shutdown: %v", err)
	}

	if n := f.deleteWebhookCalls(); n != 1 {
		t.Errorf("deleteWebhook called %d time(s) on shutdown, want 1", n)
	}

	// The listener must be gone: a fresh connection is refused, not answered.
	if _, err := (&http.Client{Timeout: time.Second}).Post(url, "application/json", strings.NewReader("{}")); err == nil {
		t.Error("listener still accepting connections after shutdown")
	}

	// Stop is idempotent.
	if err := ch.Stop(context.Background()); err != nil {
		t.Errorf("second Stop returned error: %v", err)
	}
	if n := f.deleteWebhookCalls(); n != 1 {
		t.Errorf("deleteWebhook called %d time(s) after a second Stop, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Startup failure
// ---------------------------------------------------------------------------

func TestWebhookMode_BindFailureIsClearStartupError(t *testing.T) {
	f := newFakeBotAPI(t, "")

	// Occupy the port the channel is configured to bind.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
	if !returned {
		t.Fatal("Start never returned for an already-bound port; want a clear startup error")
	}
	if startErr == nil {
		t.Fatal("Start returned nil for an already-bound port; want a clear startup error")
	}
	if !strings.Contains(startErr.Error(), fmt.Sprintf("%d", port)) {
		t.Errorf("Start error %q does not name the port %d", startErr.Error(), port)
	}

	// The whole point of the fix: a startup failure must never degrade into
	// long polling.
	if n := f.getUpdatesCalls(); n != 0 {
		t.Errorf("getUpdates called %d time(s) after a webhook bind failure, want 0", n)
	}

	if msg := ch.StartErrorMessage(startErr); msg == "" {
		t.Error("StartErrorMessage returned empty for a webhook startup failure")
	}
}

func TestWebhookMode_SetWebhookFailureIsClearStartupError(t *testing.T) {
	f := newFakeBotAPIWithSetWebhookError(t, "", "Bad Request: bad webhook: Failed to resolve host")
	bus := &recordingPublisher{}
	port := freePort(t)
	ch := newWebhookChannel(t, f, bus, port)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := startChannel(t, ch, ctx)

	startErr, returned := stop(10 * time.Second)
	if !returned {
		t.Fatal("Start never returned when setWebhook was rejected; want a clear startup error")
	}
	if startErr == nil {
		t.Fatal("Start returned nil when setWebhook was rejected; want a clear startup error")
	}
	if n := f.getUpdatesCalls(); n != 0 {
		t.Errorf("getUpdates called %d time(s) after setWebhook failed, want 0", n)
	}

	// The failed startup must not leave the port bound.
	url := fmt.Sprintf("http://127.0.0.1:%d/telegram", port)
	if _, err := (&http.Client{Timeout: time.Second}).Post(url, "application/json", strings.NewReader("{}")); err == nil {
		t.Error("listener still bound after a failed startup")
	}
}
