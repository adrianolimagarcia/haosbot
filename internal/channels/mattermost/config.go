package mattermost

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

const ChannelName = "mattermost"

// Config contains the Mattermost server URL, bot token, and optional scope.
type Config struct {
	Enabled        bool
	ServerURL      string
	BotToken       string
	AllowFrom      []string
	AllowedChannels []string
}

func parseConfig(values map[string]any) (Config, error) {
	cfg := Config{
		Enabled:   boolValue(values["enabled"], false),
		ServerURL: strings.TrimRight(strings.TrimSpace(stringValue(values["serverUrl"], "")), "/"),
		BotToken:  strings.TrimSpace(stringValue(values["botToken"], "")),
	}
	cfg.AllowFrom = stringList(values["allowFrom"])
	cfg.AllowedChannels = stringList(values["allowedChannels"])
	if cfg.ServerURL != "" {
		parsed, err := url.Parse(cfg.ServerURL)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return Config{}, fmt.Errorf("mattermost: serverUrl must be an http or https URL without credentials, query or fragment")
		}
	}
	if cfg.Enabled && (cfg.ServerURL == "" || cfg.BotToken == "") {
		return Config{}, fmt.Errorf("mattermost: serverUrl and botToken are required when enabled")
	}
	return cfg, nil
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
