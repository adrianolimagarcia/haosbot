package agent

import "testing"

func TestDeterministicTurnIDIsStableAndInputBound(t *testing.T) {
	first := deterministicTurnID("webui:session", 4, "hello")
	if first == "" {
		t.Fatal("empty turn ID")
	}
	if again := deterministicTurnID("webui:session", 4, "hello"); again != first {
		t.Fatalf("same logical turn changed: %q != %q", first, again)
	}
	if otherHistory := deterministicTurnID("webui:session", 5, "hello"); otherHistory == first {
		t.Fatal("history position did not participate in turn ID")
	}
	if otherContent := deterministicTurnID("webui:session", 4, "hello!"); otherContent == first {
		t.Fatal("content did not participate in turn ID")
	}
}
