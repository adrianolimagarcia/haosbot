package websocket

// PublicSetup describes the fields the generic WebUI can write. Secret values
// are redacted by the config endpoint and are deliberately never returned here.
func PublicSetup() map[string]any {
	fields := []any{
		field("host", "string", false, defaultHost),
		field("port", "int", false, defaultPort),
		field("path", "string", false, defaultPath),
		field("token", "secret", true, nil),
		field("allowFrom", "list", false, nil),
		field("streaming", "bool", false, true),
		field("maxMessageBytes", "int", false, defaultMaxMessage),
		field("maxConnections", "int", false, defaultMaxClients),
	}
	return map[string]any{"fields": fields, "required": []string{"token"}, "simple_required_fields": []string{"token"}, "verifies_connection": false}
}

func field(name, kind string, required bool, defaultValue any) map[string]any {
	result := map[string]any{"field": name, "key": "channels.websocket." + name, "kind": kind, "choices": []string{}, "required": required}
	if defaultValue != nil { result["default_value"] = defaultValue }
	return result
}
