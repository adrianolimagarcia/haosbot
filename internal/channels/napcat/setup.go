package napcat

// PublicSetup returns the fields required for OneBot 11 configuration.
func PublicSetup() map[string]any {
	fields := []any{
		field("websocketUrl", "string", true),
		field("apiBase", "string", true),
		field("accessToken", "secret", true),
		field("selfId", "string", false),
		field("allowFrom", "list", false),
		field("allowedGroups", "list", false),
	}
	return map[string]any{
		"fields":                 fields,
		"required":               []string{"websocketUrl", "apiBase", "accessToken"},
		"simple_required_fields": []string{"websocketUrl", "apiBase", "accessToken"},
		"verifies_connection":    false,
	}
}

func field(name, kind string, required bool) map[string]any {
	return map[string]any{
		"field":    name,
		"key":      "channels.napcat." + name,
		"kind":     kind,
		"choices":  []string{},
		"required": required,
	}
}
