package weixin

import (
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

func TestConfigDefaults(t *testing.T) {
	cfg,err:=sectionConfig(channels.NewMapSection(map[string]any{})); if err!=nil{t.Fatal(err)}
	if cfg.BaseURL!="https://ilinkai.weixin.qq.com" || cfg.PollTimeout.Seconds()!=35 { t.Fatalf("unexpected defaults: %#v",cfg) }
}
func TestBuildClientVersion(t *testing.T) {
	if got:=buildClientVersion("2.4.6");got!=0x020406{t.Fatalf("version=%x",got)}
}
func TestSplitText(t *testing.T) {
	got:=splitText(strings.Repeat("界",1801),1800)
	if len(got)!=2 || len([]rune(got[0]))!=1800 || len([]rune(got[1]))!=1 {t.Fatalf("chunks=%d lens=%d/%d",len(got),len([]rune(got[0])),len([]rune(got[1])))}
}
