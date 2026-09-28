package linear

func PublicSetup() map[string]any {
	fields := []any{
		field("apiKey", "secret", true),
		field("webhookSigningSecret", "secret", true),
		field("listenAddr", "string", false),
		field("webhookPath", "string", false),
		field("allowFrom", "list", false),
	}
	return map[string]any{
		"fields": fields,
		"required": []string{"apiKey", "webhookSigningSecret"},
		"simple_required_fields": []string{"apiKey", "webhookSigningSecret"},
		"verifies_connection": false,
	}
}

func field(name, kind string, required bool) map[string]any {
	return map[string]any{
		"field": name, "key": "channels.linear." + name,
		"kind": kind, "choices": []string{}, "required": required,
	}
}
