package websocket

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

const (
	ChannelName       = "websocket"
	defaultHost       = "127.0.0.1"
	defaultPort       = 8765
	defaultPath       = "/"
	defaultMaxMessage = 1 << 20
	maxMessageLimit   = 8 << 20
	defaultMaxClients = 64
	maxClientLimit    = 256
)

// Config is the supported WebSocket server configuration. The listener binds
// to loopback by default and always requires a shared token.
type Config struct {
	Enabled         bool
	Host            string
	Port            int
	Path            string
	Token           string
	AllowFrom       []string
	Streaming       bool
	MaxMessageBytes int64
	MaxConnections  int
}

func defaultConfig() Config {
	return Config{Host: defaultHost, Port: defaultPort, Path: defaultPath,
		Streaming: true, MaxMessageBytes: defaultMaxMessage, MaxConnections: defaultMaxClients}
}

func parseConfig(values map[string]any) (Config, error) {
	cfg := defaultConfig()
	cfg.Enabled = boolValue(values["enabled"], false)
	cfg.Host = stringValue(values["host"], cfg.Host)
	cfg.Port = intValue(values["port"], cfg.Port)
	cfg.Path = stringValue(values["path"], cfg.Path)
	cfg.Token = stringValue(values["token"], "")
	cfg.Streaming = boolValue(values["streaming"], cfg.Streaming)
	cfg.MaxMessageBytes = int64(intValue(values["maxMessageBytes"], int(cfg.MaxMessageBytes)))
	cfg.MaxConnections = intValue(values["maxConnections"], cfg.MaxConnections)
	if raw, ok := values["allowFrom"].([]any); ok {
		for _, item := range raw {
			if value, ok := item.(string); ok {
				cfg.AllowFrom = append(cfg.AllowFrom, value)
			}
		}
	} else if raw, ok := values["allowFrom"].([]string); ok {
		cfg.AllowFrom = append(cfg.AllowFrom, raw...)
	}
	if cfg.Host == "" { cfg.Host = defaultHost }
	if cfg.Path == "" { cfg.Path = defaultPath }
	if cfg.Port < 0 || cfg.Port > 65535 { return Config{}, fmt.Errorf("websocket: port must be between 0 and 65535") }
	if !strings.HasPrefix(cfg.Path, "/") { return Config{}, fmt.Errorf("websocket: path must start with /") }
	parsed, err := url.ParseRequestURI(cfg.Path)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" { return Config{}, fmt.Errorf("websocket: path must be an absolute URL path without query or fragment") }
	if cfg.MaxMessageBytes < 1024 || cfg.MaxMessageBytes > maxMessageLimit {
		return Config{}, fmt.Errorf("websocket: maxMessageBytes must be between 1024 and %d", maxMessageLimit)
	}
	if cfg.MaxConnections < 1 || cfg.MaxConnections > maxClientLimit {
		return Config{}, fmt.Errorf("websocket: maxConnections must be between 1 and %d", maxClientLimit)
	}
	if cfg.Enabled && strings.TrimSpace(cfg.Token) == "" { return Config{}, fmt.Errorf("websocket: token is required when enabled") }
	if net.ParseIP(cfg.Host) == nil && cfg.Host != "localhost" { return Config{}, fmt.Errorf("websocket: host must be an IP address or localhost") }
	return cfg, nil
}

func stringValue(v any, fallback string) string {
	if value, ok := v.(string); ok { return value }
	return fallback
}

func boolValue(v any, fallback bool) bool {
	if value, ok := v.(bool); ok { return value }
	return fallback
}

func intValue(v any, fallback int) int {
	switch value := v.(type) {
	case int: return value
	case int64: return int(value)
	case float64: return int(value)
	case string:
		parsed, err := strconv.Atoi(value); if err == nil { return parsed }
	}
	return fallback
}

func sectionConfig(section channels.Section) (Config, error) {
	values, _ := section.Map()
	return parseConfig(values)
}
