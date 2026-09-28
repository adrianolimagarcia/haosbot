package msteams

func PublicSetup() map[string]any {
	fields:=[]any{
		field("appId","string",true), field("appPassword","secret",true),
		field("tenantId","string",false), field("host","string",false),
		field("port","int",false), field("path","string",false),
		field("allowFrom","list",false), field("replyInThread","bool",false),
		field("mentionOnlyResponse","string",false), field("validateInboundAuth","bool",false),
		field("trustedServiceUrlHosts","list",false),
	}
	return map[string]any{
		"fields":fields,
		"required":[]string{"appId","appPassword"},
		"simple_required_fields":[]string{"appId","appPassword"},
		"verifies_connection":false,
	}
}
func field(name,kind string,required bool)map[string]any{
	return map[string]any{"field":name,"key":"channels.msteams."+name,"kind":kind,"choices":[]string{},"required":required}
}
