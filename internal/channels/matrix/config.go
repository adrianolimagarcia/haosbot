package matrix

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

const (
	ChannelName        = "matrix"
	defaultSyncTimeout = 30_000
	maxSyncTimeout     = 120_000
	maxResponseBytes   = 16 << 20
)

// Config contains the credentials and scope for the Matrix client API.
type Config struct {
	Enabled        bool
	Homeserver     string
	AccessToken    string
	UserID         string
	Rooms          []string
	AllowFrom      []string
	SyncTimeoutMS  int
}

func parseConfig(values map[string]any) (Config, error) {
	cfg := Config{
		Enabled:       boolValue(values["enabled"], false),
		Homeserver:    strings.TrimRight(stringValue(values["homeserver"], ""), "/"),
		AccessToken:   strings.TrimSpace(stringValue(values["accessToken"], "")),
		UserID:        strings.TrimSpace(stringValue(values["userId"], "")),
		SyncTimeoutMS: intValue(values["syncTimeoutMs"], defaultSyncTimeout),
	}
	cfg.Rooms = stringList(values["rooms"])
	cfg.AllowFrom = stringList(values["allowFrom"])
	if cfg.SyncTimeoutMS < 1_000 || cfg.SyncTimeoutMS > maxSyncTimeout {
		return Config{}, fmt.Errorf("matrix: syncTimeoutMs must be between 1000 and %d", maxSyncTimeout)
	}
	if cfg.Homeserver != "" {
		parsed, err := url.Parse(cfg.Homeserver)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return Config{}, fmt.Errorf("matrix: homeserver must be an http or https URL without credentials, query or fragment")
		}
	}
	if cfg.Enabled {
		if cfg.Homeserver == "" || cfg.AccessToken == "" || cfg.UserID == "" {
			return Config{}, fmt.Errorf("matrix: homeserver, accessToken and userId are required when enabled")
		}
	}
	return cfg, nil
}

func stringValue(value any, fallback string) string {
	if text, ok := value.(string); ok {
		return text
	}
	return fallback
}

func intValue(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		parsed, err := strconv.Atoi(typed)
		if err == nil {
			return parsed
		}
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
	}
	return result
}

func sectionConfig(section channels.Section) (Config, error) {
	values, _ := section.Map()
	return parseConfig(values)
}
