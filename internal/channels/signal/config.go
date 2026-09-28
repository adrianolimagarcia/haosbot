package signal

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

const ChannelName = "signal"

// Config connects to a signal-cli-rest-api service. Signal's protocol remains
// owned by that companion service; HAOSbot only speaks its HTTP/WebSocket API.
type Config struct {
	Enabled   bool
	APIBase   string
	Number    string
	APIToken  string
	AllowFrom []string
}

func parseConfig(values map[string]any) (Config, error) {
	cfg := Config{
		Enabled:   boolValue(values["enabled"], false),
		APIBase:   strings.TrimRight(strings.TrimSpace(stringValue(values["apiBase"], "")), "/"),
		Number:    strings.TrimSpace(stringValue(values["number"], "")),
		APIToken:  strings.TrimSpace(stringValue(values["apiToken"], "")),
		AllowFrom: stringList(values["allowFrom"]),
	}
	if cfg.APIBase != "" {
		parsed, err := url.Parse(cfg.APIBase)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return Config{}, fmt.Errorf("signal: apiBase must be an http or https URL without credentials, query or fragment")
		}
	}
	if cfg.Enabled && (cfg.APIBase == "" || cfg.Number == "") {
		return Config{}, fmt.Errorf("signal: apiBase and number are required when enabled")
	}
	return cfg, nil
}

func sectionConfig(section channels.Section) (Config, error) {
	values, _ := section.Map()
	return parseConfig(values)
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
