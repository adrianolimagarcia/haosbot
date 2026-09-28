package feishu
import(
 "crypto/sha256"
 "encoding/hex"
 "testing"
 "github.com/adrianolimagarcia/nanobot-go/internal/channels"
)
func TestDefaults(t *testing.T){cfg,err:=sectionConfig(channels.NewMapSection(map[string]any{}));if err!=nil{t.Fatal(err)};if cfg.apiBase()!="https://open.feishu.cn"||cfg.WebhookPath!="/feishu/webhook"{t.Fatalf("cfg=%#v",cfg)}}
func TestSignature(t *testing.T){body:=[]byte(`{"x":1}`);ts,nonce,key:="1","n","k";sum:=sha256.Sum256(append([]byte(ts+nonce+key),body...));sig:=hex.EncodeToString(sum[:]);if !validSignature(body,key,ts,nonce,sig){t.Fatal("valid signature rejected")};if validSignature(body,key,ts,nonce,sig+"00"){t.Fatal("invalid accepted")}}
func TestLarkDomain(t *testing.T){cfg,err:=sectionConfig(channels.NewMapSection(map[string]any{"domain":"lark"}));if err!=nil{t.Fatal(err)};if cfg.apiBase()!="https://open.larksuite.com"{t.Fatal(cfg.apiBase())}}
