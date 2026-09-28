package wecom

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

const ChannelName = "wecom"

type Config struct {
	Enabled bool
	BotID string
	Secret string
	AllowFrom []string
	WelcomeMessage string
	WebSocketURL string
	HeartbeatInterval time.Duration
}

func sectionConfig(section channels.Section) (Config, error) {
	values, _ := section.Map()
	cfg := Config{
		Enabled: boolValue(values["enabled"], false),
		BotID: strings.TrimSpace(stringValue(values["botId"], "")),
		Secret: stringValue(values["secret"], ""),
		AllowFrom: stringList(values["allowFrom"]),
		WelcomeMessage: stringValue(values["welcomeMessage"], ""),
		WebSocketURL: strings.TrimSpace(stringValue(values["websocketUrl"], "wss://openws.work.weixin.qq.com")),
		HeartbeatInterval: time.Duration(intValue(values["heartbeatIntervalMs"], 30000)) * time.Millisecond,
	}
	u, err := url.Parse(cfg.WebSocketURL)
	if err != nil || u.Scheme != "wss" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return Config{}, fmt.Errorf("wecom: websocketUrl must be a WSS URL")
	}
	if cfg.HeartbeatInterval < 5*time.Second || cfg.HeartbeatInterval > 5*time.Minute {
		return Config{}, fmt.Errorf("wecom: heartbeatIntervalMs must be between 5000 and 300000")
	}
	return cfg, nil
}

func (c Config) validateRuntime() error {
	var missing []string
	if c.BotID == "" { missing = append(missing, "botId") }
	if c.Secret == "" { missing = append(missing, "secret") }
	if len(missing) != 0 {
		return fmt.Errorf("wecom: missing required setting(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

func stringValue(v any, fallback string) string { if s, ok := v.(string); ok { return s }; return fallback }
func boolValue(v any, fallback bool) bool { if b, ok := v.(bool); ok { return b }; return fallback }
func intValue(v any, fallback int) int {
	switch n := v.(type) { case int: return n; case int64: return int(n); case float64: return int(n) }
	return fallback
}
func stringList(v any) []string {
	switch x := v.(type) {
	case []string:
		return append([]string(nil), x...)
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" { out = append(out, strings.TrimSpace(s)) }
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
