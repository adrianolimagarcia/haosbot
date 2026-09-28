package napcat

// PublicSetup returns the fields required for OneBot 11 configuration.
func PublicSetup() map[string]any {
	return PublicSetupFor(ChannelName)
}

// PublicSetupFor returns the OneBot fields under the requested config key.
func PublicSetupFor(channelName string) map[string]any {
	fields := []any{
		field(channelName, "websocketUrl", "string", true),
		field(channelName, "apiBase", "string", true),
		field(channelName, "accessToken", "secret", true),
		field(channelName, "selfId", "string", false),
		field(channelName, "allowFrom", "list", false),
		field(channelName, "allowedGroups", "list", false),
	}
	return map[string]any{
		"fields":                 fields,
		"required":               []string{"websocketUrl", "apiBase", "accessToken"},
		"simple_required_fields": []string{"websocketUrl", "apiBase", "accessToken"},
		"verifies_connection":    false,
	}
}

func field(channelName, name, kind string, required bool) map[string]any {
	return map[string]any{
		"field":    name,
		"key":      "channels." + channelName + "." + name,
		"kind":     kind,
		"choices":  []string{},
		"required": required,
	}
}
