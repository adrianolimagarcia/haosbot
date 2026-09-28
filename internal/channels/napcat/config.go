package napcat

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

const ChannelName = "napcat"

// Config specifies the Napcat OneBot 11 WebSocket and HTTP API endpoints.
type Config struct {
	Enabled       bool
	WebSocketURL  string
	APIBase       string
	AccessToken   string
	SelfID        string
	AllowFrom     []string
	AllowedGroups []string
}

func parseConfig(values map[string]any) (Config, error) {
	cfg := Config{
		Enabled:      boolValue(values["enabled"], false),
		WebSocketURL: strings.TrimSpace(stringValue(values["websocketUrl"], "")),
		APIBase:      strings.TrimRight(strings.TrimSpace(stringValue(values["apiBase"], "")), "/"),
		AccessToken:  strings.TrimSpace(stringValue(values["accessToken"], "")),
		SelfID:       strings.TrimSpace(stringValue(values["selfId"], "")),
	}
	cfg.AllowFrom = stringList(values["allowFrom"])
	cfg.AllowedGroups = stringList(values["allowedGroups"])
	if cfg.WebSocketURL != "" {
		if err := validateURL(cfg.WebSocketURL, "ws", "wss"); err != nil {
			return Config{}, fmt.Errorf("napcat: %w", err)
		}
	}
	if cfg.APIBase != "" {
		if err := validateURL(cfg.APIBase, "http", "https"); err != nil {
			return Config{}, fmt.Errorf("napcat: %w", err)
		}
	}
	if cfg.Enabled && (cfg.WebSocketURL == "" || cfg.APIBase == "" || cfg.AccessToken == "") {
		return Config{}, fmt.Errorf("napcat: websocketUrl, apiBase and accessToken are required when enabled")
	}
	return cfg, nil
}

func validateURL(value string, schemes ...string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("endpoint must be an absolute URL without credentials, query or fragment")
	}
	for _, scheme := range schemes {
		if parsed.Scheme == scheme {
			return nil
		}
	}
	return fmt.Errorf("endpoint must use %s", strings.Join(schemes, " or "))
}

func stringValue(value any, fallback string) string {
	if text, ok := value.(string); ok {
		return text
	}
	return fallback
}

func boolValue(value any, fallback bool) bool {
	if flag, ok := value.(bool); ok {
		return flag
	}
	return fallback
}

func stringList(value any) []string {
	var result []string
	switch typed := value.(type) {
	case []string:
		result = append(result, typed...)
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
	case string:
		for _, item := range strings.Split(typed, ",") {
			if text := strings.TrimSpace(item); text != "" {
				result = append(result, text)
			}
		}
	}
	return result
}

func sectionConfig(section channels.Section) (Config, error) {
	values, _ := section.Map()
	return parseConfig(values)
}
