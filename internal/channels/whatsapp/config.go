package whatsapp

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

const ChannelName = "whatsapp"

var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+$`)

type Config struct {
	Enabled       bool
	ListenAddr    string
	WebhookPath   string
	PhoneNumberID string
	AccessToken   string
	AppSecret     string
	VerifyToken   string
	GraphVersion  string
	GraphBase     string
	AllowFrom     []string
}

func parseConfig(values map[string]any) (Config, error) {
	cfg := Config{
		Enabled:       boolValue(values["enabled"], false),
		ListenAddr:    strings.TrimSpace(stringValue(values["listenAddr"], "127.0.0.1:8089")),
		WebhookPath:   strings.TrimSpace(stringValue(values["webhookPath"], "/webhooks/whatsapp")),
		PhoneNumberID: strings.TrimSpace(stringValue(values["phoneNumberId"], "")),
		AccessToken:   strings.TrimSpace(stringValue(values["accessToken"], "")),
		AppSecret:     strings.TrimSpace(stringValue(values["appSecret"], "")),
		VerifyToken:   strings.TrimSpace(stringValue(values["verifyToken"], "")),
		GraphVersion:  strings.TrimSpace(stringValue(values["graphVersion"], "")),
		GraphBase:     strings.TrimRight(strings.TrimSpace(stringValue(values["graphBase"], "https://graph.facebook.com")), "/"),
		AllowFrom:     stringList(values["allowFrom"]),
	}
	if cfg.WebhookPath == "" || !strings.HasPrefix(cfg.WebhookPath, "/") || strings.ContainsAny(cfg.WebhookPath, "?#") {
		return Config{}, fmt.Errorf("whatsapp: webhookPath must be an absolute path without query or fragment")
	}
	if cfg.GraphBase != "" {
		parsed, err := url.Parse(cfg.GraphBase)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopbackHost(parsed.Hostname()))) || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return Config{}, fmt.Errorf("whatsapp: graphBase must be an https URL (http is allowed for loopback tests/services) without credentials, query or fragment")
		}
	}
	if cfg.GraphVersion != "" && !versionPattern.MatchString(cfg.GraphVersion) {
		return Config{}, fmt.Errorf("whatsapp: graphVersion must use the form vNN.N")
	}
	if cfg.Enabled && (cfg.ListenAddr == "" || cfg.PhoneNumberID == "" || cfg.AccessToken == "" || cfg.AppSecret == "" || cfg.VerifyToken == "" || cfg.GraphVersion == "") {
		return Config{}, fmt.Errorf("whatsapp: listenAddr, phoneNumberId, accessToken, appSecret, verifyToken and graphVersion are required when enabled")
	}
	return cfg, nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
