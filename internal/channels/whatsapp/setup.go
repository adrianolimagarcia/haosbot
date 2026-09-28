package whatsapp

// PublicSetup describes the official WhatsApp Cloud API configuration.
func PublicSetup() map[string]any {
	fields := []any{
		field("phoneNumberId", "string", true),
		field("accessToken", "secret", true),
		field("appSecret", "secret", true),
		field("verifyToken", "secret", true),
		field("graphVersion", "string", true),
		field("graphBase", "string", false),
		field("listenAddr", "string", false),
		field("webhookPath", "string", false),
		field("allowFrom", "list", false),
	}
	return map[string]any{
		"fields":                 fields,
		"required":               []string{"phoneNumberId", "accessToken", "appSecret", "verifyToken", "graphVersion"},
		"simple_required_fields": []string{"phoneNumberId", "accessToken", "appSecret", "verifyToken", "graphVersion"},
		"verifies_connection":    false,
	}
}

func field(name, kind string, required bool) map[string]any {
	return map[string]any{
		"field": name, "key": "channels.whatsapp." + name,
		"kind": kind, "choices": []string{}, "required": required,
	}
}
