package dingtalk

func PublicSetup() map[string]any {
	fields := []any{
		field("clientId", "string", true),
		field("clientSecret", "secret", true),
		field("allowFrom", "list", false),
		field("groupUserIsolation", "bool", false),
		field("disablePrivateChat", "bool", false),
	}
	return map[string]any{
		"fields": fields,
		"required": []string{"clientId", "clientSecret"},
		"simple_required_fields": []string{"clientId", "clientSecret"},
		"verifies_connection": false,
	}
}
func field(name, kind string, required bool) map[string]any {
	return map[string]any{
		"field": name, "key": "channels.dingtalk." + name,
		"kind": kind, "choices": []string{}, "required": required,
	}
}
