package signal

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

func TestSignalReceiveAndSend(t *testing.T) {
	publisher := &capturePublisher{inbound: make(chan core.InboundMessage, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/receive/+15551234567", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer proxy-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept Signal socket: %v", err)
			return
		}
		defer conn.CloseNow()
		event := `[{"envelope":{"source":"+15557654321","sourceNumber":"+15557654321","dataMessage":{"message":"hello"}},"account":"+15551234567"}]`
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(event)); err != nil {
			return
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("/v2/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer proxy-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var payload struct {
			Number     string   `json:"number"`
			Recipients []string `json:"recipients"`
			Message    string   `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode Signal send body: %v", err)
		}
		if payload.Number != "+15551234567" || len(payload.Recipients) != 1 || payload.Recipients[0] != "+15557654321" || payload.Message != "reply" {
			t.Errorf("unexpected send payload: %#v", payload)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	channel, err := New(channels.NewMapSection(map[string]any{
		"enabled": true, "apiBase": server.URL, "number": "+15551234567", "apiToken": "proxy-secret",
		"allowFrom": []any{"+15557654321"},
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
		if inbound.Channel != ChannelName || inbound.SenderID != "+15557654321" || inbound.ChatID != "+15557654321" || inbound.Content != "hello" {
			t.Fatalf("unexpected Signal inbound message: %#v", inbound)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("Signal event was not published")
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

func TestSignalGroupEventAndSenderFilter(t *testing.T) {
	channel, err := New(channels.NewMapSection(map[string]any{
		"apiBase": "http://127.0.0.1:8080", "number": "+15550000000",
		"allowFrom": []any{"allowed"},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !channel.allowedSender("allowed") || channel.allowedSender("blocked") {
		t.Fatal("Signal sender allowlist was not enforced")
	}
	var event receiveEvent
	if err := json.Unmarshal([]byte(`{"envelope":{"source":"allowed","dataMessage":{"message":"hello","groupInfo":{"groupId":"group-id"}}}}`), &event); err != nil {
		t.Fatal(err)
	}
	if group := event.Envelope.DataMessage.GroupInfo; group == nil || group.GroupID != "group-id" {
		t.Fatalf("Signal group ID was not decoded: %#v", event)
	}
}
