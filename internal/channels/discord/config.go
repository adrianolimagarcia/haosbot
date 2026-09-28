package discord

import (
	"fmt"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

const ChannelName = "discord"

// Config holds the Discord bot credentials and optional channel scope.
type Config struct {
	Enabled        bool
	BotToken       string
	AllowFrom      []string
	AllowedChannels []string
}

func parseConfig(values map[string]any) (Config, error) {
	cfg := Config{
		Enabled:  boolValue(values["enabled"], false),
		BotToken: strings.TrimSpace(stringValue(values["botToken"], "")),
	}
	cfg.AllowFrom = stringList(values["allowFrom"])
	cfg.AllowedChannels = stringList(values["allowedChannels"])
	if cfg.Enabled && cfg.BotToken == "" {
		return Config{}, fmt.Errorf("discord: botToken is required when enabled")
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
