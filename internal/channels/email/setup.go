package email

func PublicSetup() map[string]any {
	fields := []any{
		field("consentGranted", "bool", true),
		field("imapHost", "string", true),
		field("imapPort", "int", true),
		field("imapUsername", "string", true),
		field("imapPassword", "secret", true),
		field("imapUseTls", "bool", false),
		field("mailbox", "string", false),
		field("smtpHost", "string", true),
		field("smtpPort", "int", true),
		field("smtpUsername", "string", true),
		field("smtpPassword", "secret", true),
		field("smtpUseTls", "bool", false),
		field("smtpUseSsl", "bool", false),
		field("fromAddress", "string", true),
		field("allowFrom", "list", false),
		field("pollIntervalSeconds", "int", false),
		field("autoReplyEnabled", "bool", false),
	}
	return map[string]any{
		"fields": fields,
		"required": []string{
			"consentGranted", "imapHost", "imapPort", "imapUsername", "imapPassword",
			"smtpHost", "smtpPort", "smtpUsername", "smtpPassword", "fromAddress",
		},
		"simple_required_fields": []string{
			"consentGranted", "imapHost", "imapUsername", "imapPassword",
			"smtpHost", "smtpUsername", "smtpPassword", "fromAddress",
		},
		"verifies_connection": false,
	}
}

func field(name, kind string, required bool) map[string]any {
	return map[string]any{
		"field": name, "key": "channels.email." + name,
		"kind": kind, "choices": []string{}, "required": required,
	}
}
