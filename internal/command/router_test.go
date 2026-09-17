package command

import (
	"strings"
	"testing"
)

func TestIsSlashCommand(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"/help", true},
		{"   /status  ", true},
		{"/model gpt-4o", true},
		{"hello /world", false},
		{"not a command", false},
		{"", false},
		{"   ", false},
	}

	for _, tc := range cases {
		got := IsSlashCommand(tc.input)
		if got != tc.want {
			t.Errorf("IsSlashCommand(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestRouterExecute(t *testing.T) {
	r := NewRouter()

	// Non-command
	res, handled := r.Execute("just a message", "test-model", "/tmp/ws")
	if handled || res != "" {
		t.Errorf("Execute non-command handled=%v, res=%q", handled, res)
	}

	// /help
	res, handled = r.Execute("/help", "test-model", "/tmp/ws")
	if !handled || !strings.Contains(res, "Comandos Slash Disponíveis") {
		t.Errorf("/help returned unexpected: handled=%v, res=%q", handled, res)
	}

	// /status
	res, handled = r.Execute("/status", "my-test-model", "/tmp/my-ws")
	if !handled || !strings.Contains(res, "my-test-model") || !strings.Contains(res, "/tmp/my-ws") {
		t.Errorf("/status returned unexpected: handled=%v, res=%q", handled, res)
	}

	// /skills
	res, handled = r.Execute("/skills", "my-test-model", "/tmp/my-ws")
	if !handled || !strings.Contains(res, "Habilidades Nativas") {
		t.Errorf("/skills returned unexpected: handled=%v, res=%q", handled, res)
	}

	// /new
	res, handled = r.Execute("/new", "my-test-model", "/tmp/my-ws")
	if !handled || !strings.Contains(res, "Sessão reiniciada") {
		t.Errorf("/new returned unexpected: handled=%v, res=%q", handled, res)
	}

	// /model without args
	res, handled = r.Execute("/model", "current-model-123", "/tmp/ws")
	if !handled || !strings.Contains(res, "current-model-123") {
		t.Errorf("/model without args unexpected: handled=%v, res=%q", handled, res)
	}

	// /model with args
	res, handled = r.Execute("/model new-model-456", "current-model-123", "/tmp/ws")
	if !handled || !strings.Contains(res, "new-model-456") {
		t.Errorf("/model with args unexpected: handled=%v, res=%q", handled, res)
	}

	// Unknown slash command
	res, handled = r.Execute("/unknown_command", "test-model", "/tmp/ws")
	if !handled || !strings.Contains(res, "Comando desconhecido") {
		t.Errorf("unknown command unexpected: handled=%v, res=%q", handled, res)
	}
}
