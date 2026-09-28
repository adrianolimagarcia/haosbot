package weixin

func PublicSetup() map[string]any {
	fields := []any{
		field("token","secret",true),
		field("allowFrom","list",false),
		field("baseUrl","string",false),
		field("routeTag","string",false),
		field("pollTimeout","int",false),
	}
	return map[string]any{
		"fields": fields,
		"required": []string{"token"},
		"simple_required_fields": []string{"token"},
		"verifies_connection": false,
	}
}
func field(name,kind string,required bool) map[string]any {
	return map[string]any{"field":name,"key":"channels.weixin."+name,"kind":kind,"choices":[]string{},"required":required}
}
