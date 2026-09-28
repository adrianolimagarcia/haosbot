package wecom

func PublicSetup() map[string]any {
	fields := []any{
		field("botId", "string", true),
		field("secret", "secret", true),
		field("allowFrom", "list", false),
		field("welcomeMessage", "string", false),
		field("heartbeatIntervalMs", "int", false),
	}
	return map[string]any{
		"fields": fields,
		"required": []string{"botId", "secret"},
		"simple_required_fields": []string{"botId", "secret"},
		"verifies_connection": false,
	}
}
func field(name, kind string, required bool) map[string]any {
	return map[string]any{
		"field": name, "key": "channels.wecom." + name,
		"kind": kind, "choices": []string{}, "required": required,
	}
}
