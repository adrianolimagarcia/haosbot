package dingtalk

import (
	"testing"

	"github.com/adrianolimagaracia/nanobot-go/internal/channels"
)

func TestConfigDefaults(t *testing.T) {
	cfg, err := sectionConfig(channels.NewMapSection(map[string]any{}))
	if err != nil { t.Fatal(err) }
	if cfg.OpenAPIHost != "https://api.dingtalk.com" { t.Fatalf("host=%q", cfg.OpenAPIHost) }
}

func TestTrustedSessionWebhook(t *testing.T) {
	for _, raw := range []string{
		"https://oapi.dingtalk.com/robot/sendBySession?session=1",
		"https://api.dingtalk.com/x",
	} {
		if !trustedSessionWebhook(raw) { t.Fatalf("expected trusted: %s", raw) }
	}
	for _, raw := range []string{
		"http://oapi.dingtalk.com/x",
		"https://dingtalk.com.evil.example/x",
		"https://evil.example/x",
	} {
		if trustedSessionWebhook(raw) { t.Fatalf("expected untrusted: %s", raw) }
	}
}
