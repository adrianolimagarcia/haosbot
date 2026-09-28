package matrix

// PublicSetup returns the non-secret form contract used by the WebUI.
func PublicSetup() map[string]any {
	fields := []any{
		field("homeserver", "string", true, nil),
		field("accessToken", "secret", true, nil),
		field("userId", "string", true, nil),
		field("rooms", "list", false, nil),
		field("allowFrom", "list", false, nil),
		field("syncTimeoutMs", "int", false, 30000),
	}
	return map[string]any{
		"fields":                 fields,
		"required":               []string{"homeserver", "accessToken", "userId"},
		"simple_required_fields": []string{"homeserver", "accessToken", "userId"},
		"verifies_connection":    false,
	}
}

func field(name, kind string, required bool, defaultValue any) map[string]any {
	result := map[string]any{
		"field":    name,
		"key":      "channels.matrix." + name,
		"kind":     kind,
		"choices":  []string{},
		"required": required,
	}
	if defaultValue != nil {
		result["default_value"] = defaultValue
	}
	return result
}
