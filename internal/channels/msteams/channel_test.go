package msteams

import (
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

func TestDefaultsAndTrustedServiceURL(t *testing.T){
	c,err:=New(channels.NewMapSection(map[string]any{}),nil);if err!=nil{t.Fatal(err)}
	if !c.cfg.ValidateInboundAuth||!c.cfg.ReplyInThread{t.Fatalf("unsafe defaults: %#v",c.cfg)}
	for _,u:=range []string{"https://smba.trafficmanager.net/emea/","https://foo.botframework.com/path"}{if !c.trustedServiceURL(u){t.Fatalf("expected trusted: %s",u)}}
	for _,u:=range []string{"http://smba.trafficmanager.net/","https://botframework.com.evil.example/","https://webchat.example.com/"}{if c.trustedServiceURL(u){t.Fatalf("expected untrusted: %s",u)}}
}
func TestSanitizeText(t *testing.T){
	got:=sanitizeText("<at>HAOS</at> Hello&nbsp; <b>world</b>")
	if got!="Hello world"{t.Fatalf("sanitizeText=%q",got)}
}
func TestAudienceContains(t *testing.T){
	if !audienceContains([]any{"x","app"},"app")||audienceContains("x","app"){t.Fatal("audience matching failed")}
}
