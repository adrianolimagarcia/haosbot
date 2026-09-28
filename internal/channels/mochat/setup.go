package mochat
func PublicSetup()map[string]any{
	fields:=[]any{
		setupField("baseUrl","string",false),setupField("clawToken","secret",true),setupField("agentUserId","string",false),
		setupField("sessions","list",false),setupField("panels","list",false),setupField("allowFrom","list",false),
		setupField("refreshIntervalMs","int",false),setupField("watchTimeoutMs","int",false),
		setupField("watchLimit","int",false),setupField("retryDelayMs","int",false),
	}
	return map[string]any{"fields":fields,"required":[]string{"clawToken"},"simple_required_fields":[]string{"clawToken"},"verifies_connection":false}
}
func setupField(name,kind string,required bool)map[string]any{return map[string]any{"field":name,"key":"channels.mochat."+name,"kind":kind,"choices":[]string{},"required":required}}
