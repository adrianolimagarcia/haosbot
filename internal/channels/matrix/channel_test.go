package matrix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestParseConfigRequiresCredentialsWhenEnabled(t *testing.T) {
	if _, err := parseConfig(map[string]any{"enabled": true}); err == nil {
		t.Fatal("enabled Matrix channel accepted missing credentials")
	}
	if _, err := parseConfig(map[string]any{"enabled": false}); err != nil {
		t.Fatal(err)
	}
	if _, err := parseConfig(map[string]any{"homeserver": "https://user:pass@example.org"}); err == nil {
		t.Fatal("homeserver userinfo was accepted")
	}
}

func TestSyncAndSend(t *testing.T) {
	publisher := &capturePublisher{inbound: make(chan core.InboundMessage, 1)}
	var syncs int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/_matrix/client/v3/account/whoami":
			_ = json.NewEncoder(w).Encode(map[string]string{"user_id": "@haosbot:example.org"})
		case r.URL.Path == "/_matrix/client/v3/sync":
			syncs++
			if syncs == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"next_batch": "s1",
					"rooms": map[string]any{"join": map[string]any{
						"!room:example.org": map[string]any{"timeline": map[string]any{"events": []any{
							map[string]any{"event_id": "$event1", "sender": "@alice:example.org", "type": "m.room.message", "content": map[string]any{"msgtype": "m.text", "body": "hello"}},
						}}},
					}},
				})
				return
			}
			<-r.Context().Done()
		case strings.Contains(r.URL.Path, "/send/m.room.message/"):
			if r.Method != http.MethodPut {
				http.Error(w, "bad method", http.StatusMethodNotAllowed)
				return
			}
			var message map[string]string
			if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
				t.Errorf("decode send body: %v", err)
			}
			if message["body"] != "reply" || message["msgtype"] != "m.text" {
				t.Errorf("unexpected message body: %#v", message)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"event_id": "$reply"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	section := channels.NewMapSection(map[string]any{
		"enabled": true, "homeserver": server.URL, "accessToken": "test-token",
		"userId": "@haosbot:example.org", "rooms": []any{"!room:example.org"},
		"allowFrom": []any{"@alice:example.org"}, "syncTimeoutMs": 1000,
	})
	channel, err := New(section, publisher)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan error, 1)
	go func() { started <- channel.Start(ctx) }()

	select {
	case inbound := <-publisher.inbound:
		if inbound.Channel != ChannelName || inbound.Content != "hello" || inbound.ChatID != "!room:example.org" || inbound.SenderID != "@alice:example.org" {
			t.Fatalf("unexpected inbound message: %#v", inbound)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("Matrix event was not published")
	}
	if err := channel.Send(context.Background(), core.OutboundMessage{ChatID: "!room:example.org", Content: "reply"}); err != nil {
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
