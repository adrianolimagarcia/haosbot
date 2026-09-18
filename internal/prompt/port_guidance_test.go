package prompt

import (
	"strings"
	"testing"
)

// TestPortGuidanceIsPresentInSystemPrompt is the canary for the only
// system-prompt addition this port makes. The differential suite discounts that
// section so it can keep comparing the rest of the prompt byte for byte, which
// means a silent removal here would otherwise go unnoticed: the comparison would
// still pass, and the behaviour the section exists for would quietly regress.
func TestPortGuidanceIsPresentInSystemPrompt(t *testing.T) {
	ws := t.TempDir()
	full := New(ws).BuildSystemPrompt("cli", nil, ws, true)

	for _, want := range []string{"# Execution Efficiency", "# Measurement"} {
		if !strings.Contains(full, want) {
			t.Errorf("the built system prompt is missing the %q section", want)
		}
	}
	if !strings.Contains(full, PortGuidance()) {
		t.Error("the built system prompt does not contain the port guidance verbatim")
	}
	// It is appended last, so nothing may follow it. NormalizePortGuidance relies
	// on exactly that to discount it as a suffix.
	if !strings.HasSuffix(full, PortGuidance()) {
		t.Error("the port guidance is not the final section of the system prompt")
	}
}

// TestNormalizePortGuidanceRemovesExactlyTheSection pins the discount the
// differential suite applies. If this removal were wider than the section itself
// it would hide real drift in the rest of the prompt, which is the one thing the
// byte-for-byte comparison against the reference exists to catch.
func TestNormalizePortGuidanceRemovesExactlyTheSection(t *testing.T) {
	ws := t.TempDir()
	full := New(ws).BuildSystemPrompt("cli", nil, ws, true)

	stripped := NormalizePortGuidance(full)

	if strings.Contains(stripped, PortGuidance()) {
		t.Fatal("NormalizePortGuidance left the guidance section in place")
	}
	if stripped == full {
		t.Fatal("NormalizePortGuidance was a no-op on a prompt that carries the section")
	}
	// The result must be exactly the prompt with separator+section removed and
	// nothing else touched.
	want := strings.TrimSuffix(full, sectionSeparator+PortGuidance())
	if stripped != want {
		t.Errorf("NormalizePortGuidance removed more or less than the separator plus section:\n got %d bytes\nwant %d bytes",
			len(stripped), len(want))
	}
	if want == full {
		t.Fatal("the section is not a suffix of the built prompt; the discount is not exact")
	}

	// A prompt without the section is returned unchanged.
	const untouched = "no guidance here"
	if got := NormalizePortGuidance(untouched); got != untouched {
		t.Errorf("NormalizePortGuidance modified a prompt it should not touch: %q", got)
	}
	// Idempotent, so the differential harness can apply it unconditionally.
	if once, twice := stripped, NormalizePortGuidance(stripped); once != twice {
		t.Error("NormalizePortGuidance is not idempotent")
	}
}

// TestPortGuidanceStaysSmall guards the cost of the section. It is sent on every
// round trip, and round trips are exactly what it exists to reduce, so an
// unbounded section would defeat its own purpose.
func TestPortGuidanceStaysSmall(t *testing.T) {
	const budget = 2048
	if n := len(PortGuidance()); n > budget {
		t.Errorf("the port guidance section is %d bytes, over the %d-byte budget", n, budget)
	}
}
