package wecom

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

func TestConfigDefaults(t *testing.T) {
	cfg, err := sectionConfig(channels.NewMapSection(map[string]any{}))
	if err != nil { t.Fatal(err) }
	if cfg.WebSocketURL != "wss://openws.work.weixin.qq.com" || cfg.HeartbeatInterval != 30_000_000_000 {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
}

func TestGenerateReqID(t *testing.T) {
	a := generateReqID("ping")
	b := generateReqID("ping")
	if !strings.HasPrefix(a, "ping-") || a == b { t.Fatalf("bad ids: %q %q", a, b) }
}

func TestInboundMessageShape(t *testing.T) {
	var msg inboundMessage
	if err := json.Unmarshal([]byte(`{"msgid":"m1","chatid":"c1","chattype":"single","from":{"userid":"u1"},"msgtype":"text","text":{"content":"hello"}}`), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.MsgID != "m1" || msg.ChatID != "c1" || msg.From.UserID != "u1" || msg.Text.Content != "hello" {
		t.Fatalf("decoded=%#v", msg)
	}
}
