package linear

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

func TestConfigDefaults(t *testing.T) {
	cfg, err := sectionConfig(channels.NewMapSection(map[string]any{}))
	if err != nil { t.Fatal(err) }
	if cfg.ListenAddr != "0.0.0.0:3979" || cfg.WebhookPath != "/linear/webhook" || cfg.APIBase != "https://api.linear.app/graphql" {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
}

func TestWebhookSignature(t *testing.T) {
	c, err := New(channels.NewMapSection(map[string]any{"webhookSigningSecret":"secret"}), nil)
	if err != nil { t.Fatal(err) }
	body := []byte(`{"type":"Comment"}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))
	if !c.validSignature(body, sig) { t.Fatal("valid signature rejected") }
	if c.validSignature(body, sig+"00") { t.Fatal("invalid signature accepted") }
}
