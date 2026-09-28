package linear

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

const ChannelName = "linear"

type Config struct {
	Enabled              bool
	APIKey               string
	WebhookSigningSecret string
	ListenAddr           string
	WebhookPath          string
	AllowFrom            []string
	APIBase              string
}

func sectionConfig(section channels.Section) (Config, error) {
	values, _ := section.Map()
	cfg := Config{
		Enabled: boolValue(values["enabled"], false),
		APIKey: strings.TrimSpace(stringValue(values["apiKey"], "")),
		WebhookSigningSecret: stringValue(values["webhookSigningSecret"], ""),
		ListenAddr: strings.TrimSpace(stringValue(values["listenAddr"], "0.0.0.0:3979")),
		WebhookPath: strings.TrimSpace(stringValue(values["webhookPath"], "/linear/webhook")),
		AllowFrom: stringList(values["allowFrom"]),
		APIBase: strings.TrimRight(strings.TrimSpace(stringValue(values["apiBase"], "https://api.linear.app/graphql")), "/"),
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "0.0.0.0:3979"
	}
	if cfg.WebhookPath == "" || !strings.HasPrefix(cfg.WebhookPath, "/") || strings.ContainsAny(cfg.WebhookPath, "?#") {
		return Config{}, fmt.Errorf("linear: webhookPath must be an absolute path without query or fragment")
	}
	parsed, err := url.Parse(cfg.APIBase)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return Config{}, fmt.Errorf("linear: apiBase must be an HTTPS URL without credentials, query or fragment")
	}
	return cfg, nil
}

func (c Config) validateRuntime() error {
	var missing []string
	if c.APIKey == "" { missing = append(missing, "apiKey") }
	if c.WebhookSigningSecret == "" { missing = append(missing, "webhookSigningSecret") }
	if len(missing) != 0 {
		return fmt.Errorf("linear: missing required setting(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

func stringValue(v any, fallback string) string {
	if s, ok := v.(string); ok { return s }
	return fallback
}
func boolValue(v any, fallback bool) bool {
	if b, ok := v.(bool); ok { return b }
	return fallback
}
func stringList(v any) []string {
	switch x := v.(type) {
	case []string:
		return append([]string(nil), x...)
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case string:
		var out []string
		for _, item := range strings.Split(x, ",") {
			if s := strings.TrimSpace(item); s != "" { out = append(out, s) }
		}
		return out
	default:
		return nil
	}
}
