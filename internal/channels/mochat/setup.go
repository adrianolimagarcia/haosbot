package mochat
func PublicSetup()map[string]any{
	fields:=[]any{
		field("baseUrl","string",false),field("clawToken","secret",true),field("agentUserId","string",false),
		field("sessions","list",false),field("panels","list",false),field("allowFrom","list",false),
		field("refreshIntervalMs","int",false),field("watchTimeoutMs","int",false),
		field("watchLimit","int",false),field("retryDelayMs","int",false),
	}
	return map[string]any{"fields":fields,"required":[]string{"clawToken"},"simple_required_fields":[]string{"clawToken"},"verifies_connection":false}
}
func field(name,kind string,required bool)map[string]any{return map[string]any{"field":name,"key":"channels.mochat."+name,"kind":kind,"choices":[]string{},"required":required}}
