package mochat
import(
 "testing"
 "github.com/adrianolimagarcia/nanobot-go/internal/channels"
)
func TestDefaults(t *testing.T){cfg,err:=sectionConfig(channels.NewMapSection(map[string]any{}));if err!=nil{t.Fatal(err)};if cfg.BaseURL!="https://mochat.io"||cfg.WatchLimit!=100||cfg.WatchTimeout.Milliseconds()!=25000{t.Fatalf("defaults=%#v",cfg)}}
func TestNormalizeTargets(t *testing.T){got,auto:=normalizeTargets([]string{"a","*","a","b"});if !auto||len(got)!=2{t.Fatalf("targets=%v auto=%v",got,auto)}}
func TestNormalizeContent(t *testing.T){if got:=normalizeContent(map[string]any{"text":" hi "});got!="hi"{t.Fatalf("got=%q",got)}}
