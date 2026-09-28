package signal

// PublicSetup describes the signal-cli-rest-api fields surfaced by WebUI.
func PublicSetup() map[string]any {
	return map[string]any{
		"fields": []any{
			field("apiBase", "string", true),
			field("number", "string", true),
			field("apiToken", "secret", false),
			field("allowFrom", "list", false),
		},
		"required":               []string{"apiBase", "number"},
		"simple_required_fields": []string{"apiBase", "number"},
		"verifies_connection":    false,
		"external_dependency":    "signal-cli-rest-api",
	}
}

func field(name, kind string, required bool) map[string]any {
	return map[string]any{
		"field": name, "key": "channels.signal." + name,
		"kind": kind, "choices": []string{}, "required": required,
	}
}
