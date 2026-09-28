package napcat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestParseConfigRequiresSecureEndpointsWhenEnabled(t *testing.T) {
	if _, err := parseConfig(map[string]any{"enabled": true}); err == nil {
		t.Fatal("enabled Napcat channel accepted missing endpoints")
	}
	if _, err := parseConfig(map[string]any{
		"enabled": true, "websocketUrl": "https://example.org/ws",
		"apiBase": "http://example.org", "accessToken": "secret",
	}); err == nil {
		t.Fatal("HTTPS URL was accepted as a WebSocket endpoint")
	}
}

func TestOneBotInboundAndOutbound(t *testing.T) {
	publisher := &capturePublisher{inbound: make(chan core.InboundMessage, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept OneBot socket: %v", err)
			return
		}
		defer conn.CloseNow()
		event := `{"post_type":"message","message_type":"private","self_id":123,"user_id":456,"message_id":7,"raw_message":"hello","message":[{"type":"text","data":{"text":"hello"}}]}`
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(event)); err != nil {
			return
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("/api/send_msg", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		var payload struct {
			MessageType string `json:"message_type"`
			UserID      string `json:"user_id"`
			GroupID     string `json:"group_id"`
			Message     string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode send_msg body: %v", err)
		}
		if payload.MessageType != "private" || payload.UserID != "456" || payload.GroupID != "" || payload.Message != "reply" {
			t.Errorf("unexpected send_msg request: %#v", payload)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "retcode": 0, "data": map[string]any{"message_id": 8}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	section := channels.NewMapSection(map[string]any{
		"enabled": true,
		"websocketUrl": "ws" + server.URL[len("http"): ] + "/ws",
		"apiBase":     server.URL + "/api",
		"accessToken": "secret",
		"selfId":      "123",
		"allowFrom":   []any{"456"},
	})
	channel, err := New(section, publisher)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan error, 1)
	go func() { started <- channel.Start(ctx) }()

	var inbound core.InboundMessage
	select {
	case inbound = <-publisher.inbound:
		if inbound.Channel != ChannelName || inbound.SenderID != "456" || inbound.ChatID != "private:456" || inbound.Content != "hello" {
			t.Fatalf("unexpected OneBot inbound message: %#v", inbound)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("OneBot event was not published")
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

func TestGroupFilterAndChatID(t *testing.T) {
	channel, err := New(channels.NewMapSection(map[string]any{
		"websocketUrl": "ws://127.0.0.1:3001/ws",
		"apiBase":      "http://127.0.0.1:3000",
		"accessToken":  "secret",
		"allowedGroups": []any{"G1"},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !channel.allowedGroup("G1") || channel.allowedGroup("G2") {
		t.Fatal("group filter did not enforce the configured list")
	}
	kind, id, ok := splitChatID("group:G1")
	if !ok || kind != "group" || id != "G1" {
		t.Fatalf("splitChatID(group:G1) = %q, %q, %v", kind, id, ok)
	}
}

func TestQQAliasUsesOneBotRuntimeIdentity(t *testing.T) {
	channel, err := NewQQ(channels.NewMapSection(map[string]any{
		"websocketUrl": "ws://127.0.0.1:3001/ws",
		"apiBase":      "http://127.0.0.1:3000",
		"accessToken":  "secret",
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if channel.Name() != QQChannelName || channel.DisplayName() != "QQ via OneBot 11" {
		t.Fatalf("QQ OneBot identity = %q / %q", channel.Name(), channel.DisplayName())
	}
}
