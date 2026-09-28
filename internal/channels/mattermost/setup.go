package mattermost

// PublicSetup returns the fields needed to configure the Mattermost bot.
func PublicSetup() map[string]any {
	fields := []any{
		field("serverUrl", "string", true),
		field("botToken", "secret", true),
		field("allowFrom", "list", false),
		field("allowedChannels", "list", false),
	}
	return map[string]any{
		"fields":                 fields,
		"required":               []string{"serverUrl", "botToken"},
		"simple_required_fields": []string{"serverUrl", "botToken"},
		"verifies_connection":    false,
	}
}

func field(name, kind string, required bool) map[string]any {
	return map[string]any{
		"field":    name,
		"key":      "channels.mattermost." + name,
		"kind":     kind,
		"choices":  []string{},
		"required": required,
	}
}
