package email

import (
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

const ChannelName = "email"

type Config struct {
	Enabled          bool
	ConsentGranted   bool
	IMAPHost         string
	IMAPPort         int
	IMAPUsername     string
	IMAPPassword     string
	IMAPUseTLS       bool
	Mailbox          string
	SMTPHost         string
	SMTPPort         int
	SMTPUsername     string
	SMTPPassword     string
	SMTPUseTLS       bool
	SMTPUseSSL       bool
	FromAddress      string
	AllowFrom        []string
	PollInterval     time.Duration
	AutoReplyEnabled bool
}

func sectionConfig(section channels.Section) (Config, error) {
	values, _ := section.Map()
	return parseConfig(values)
}

func parseConfig(values map[string]any) (Config, error) {
	cfg := Config{
		Enabled:          boolValue(values["enabled"], false),
		ConsentGranted:   boolValue(values["consentGranted"], false),
		IMAPHost:         strings.TrimSpace(stringValue(values["imapHost"], "")),
		IMAPPort:         intValue(values["imapPort"], 993),
		IMAPUsername:     strings.TrimSpace(stringValue(values["imapUsername"], "")),
		IMAPPassword:     stringValue(values["imapPassword"], ""),
		IMAPUseTLS:       boolValue(values["imapUseTls"], true),
		Mailbox:          strings.TrimSpace(stringValue(values["mailbox"], "INBOX")),
		SMTPHost:         strings.TrimSpace(stringValue(values["smtpHost"], "")),
		SMTPPort:         intValue(values["smtpPort"], 587),
		SMTPUsername:     strings.TrimSpace(stringValue(values["smtpUsername"], "")),
		SMTPPassword:     stringValue(values["smtpPassword"], ""),
		SMTPUseTLS:       boolValue(values["smtpUseTls"], true),
		SMTPUseSSL:       boolValue(values["smtpUseSsl"], false),
		FromAddress:      strings.TrimSpace(stringValue(values["fromAddress"], "")),
		AllowFrom:        stringList(values["allowFrom"]),
		PollInterval:     time.Duration(intValue(values["pollIntervalSeconds"], 30)) * time.Second,
		AutoReplyEnabled: boolValue(values["autoReplyEnabled"], true),
	}
	if cfg.Mailbox == "" {
		cfg.Mailbox = "INBOX"
	}
	if cfg.IMAPPort < 1 || cfg.IMAPPort > 65535 || cfg.SMTPPort < 1 || cfg.SMTPPort > 65535 {
		return Config{}, fmt.Errorf("email: IMAP/SMTP ports must be between 1 and 65535")
	}
	if cfg.PollInterval < 5*time.Second || cfg.PollInterval > 24*time.Hour {
		return Config{}, fmt.Errorf("email: pollIntervalSeconds must be between 5 and 86400")
	}
	for name, value := range map[string]string{
		"imapHost": cfg.IMAPHost, "imapUsername": cfg.IMAPUsername,
		"smtpHost": cfg.SMTPHost, "smtpUsername": cfg.SMTPUsername,
		"fromAddress": cfg.FromAddress,
	} {
		if strings.ContainsAny(value, "\r\n") {
			return Config{}, fmt.Errorf("email: %s contains a line break", name)
		}
	}
	if cfg.FromAddress != "" {
		if _, err := mail.ParseAddress(cfg.FromAddress); err != nil {
			return Config{}, fmt.Errorf("email: invalid fromAddress: %w", err)
		}
	}
	if cfg.SMTPUseSSL && cfg.SMTPUseTLS {
		return Config{}, fmt.Errorf("email: smtpUseSsl and smtpUseTls cannot both be true")
	}
	return cfg, nil
}

func (c Config) validateRuntime() error {
	if !c.ConsentGranted {
		return fmt.Errorf("email: consentGranted must be true before mailbox access is enabled")
	}
	missing := make([]string, 0, 8)
	for name, value := range map[string]string{
		"imapHost": c.IMAPHost, "imapUsername": c.IMAPUsername, "imapPassword": c.IMAPPassword,
		"smtpHost": c.SMTPHost, "smtpUsername": c.SMTPUsername, "smtpPassword": c.SMTPPassword,
		"fromAddress": c.FromAddress,
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("email: missing required setting(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

func stringValue(v any, fallback string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fallback
}
func boolValue(v any, fallback bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return fallback
}
func intValue(v any, fallback int) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return fallback
}
func stringList(v any) []string {
	switch x := v.(type) {
	case []string:
		return append([]string(nil), x...)
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case string:
		var out []string
		for _, item := range strings.Split(x, ",") {
			if s := strings.TrimSpace(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
