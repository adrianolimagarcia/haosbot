package main

// Regression tests for the gateway's channel wiring.
//
// Before cmd/haosbot/channels.go existed, no code path in the binary touched
// internal/channels at all: `grep -rn "channels\." cmd/` returned nothing, so a
// configuration that enabled Telegram was accepted and silently ignored. These
// tests drive buildChannelManager — the function cmdGateway calls — and assert
// that a configured channel is constructed, registered, started and stopped,
// and that an unconfigured server is unaffected.
//
// No test opens a socket or needs a Telegram credential: the real
// telegram.Channel is driven through an in-process HTTPDoer.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/telegram"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// fakeBotAPI is an in-process stand-in for the Telegram Bot API.
//
// It is an HTTPDoer, so the test opens no socket and api.telegram.org is never
// contacted: telegram.BotClient performs no I/O beyond the HTTPDoer it is
// handed (internal/channels/telegram/client.go:305-330).
type fakeBotAPI struct {
	mu       sync.Mutex
	calls    []string
	released bool
}

func (f *fakeBotAPI) Do(req *http.Request) (*http.Response, error) {
	method := path.Base(req.URL.Path)

	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.mu.Unlock()

	if method == "getUpdates" {
		// The real channel long-polls here. Blocking on the request context
		// keeps the poll loop from spinning, and it is how the test observes
		// that shutdown reached the transport rather than leaking it.
		<-req.Context().Done()
		f.mu.Lock()
		f.released = true
		f.mu.Unlock()
		return nil, req.Context().Err()
	}

	var result any = true
	if method == "getMe" {
		result = map[string]any{
			"id":         42,
			"is_bot":     true,
			"first_name": "Test",
			"username":   "test_bot",
		}
	}
	body, err := json.Marshal(map[string]any{"ok": true, "result": result})
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}, nil
}

func (f *fakeBotAPI) called(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == method {
			return true
		}
	}
	return false
}

func (f *fakeBotAPI) pollReleased() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.released
}

// logCapture records every record the default slog logger handles. The channel
// manager logs through slog.Default(), so this is how a test sees what a
// gateway operator would see on stderr.
type logCapture struct {
	mu      sync.Mutex
	records []string
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" ")
		b.WriteString(a.Key)
		b.WriteString("=")
		b.WriteString(a.Value.String())
		return true
	})
	c.mu.Lock()
	c.records = append(c.records, b.String())
	c.mu.Unlock()
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

func (c *logCapture) contains(substr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, record := range c.records {
		if strings.Contains(record, substr) {
			return true
		}
	}
	return false
}

// captureLogs installs the capture handler on the default logger until the test
// ends. It must run BEFORE the manager is constructed, because the manager
// resolves slog.Default() once (internal/channels/manager.go:271).
func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	capture := &logCapture{}
	previous := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return capture
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// loadConfigFile writes a configuration file and loads it through the real
// loader, so the test exercises the same `channels.<name>` decoding path a
// deployment does — ChannelsConfig declares extra="allow", which is where the
// section is preserved verbatim (internal/config/models.go:426-434).
func loadConfigFile(t *testing.T, body string) *config.Config {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// waitFor polls cond until it holds or the timeout expires. Every wait in this
// file is bounded, so a wiring regression fails the test instead of hanging it.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// newTestBus returns a real in-process bus, which is what cmdGateway passes to
// buildChannelManager (cmd/haosbot/runtime.go).
func newTestBus(t *testing.T) *bus.Bus {
	t.Helper()
	messageBus := bus.New(bus.Options{})
	t.Cleanup(messageBus.Close)
	return messageBus
}

// ---------------------------------------------------------------------------
// The wiring path
// ---------------------------------------------------------------------------

// TestGatewayWiringStartsAndStopsConfiguredChannel is the regression test for
// the defect: a configured channel must be constructed from the configuration,
// registered with the manager, started, and stopped on shutdown — all through
// buildChannelManager, the function cmdGateway calls.
func TestGatewayWiringStartsAndStopsConfiguredChannel(t *testing.T) {
	cfg := loadConfigFile(t, `{
  "channels": {
    "telegram": { "enabled": true, "token": "TESTTOKEN" }
  }
}`)
	messageBus := newTestBus(t)

	manager := buildChannelManager(cfg, messageBus)

	names := manager.EnabledChannels()
	if len(names) != 1 || names[0] != telegram.ChannelName {
		t.Fatalf("EnabledChannels() = %v, want [%s]", names, telegram.ChannelName)
	}
	registered, ok := manager.GetChannel(telegram.ChannelName)
	if !ok {
		t.Fatal("the configured telegram channel was not registered")
	}
	tg, ok := registered.(*telegram.Channel)
	if !ok {
		t.Fatalf("registered channel has type %T, want *telegram.Channel", registered)
	}

	// No credential and no socket: the real channel talks to whatever HTTPDoer
	// its client is given, so the test drives the production runtime in-process.
	api := &fakeBotAPI{}
	tg.SetClient(telegram.NewBotClient("TESTTOKEN", "", telegram.WithHTTPClient(api)))

	// context.Background(), not the gateway's signal context: StopAll must be
	// the thing that stops the channel, exactly as cmdGateway's deferred stop
	// does it.
	startResult := make(chan error, 1)
	go func() { startResult <- manager.StartAll(context.Background()) }()

	waitFor(t, 5*time.Second, "the channel to reach getMe", func() bool { return api.called("getMe") })
	waitFor(t, 5*time.Second, "the channel to enter its getUpdates poll", func() bool {
		return api.called("getUpdates")
	})
	if !tg.IsRunning() {
		t.Fatal("channel does not report running after StartAll")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stopResult := make(chan error, 1)
	go func() { stopResult <- manager.StopAll(stopCtx) }()
	select {
	case err := <-stopResult:
		if err != nil {
			t.Fatalf("StopAll() = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("StopAll did not return: a channel's Stop is blocking shutdown")
	}

	select {
	case err := <-startResult:
		if err != nil {
			t.Fatalf("StartAll() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StartAll did not return after StopAll: the channel's Start is still blocked")
	}

	if tg.IsRunning() {
		t.Error("channel still reports running after StopAll")
	}
	if manager.Started() {
		t.Error("manager still reports started after StopAll")
	}
	if !api.pollReleased() {
		t.Error("the long poll was not cancelled: the channel's transport outlived shutdown")
	}
}

// TestNoChannelConfiguredStartsCleanly pins the behaviour that must not
// regress: with no channel configured the server still starts, and no channel
// is constructed or started.
func TestNoChannelConfiguredStartsCleanly(t *testing.T) {
	// The channels section is present but configures no channel.
	cfg := loadConfigFile(t, `{"channels": {"sendProgress": true}}`)
	messageBus := newTestBus(t)

	manager := buildChannelManager(cfg, messageBus)

	if names := manager.EnabledChannels(); len(names) != 0 {
		t.Fatalf("EnabledChannels() = %v, want none", names)
	}
	if status := manager.GetStatus(); len(status) != 0 {
		t.Fatalf("GetStatus() = %v, want no runtimes", status)
	}

	// StartAll must return instead of holding the gateway open.
	startResult := make(chan error, 1)
	go func() { startResult <- manager.StartAll(context.Background()) }()
	select {
	case err := <-startResult:
		if err != nil {
			t.Fatalf("StartAll() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StartAll blocked with no channels configured: startup would hang")
	}
	if manager.Started() {
		t.Error("manager reports started with no channel registered")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.StopAll(stopCtx); err != nil {
		t.Fatalf("StopAll() = %v, want nil", err)
	}
}

// TestTelegramActivation covers the activation rule: a channel is registered
// only when its section exists and resolves to enabled.
//
// Mirrors _channel_section (manager.py:152-182) and channel_instance_specs
// (contracts.py:337-356). The last two cases pin Python's truthiness
// (`bool(raw_enabled)`, contracts.py:89): a non-empty string enables the
// channel even when it spells "false".
func TestTelegramActivation(t *testing.T) {
	cases := []struct {
		name    string
		section map[string]any
		want    bool
	}{
		{"enabled true", map[string]any{"enabled": true, "token": "T"}, true},
		{"enabled false", map[string]any{"enabled": false, "token": "T"}, false},
		{"enabled omitted", map[string]any{"token": "T"}, false},
		{"enabled null", map[string]any{"enabled": nil, "token": "T"}, false},
		{"enabled 1", map[string]any{"enabled": json.Number("1")}, true},
		{"enabled 0", map[string]any{"enabled": json.Number("0")}, false},
		{"enabled \"yes\"", map[string]any{"enabled": "yes"}, true},
		{"enabled \"false\" (non-empty string is truthy)", map[string]any{"enabled": "false"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Channels.Extra = map[string]any{telegram.ChannelName: tc.section}

			manager := buildChannelManager(cfg, newTestBus(t))
			names := manager.EnabledChannels()
			if tc.want {
				if len(names) != 1 || names[0] != telegram.ChannelName {
					t.Fatalf("EnabledChannels() = %v, want [%s]", names, telegram.ChannelName)
				}
				return
			}
			if len(names) != 0 {
				t.Fatalf("EnabledChannels() = %v, want none", names)
			}
		})
	}

	t.Run("section is not a JSON object", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.Channels.Extra = map[string]any{telegram.ChannelName: "yes"}
		if names := buildChannelManager(cfg, newTestBus(t)).EnabledChannels(); len(names) != 0 {
			t.Fatalf("EnabledChannels() = %v, want none", names)
		}
	})
}

// TestUnbuildableTelegramSectionIsReportedNotSwallowed covers the invalid
// credential case that never reaches a channel: webhook mode without a
// webhookUrl fails TelegramConfig validation (telegram/config.go:742-765).
//
// The reference catches that per channel, records a public error and keeps
// going (manager.py:303-310); the failure must be visible in the log and in
// get_status, never silently dropped.
func TestUnbuildableTelegramSectionIsReportedNotSwallowed(t *testing.T) {
	logs := captureLogs(t)
	cfg := loadConfigFile(t, `{
  "channels": {
    "telegram": { "enabled": true, "token": "TESTTOKEN", "mode": "webhook" }
  }
}`)

	manager := buildChannelManager(cfg, newTestBus(t))

	if names := manager.EnabledChannels(); len(names) != 0 {
		t.Fatalf("EnabledChannels() = %v, want none", names)
	}
	if msg, ok := manager.ChannelError(telegram.ChannelName); !ok || msg != channelLoadError {
		t.Fatalf("ChannelError() = %q (present=%v), want %q", msg, ok, channelLoadError)
	}
	status, ok := manager.GetStatus()[telegram.ChannelName]
	if !ok {
		t.Fatalf("GetStatus() has no entry for %s", telegram.ChannelName)
	}
	if status.State != channels.ChannelStateFailed {
		t.Errorf("status.State = %q, want %q", status.State, channels.ChannelStateFailed)
	}
	if !logs.contains("channel not available") {
		t.Errorf("the construction failure was not logged; records: %v", logs.records)
	}
	if !logs.contains("webhook_url is required") {
		t.Errorf("the logged failure does not carry the underlying cause; records: %v", logs.records)
	}
}

// TestMissingTelegramTokenIsReported covers the missing-credential case: the
// channel is constructed (as the reference constructs it) and its Start fails.
// The manager records the failure and logs the real error, and the gateway is
// not taken down by it.
func TestMissingTelegramTokenIsReported(t *testing.T) {
	logs := captureLogs(t)
	cfg := loadConfigFile(t, `{"channels": {"telegram": {"enabled": true}}}`)

	manager := buildChannelManager(cfg, newTestBus(t))
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = manager.StopAll(stopCtx)
	}()

	if names := manager.EnabledChannels(); len(names) != 1 {
		t.Fatalf("EnabledChannels() = %v, want the configured telegram channel", names)
	}

	// A channel that cannot start must not end the gateway: StartAll returns
	// nil and the failure lives on the manager (manager.py:373-389).
	if err := manager.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll() = %v, want nil", err)
	}
	if msg, ok := manager.ChannelError(telegram.ChannelName); !ok || msg == "" {
		t.Fatal("no channel error was recorded for the failed start")
	}
	if !logs.contains("bot token not configured") {
		t.Errorf("the real start failure was not logged; records: %v", logs.records)
	}
}
