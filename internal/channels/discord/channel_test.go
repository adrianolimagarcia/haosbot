package discord

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

func TestParseConfigRequiresBotTokenWhenEnabled(t *testing.T) {
	if _, err := parseConfig(map[string]any{"enabled": true}); err == nil {
		t.Fatal("enabled Discord channel accepted an empty bot token")
	}
	if _, err := parseConfig(map[string]any{"enabled": false}); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayInboundAndReply(t *testing.T) {
	publisher := &capturePublisher{inbound: make(chan core.InboundMessage, 1)}
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/gateway", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "10" || r.URL.Query().Get("encoding") != "json" {
			t.Errorf("gateway query = %q", r.URL.RawQuery)
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept Gateway socket: %v", err)
			return
		}
		defer conn.CloseNow()
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(`{"op":10,"d":{"heartbeat_interval":60000}}`)); err != nil {
			return
		}
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var identify struct {
			Op int `json:"op"`
			Data struct {
				Token   string `json:"token"`
				Intents int    `json:"intents"`
			} `json:"d"`
		}
		if err := json.Unmarshal(data, &identify); err != nil {
			t.Errorf("decode identify: %v", err)
			return
		}
		if identify.Op != 2 || identify.Data.Token != "bot-secret" || identify.Data.Intents != defaultIntents {
			t.Errorf("unexpected identify: %#v", identify)
			return
		}
		message := `{"op":0,"s":1,"t":"MESSAGE_CREATE","d":{"id":"M1","channel_id":"C1","guild_id":"G1","content":"hello","author":{"id":"U1","bot":false}}}`
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(message)); err != nil {
			return
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("/api/v10/users/@me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot bot-secret" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "BOT1", "bot": true})
	})
	mux.HandleFunc("/api/v10/gateway/bot", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "ws://" + strings.TrimPrefix(server.URL, "http://") + "/gateway"})
	})
	mux.HandleFunc("/api/v10/channels/C1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot bot-secret" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		var payload struct {
			Content          string `json:"content"`
			MessageReference struct {
				MessageID string `json:"message_id"`
			} `json:"message_reference"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode outbound message: %v", err)
		}
		if payload.Content != "reply" || payload.MessageReference.MessageID != "M1" {
			t.Errorf("unexpected outbound payload: %#v", payload)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"M2"}`))
	})
	server = httptest.NewServer(mux)
	defer server.Close()

	section := channels.NewMapSection(map[string]any{
		"enabled": true, "botToken": "bot-secret",
		"allowFrom": []any{"U1"}, "allowedChannels": []any{"C1"},
	})
	channel, err := New(section, publisher)
	if err != nil {
		t.Fatal(err)
	}
	channel.apiBase = server.URL + "/api/v10"
	channel.gateway = server.URL + "/api/v10/gateway/bot"
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan error, 1)
	go func() { started <- channel.Start(ctx) }()

	var inbound core.InboundMessage
	select {
	case inbound = <-publisher.inbound:
		if inbound.Channel != ChannelName || inbound.SenderID != "U1" || inbound.ChatID != "C1" || inbound.Content != "hello" {
			t.Fatalf("unexpected inbound Discord message: %#v", inbound)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("Discord message was not published")
	}
	replyTo := "M1"
	if err := channel.Send(context.Background(), core.OutboundMessage{ChatID: inbound.ChatID, Content: "reply", ReplyTo: &replyTo}); err != nil {
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

func TestAllowedChannelFilter(t *testing.T) {
	channel, err := New(channels.NewMapSection(map[string]any{
		"botToken": "bot-secret", "allowedChannels": []any{"C1"},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !channel.allowedChannel("C1") || channel.allowedChannel("C2") {
		t.Fatal("Discord channel filter did not enforce the configured list")
	}
}
