package discord

// PublicSetup returns the fields required to configure the bot transport.
func PublicSetup() map[string]any {
	fields := []any{
		field("botToken", "secret", true),
		field("allowFrom", "list", false),
		field("allowedChannels", "list", false),
	}
	return map[string]any{
		"fields":                 fields,
		"required":               []string{"botToken"},
		"simple_required_fields": []string{"botToken"},
		"verifies_connection":    false,
	}
}

func field(name, kind string, required bool) map[string]any {
	return map[string]any{
		"field":    name,
		"key":      "channels.discord." + name,
		"kind":     kind,
		"choices":  []string{},
		"required": required,
	}
}
