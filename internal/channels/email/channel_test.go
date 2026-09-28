package email

import (
	"strings"
	"testing"
)

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := parseConfig(map[string]any{
		"imapHost": "imap.example.com",
		"smtpHost": "smtp.example.com",
		"fromAddress": "bot@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IMAPPort != 993 || cfg.SMTPPort != 587 || !cfg.IMAPUseTLS || !cfg.SMTPUseTLS || cfg.SMTPUseSSL {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	if cfg.PollInterval.Seconds() != 30 || !cfg.AutoReplyEnabled || cfg.Mailbox != "INBOX" {
		t.Fatalf("unexpected runtime defaults: %#v", cfg)
	}
}

func TestConfigRequiresConsentAtRuntime(t *testing.T) {
	cfg, err := parseConfig(map[string]any{
		"imapHost": "imap.example.com", "imapUsername": "u", "imapPassword": "p",
		"smtpHost": "smtp.example.com", "smtpUsername": "u", "smtpPassword": "p",
		"fromAddress": "bot@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.validateRuntime(); err == nil || !strings.Contains(err.Error(), "consentGranted") {
		t.Fatalf("validateRuntime() = %v, want consent error", err)
	}
}

func TestParseSearchUIDs(t *testing.T) {
	got := parseSearchUIDs([]string{"* 4 EXISTS\r\n", "* SEARCH 1 5 42\r\n", "A0002 OK done\r\n"})
	if strings.Join(got, ",") != "1,5,42" {
		t.Fatalf("UIDs = %v", got)
	}
}

func TestStripHTML(t *testing.T) {
	if got := stripHTML("<p>Hello &amp; <b>world</b></p>"); got != "Hello & world" {
		t.Fatalf("stripHTML = %q", got)
	}
}
