package websocket

import (
	"context"
	"encoding/json"
	"net/url"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

type capturePublisher struct { inbound chan core.InboundMessage }

func (p *capturePublisher) PublishInbound(_ context.Context, msg core.InboundMessage) error {
	p.inbound <- msg
	return nil
}

func TestParseConfigRequiresTokenOnlyWhenEnabled(t *testing.T) {
	if _, err := parseConfig(map[string]any{"enabled": true}); err == nil { t.Fatal("enabled channel accepted an empty token") }
	cfg, err := parseConfig(map[string]any{"enabled": false})
	if err != nil { t.Fatal(err) }
	if cfg.Host != defaultHost || cfg.Port != defaultPort || cfg.Path != defaultPath { t.Fatalf("unexpected defaults: %#v", cfg) }
}

func TestWebSocketRoundTripAndShutdown(t *testing.T) {
	publisher := &capturePublisher{inbound: make(chan core.InboundMessage, 1)}
	section := channels.NewMapSection(map[string]any{
		"enabled": true, "host": "127.0.0.1", "port": 0, "path": "/ws",
		"token": "test-secret", "allowFrom": []any{"test-client"}, "streaming": true,
	})
	channel, err := New(section, publisher)
	if err != nil { t.Fatal(err) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	go func() { started <- channel.Start(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	var address string
	for time.Now().Before(deadline) {
		channel.mu.Lock()
		if channel.listener != nil { address = channel.listener.Addr().String() }
		channel.mu.Unlock()
		if address != "" { break }
		time.Sleep(10 * time.Millisecond)
	}
	if address == "" { t.Fatal("listener did not start") }
	parsed := url.URL{Scheme: "ws", Host: address, Path: "/ws"}
	query := parsed.Query()
	query.Set("client_id", "test-client")
	query.Set("token", "test-secret")
	parsed.RawQuery = query.Encode()
	client, _, err := websocket.Dial(context.Background(), parsed.String(), nil)
	if err != nil { t.Fatal(err) }
	defer client.Close(websocket.StatusNormalClosure, "test done")

	read := func() map[string]any {
		readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer readCancel()
		_, data, readErr := client.Read(readCtx)
		if readErr != nil { t.Fatal(readErr) }
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil { t.Fatal(err) }
		return payload
	}
	ready := read()
	if ready["event"] != "ready" || ready["client_id"] != "test-client" { t.Fatalf("unexpected ready frame: %#v", ready) }
	chatID, ok := ready["chat_id"].(string)
	if !ok || chatID == "" { t.Fatalf("missing chat_id: %#v", ready) }
	if err := client.Write(context.Background(), websocket.MessageText, []byte(`{"text":"hello"}`)); err != nil { t.Fatal(err) }
	select {
	case inbound := <-publisher.inbound:
		if inbound.Channel != ChannelName || inbound.Content != "hello" || inbound.ChatID != chatID || inbound.SenderID != "test-client" {
			t.Fatalf("unexpected inbound message: %#v", inbound)
		}
	case <-time.After(3 * time.Second): t.Fatal("inbound message was not published")
	}
	if err := channel.Send(context.Background(), core.OutboundMessage{Channel: ChannelName, ChatID: chatID, Content: "reply"}); err != nil { t.Fatal(err) }
	reply := read()
	if reply["event"] != "message" || reply["text"] != "reply" { t.Fatalf("unexpected reply frame: %#v", reply) }

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	if err := channel.Stop(stopCtx); err != nil { t.Fatal(err) }
	select {
	case err := <-started:
		if err != nil { t.Fatalf("Start returned %v", err) }
	case <-time.After(3 * time.Second): t.Fatal("Start did not return after Stop")
	}
}

func TestInvalidClientIDRejectedBeforeUpgrade(t *testing.T) {
	c, err := New(channels.NewMapSection(map[string]any{"token": "secret", "port": 0}), nil)
	if err != nil { t.Fatal(err) }
	r := httptest.NewRequest("GET", "/?token=secret&client_id="+strings.Repeat("x", 129), nil)
	w := httptest.NewRecorder()
	c.serveHTTP(w, r)
	if w.Code != 400 { t.Fatalf("status = %d, want 400", w.Code) }
}

func TestWebSocketRejectsBadToken(t *testing.T) {
	c, err := New(channels.NewMapSection(map[string]any{"token": "expected-secret", "port": 0}), nil)
	if err != nil { t.Fatal(err) }
	r := httptest.NewRequest("GET", "/?token=wrong", nil)
	w := httptest.NewRecorder()
	c.serveHTTP(w, r)
	if w.Code != 401 { t.Fatalf("status = %d, want 401", w.Code) }
}
