package feishu
func PublicSetup()map[string]any{
 fields:=[]any{field("appId","string",true),field("appSecret","secret",true),field("domain","select",false),field("verificationToken","secret",true),field("signatureKey","secret",false),field("host","string",false),field("port","int",false),field("webhookPath","string",false),field("allowFrom","list",false)}
 fields[2].(map[string]any)["choices"]=[]string{"feishu","lark"}
 return map[string]any{"fields":fields,"required":[]string{"appId","appSecret","verificationToken"},"simple_required_fields":[]string{"appId","appSecret","verificationToken"},"verifies_connection":false}
}
func field(name,kind string,required bool)map[string]any{return map[string]any{"field":name,"key":"channels.feishu."+name,"kind":kind,"choices":[]string{},"required":required}}
