package weixin

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

const ChannelName = "weixin"

type Config struct {
	Enabled bool
	Token string
	AllowFrom []string
	BaseURL string
	RouteTag string
	PollTimeout time.Duration
}

func sectionConfig(section channels.Section) (Config, error) {
	values, _ := section.Map()
	cfg := Config{
		Enabled: boolValue(values["enabled"], false),
		Token: strings.TrimSpace(stringValue(values["token"], "")),
		AllowFrom: stringList(values["allowFrom"]),
		BaseURL: strings.TrimRight(strings.TrimSpace(stringValue(values["baseUrl"], "https://ilinkai.weixin.qq.com")), "/"),
		RouteTag: strings.TrimSpace(stringValue(values["routeTag"], "")),
		PollTimeout: time.Duration(intValue(values["pollTimeout"], 35))*time.Second,
	}
	if cfg.PollTimeout < 5*time.Second || cfg.PollTimeout > 2*time.Minute {
		return Config{}, fmt.Errorf("weixin: pollTimeout must be between 5 and 120 seconds")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Config{}, fmt.Errorf("weixin: baseUrl must be an HTTPS URL without credentials, query or fragment")
	}
	return cfg, nil
}

func (c Config) validateRuntime() error {
	if c.Token == "" { return fmt.Errorf("weixin: token is required; obtain one through WeChat QR login") }
	return nil
}

func stringValue(v any, fallback string) string { if s, ok := v.(string); ok { return s }; return fallback }
func boolValue(v any, fallback bool) bool { if b, ok := v.(bool); ok { return b }; return fallback }
func intValue(v any, fallback int) int {
	switch n:=v.(type) { case int: return n; case int64: return int(n); case float64: return int(n) }
	return fallback
}
func stringList(v any) []string {
	switch x:=v.(type) {
	case []string: return append([]string(nil), x...)
	case []any:
		out:=make([]string,0,len(x)); for _,item:=range x { if s,ok:=item.(string); ok && strings.TrimSpace(s)!="" { out=append(out,strings.TrimSpace(s)) } }; return out
	case string:
		var out []string; for _,item:=range strings.Split(x,",") { if s:=strings.TrimSpace(item); s!="" { out=append(out,s) } }; return out
	default: return nil
	}
}
