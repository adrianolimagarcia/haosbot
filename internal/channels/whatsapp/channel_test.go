package whatsapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

type capturePublisher struct{ inbound chan core.InboundMessage }

func (p *capturePublisher) PublishInbound(_ context.Context, message core.InboundMessage) error {
	p.inbound <- message
	return nil
}

func TestWhatsAppWebhookVerificationAndInbound(t *testing.T) {
	publisher := &capturePublisher{inbound: make(chan core.InboundMessage, 1)}
	channel, err := New(channels.NewMapSection(map[string]any{
		"enabled": true, "phoneNumberId": "phone-id", "accessToken": "access",
		"appSecret": "app-secret", "verifyToken": "verify-me", "graphVersion": "v99.0",
		"allowFrom": []any{"15557654321"},
	}), publisher)
	if err != nil {
		t.Fatal(err)
	}

	verify := httptest.NewRequest(http.MethodGet, "/webhooks/whatsapp?hub.mode=subscribe&hub.verify_token=verify-me&hub.challenge=challenge", nil)
	verifyResult := httptest.NewRecorder()
	channel.handler().ServeHTTP(verifyResult, verify)
	if verifyResult.Code != http.StatusOK || verifyResult.Body.String() != "challenge" {
		t.Fatalf("webhook verification = %d %q", verifyResult.Code, verifyResult.Body.String())
	}

	body := `{"object":"whatsapp_business_account","entry":[{"changes":[{"value":{"metadata":{"phone_number_id":"phone-id"},"messages":[{"from":"15557654321","id":"wamid-1","type":"text","text":{"body":"hello"}},{"from":"15550000000","id":"wamid-2","type":"text","text":{"body":"blocked"}}]}}]}]}`
	request := signedRequest(t, http.MethodPost, "/webhooks/whatsapp", body, "app-secret")
	postResult := httptest.NewRecorder()
	channel.handler().ServeHTTP(postResult, request)
	if postResult.Code != http.StatusOK {
		t.Fatalf("webhook POST = %d %q", postResult.Code, postResult.Body.String())
	}
	select {
	case inbound := <-publisher.inbound:
		if inbound.Channel != ChannelName || inbound.SenderID != "15557654321" || inbound.ChatID != "15557654321" || inbound.Content != "hello" {
			t.Fatalf("unexpected WhatsApp inbound: %#v", inbound)
		}
	default:
		t.Fatal("valid WhatsApp message was not dispatched")
	}
	if channel.validSignature([]byte(body), "sha256=00") {
		t.Fatal("invalid webhook signature was accepted")
	}
}

func TestWhatsAppSendUsesCloudAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v99.0/phone-id/messages" || r.Header.Get("Authorization") != "Bearer access" {
			t.Errorf("unexpected Graph request path=%q auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var payload struct {
			Product string `json:"messaging_product"`
			To      string `json:"to"`
			Text    struct {
				Body string `json:"body"`
			} `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode Graph request: %v", err)
		}
		if payload.Product != "whatsapp" || payload.To != "15557654321" || payload.Text.Body != "reply" {
			t.Errorf("unexpected Graph payload: %#v", payload)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	channel, err := New(channels.NewMapSection(map[string]any{
		"phoneNumberId": "phone-id", "accessToken": "access", "appSecret": "secret",
		"verifyToken": "verify", "graphVersion": "v99.0", "graphBase": server.URL,
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := channel.Send(context.Background(), core.OutboundMessage{ChatID: "15557654321", Content: "reply"}); err != nil {
		t.Fatal(err)
	}
}

func TestWhatsAppWebhookRejectsBadSignature(t *testing.T) {
	channel, err := New(channels.NewMapSection(map[string]any{
		"phoneNumberId": "phone-id", "accessToken": "access", "appSecret": "secret",
		"verifyToken": "verify", "graphVersion": "v99.0",
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/webhooks/whatsapp", strings.NewReader(`{}`))
	request.Header.Set("X-Hub-Signature-256", "sha256=bad")
	result := httptest.NewRecorder()
	channel.handler().ServeHTTP(result, request)
	if result.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature status = %d, want 401", result.Code)
	}
}

func signedRequest(t *testing.T, method, path, body, secret string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(body))
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return request
}
