package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

type capturePublisher struct {
	inbound chan core.InboundMessage
}

func (publisher *capturePublisher) PublishInbound(_ context.Context, message core.InboundMessage) error {
	publisher.inbound <- message
	return nil
}

func TestParseConfigRequiresBothTokens(t *testing.T) {
	if _, err := parseConfig(map[string]any{"enabled": true, "botToken": "xoxb"}); err == nil {
		t.Fatal("enabled Slack channel accepted missing app token")
	}
	if _, err := parseConfig(map[string]any{"enabled": true, "botToken": "xoxb", "appToken": "xapp"}); err != nil {
		t.Fatal(err)
	}
}

func TestSocketModeInboundAndThreadReply(t *testing.T) {
	publisher := &capturePublisher{inbound: make(chan core.InboundMessage, 1)}
	acked := make(chan string, 1)
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/socket", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept Socket Mode connection: %v", err)
			return
		}
		defer conn.CloseNow()
		_ = conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"hello"}`))
		_ = conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"events_api","envelope_id":"env-1","payload":{"event":{"type":"message","user":"U_ALICE","channel":"C_ROOM","text":"hello","ts":"1710000000.000001"}}}`))
		_, data, err := conn.Read(r.Context())
		if err == nil {
			var ack map[string]string
			_ = json.Unmarshal(data, &ack)
			acked <- ack["envelope_id"]
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("/api/auth.test", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xoxb-test" {
			http.Error(w, "bad bot token", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "user_id": "U_BOT"})
	})
	mux.HandleFunc("/api/apps.connections.open", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xapp-test" {
			http.Error(w, "bad app token", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "ws://" + strings.TrimPrefix(server.URL, "http://") + "/socket"})
	})
	mux.HandleFunc("/api/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xoxb-test" {
			http.Error(w, "bad bot token", http.StatusUnauthorized)
			return
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode chat.postMessage: %v", err)
		}
		if payload["channel"] != "C_ROOM" || payload["thread_ts"] != "1710000000.000001" || payload["text"] != "reply" {
			t.Errorf("unexpected postMessage payload: %#v", payload)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1710000000.000002"})
	})
	server = httptest.NewServer(mux)
	defer server.Close()

	section := channels.NewMapSection(map[string]any{
		"enabled": true, "botToken": "xoxb-test", "appToken": "xapp-test",
		"allowFrom": []any{"U_ALICE"}, "allowedRooms": []any{"C_ROOM"},
	})
	channel, err := New(section, publisher)
	if err != nil {
		t.Fatal(err)
	}
	channel.apiBase = server.URL + "/api/"
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan error, 1)
	go func() { started <- channel.Start(ctx) }()

	var inbound core.InboundMessage
	select {
	case inbound = <-publisher.inbound:
		if inbound.Channel != ChannelName || inbound.SenderID != "U_ALICE" || inbound.ChatID != "C_ROOM|1710000000.000001" || inbound.Content != "hello" {
			t.Fatalf("unexpected inbound Slack message: %#v", inbound)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("Slack event was not published")
	}
	select {
	case id := <-acked:
		if id != "env-1" {
			t.Fatalf("ack id = %q", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Slack event was not acknowledged")
	}
	if err := channel.Send(context.Background(), core.OutboundMessage{ChatID: inbound.ChatID, Content: "reply"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after cancellation")
	}
}

func TestAllowedRoomAndChatIDHelpers(t *testing.T) {
	channel, err := New(channels.NewMapSection(map[string]any{
		"botToken": "xoxb-test", "appToken": "xapp-test", "allowedRooms": []any{"C1"},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !channel.allowedRoom("C1") || channel.allowedRoom("C2") {
		t.Fatal("room allowlist was not enforced")
	}
	room, thread := splitChatID(joinChatID("C1", "123.456"))
	if room != "C1" || thread != "123.456" {
		t.Fatalf("round trip room/thread = %q/%q", room, thread)
	}
}
