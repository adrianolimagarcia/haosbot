package dingtalk

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

const ChannelName = "dingtalk"

type Config struct {
	Enabled            bool
	ClientID           string
	ClientSecret       string
	AllowFrom          []string
	OpenAPIHost        string
	GroupUserIsolation bool
	DisablePrivateChat bool
}

func sectionConfig(section channels.Section) (Config, error) {
	values, _ := section.Map()
	cfg := Config{
		Enabled: boolValue(values["enabled"], false),
		ClientID: strings.TrimSpace(stringValue(values["clientId"], "")),
		ClientSecret: stringValue(values["clientSecret"], ""),
		AllowFrom: stringList(values["allowFrom"]),
		OpenAPIHost: strings.TrimRight(strings.TrimSpace(stringValue(values["openApiHost"], "https://api.dingtalk.com")), "/"),
		GroupUserIsolation: boolValue(values["groupUserIsolation"], false),
		DisablePrivateChat: boolValue(values["disablePrivateChat"], false),
	}
	u, err := url.Parse(cfg.OpenAPIHost)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Config{}, fmt.Errorf("dingtalk: openApiHost must be an HTTPS origin")
	}
	return cfg, nil
}

func (c Config) validateRuntime() error {
	var missing []string
	if c.ClientID == "" { missing = append(missing, "clientId") }
	if c.ClientSecret == "" { missing = append(missing, "clientSecret") }
	if len(missing) != 0 {
		return fmt.Errorf("dingtalk: missing required setting(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

func stringValue(v any, fallback string) string { if s, ok := v.(string); ok { return s }; return fallback }
func boolValue(v any, fallback bool) bool { if b, ok := v.(bool); ok { return b }; return fallback }
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
