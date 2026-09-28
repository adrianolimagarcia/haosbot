package slack

// PublicSetup returns the non-secret fields needed to configure Socket Mode.
func PublicSetup() map[string]any {
	fields := []any{
		field("botToken", "secret", true),
		field("appToken", "secret", true),
		field("allowFrom", "list", false),
		field("allowedRooms", "list", false),
	}
	return map[string]any{
		"fields":                 fields,
		"required":               []string{"botToken", "appToken"},
		"simple_required_fields": []string{"botToken", "appToken"},
		"verifies_connection":    false,
	}
}

func field(name, kind string, required bool) map[string]any {
	return map[string]any{
		"field":    name,
		"key":      "channels.slack." + name,
		"kind":     kind,
		"choices":  []string{},
		"required": required,
	}
}
