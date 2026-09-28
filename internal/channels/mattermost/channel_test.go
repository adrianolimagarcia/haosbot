package mattermost

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

type capturePublisher struct{ inbound chan core.InboundMessage }

func (p *capturePublisher) PublishInbound(_ context.Context, message core.InboundMessage) error {
	p.inbound <- message
	return nil
}

func TestMattermostInboundAndOutbound(t *testing.T) {
	publisher := &capturePublisher{inbound: make(chan core.InboundMessage, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/users/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "bot-id"})
	})
	mux.HandleFunc("/api/v4/posts", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var payload struct {
			ChannelID string `json:"channel_id"`
			Message   string `json:"message"`
			RootID    string `json:"root_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode post body: %v", err)
		}
		if payload.ChannelID != "channel-id" || payload.Message != "reply" || payload.RootID != "root-id" {
			t.Errorf("unexpected post payload: %#v", payload)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"sent"}`))
	})
	mux.HandleFunc("/api/v4/websocket", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept Mattermost socket: %v", err)
			return
		}
		defer conn.CloseNow()
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var challenge struct {
			Seq    int    `json:"seq"`
			Action string `json:"action"`
			Data   struct {
				Token string `json:"token"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &challenge); err != nil || challenge.Seq != 1 || challenge.Action != "authentication_challenge" || challenge.Data.Token != "secret" {
			t.Errorf("unexpected authentication challenge: %s", data)
			return
		}
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(`{"status":"OK","seq_reply":1}`)); err != nil {
			return
		}
		event := `{"event":"posted","data":{"post":"{\"id\":\"post-id\",\"user_id\":\"user-id\",\"channel_id\":\"channel-id\",\"message\":\"hello\",\"root_id\":\"root-id\"}"}}`
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(event)); err != nil {
			return
		}
		<-r.Context().Done()
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	channel, err := New(channels.NewMapSection(map[string]any{
		"enabled": true, "serverUrl": server.URL, "botToken": "secret",
		"allowFrom": []any{"user-id"}, "allowedChannels": []any{"channel-id"},
	}), publisher)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan error, 1)
	go func() { started <- channel.Start(ctx) }()

	var inbound core.InboundMessage
	select {
	case inbound = <-publisher.inbound:
		if inbound.Channel != ChannelName || inbound.SenderID != "user-id" || inbound.ChatID != "channel-id|root-id" || inbound.Content != "hello" {
			t.Fatalf("unexpected inbound message: %#v", inbound)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("Mattermost event was not published")
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

func TestMattermostSenderAndChannelFilters(t *testing.T) {
	channel, err := New(channels.NewMapSection(map[string]any{
		"serverUrl": "https://mattermost.example", "botToken": "secret",
		"allowFrom": []any{"allowed-user"}, "allowedChannels": []any{"allowed-channel"},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !channel.allowedSender("allowed-user") || channel.allowedSender("other-user") {
		t.Fatal("sender allowlist was not enforced")
	}
	if !channel.allowedChannel("allowed-channel") || channel.allowedChannel("other-channel") {
		t.Fatal("channel allowlist was not enforced")
	}
}
