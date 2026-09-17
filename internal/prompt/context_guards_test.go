package prompt

import (
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the three template guards ported from context.py. They mirror
// the reference's own unit tests (upstream/nanobot/tests/agent/test_context_builder.py,
// TestIsTemplateContent and "unmodified AGENTS.md/USER.md templates are
// skipped") and add the whitespace boundary cases that separate Python's
// str.strip() from Go's strings.TrimSpace.

// bundled returns the embedded template content or fails the test.
func bundled(t *testing.T, name string) string {
	t.Helper()
	content, ok := BundledTemplate(name)
	if !ok {
		t.Fatalf("template %q is not embedded", name)
	}
	return content
}

// freshInstall writes the shipped templates into ws exactly as the reference's
// sync_workspace_templates would on a first run: every file is byte-identical
// to the bundled default.
func freshInstall(t *testing.T, ws string) {
	t.Helper()
	for _, name := range []string{"AGENTS.md", "SOUL.md", "USER.md"} {
		mustWrite(t, filepath.Join(ws, name), bundled(t, name))
	}
	mustWrite(t, filepath.Join(ws, "memory", "MEMORY.md"), bundled(t, "memory/MEMORY.md"))
}

func TestFreshInstallWithholdsShippedDefaults(t *testing.T) {
	ws := t.TempDir()
	freshInstall(t, ws)

	got := New(ws).BuildSystemPrompt("cli", nil, ws, true)

	if strings.Contains(got, "## AGENTS.md") {
		t.Error("unmodified AGENTS.md must be skipped")
	}
	if strings.Contains(got, "## USER.md") {
		t.Error("unmodified USER.md must be skipped")
	}
	if strings.Contains(got, "# Memory") {
		t.Error("unmodified memory/MEMORY.md must be skipped")
	}
	// SOUL.md is never in _SKIPPABLE_DEFAULTS: the shipped SOUL.md is still
	// injected, because the agent needs its identity.
	if !strings.Contains(got, "## SOUL.md") {
		t.Error("SOUL.md must be present even when unmodified")
	}
}

func TestCustomisedBootstrapFilesAreIncluded(t *testing.T) {
	ws := t.TempDir()
	freshInstall(t, ws)
	mustWrite(t, filepath.Join(ws, "AGENTS.md"), "PROJECT RULES")
	mustWrite(t, filepath.Join(ws, "USER.md"), "USER FACTS")
	mustWrite(t, filepath.Join(ws, "memory", "MEMORY.md"), "REMEMBERED")

	got := New(ws).BuildSystemPrompt("cli", nil, ws, true)
	for _, want := range []string{"## AGENTS.md\n\nPROJECT RULES", "## USER.md\n\nUSER FACTS", "# Memory\n\n## Long-term Memory\nREMEMBERED"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestCustomisedSOULIsNotSubstituted(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "SOUL.md"), "MY OWN SOUL")

	got := New(ws).BuildSystemPrompt("cli", nil, ws, true)
	if !strings.Contains(got, "## SOUL.md\n\nMY OWN SOUL") {
		t.Errorf("customised SOUL.md must be used verbatim:\n%s", got)
	}
}

// TestLegacySOULIsUpgraded is context.py:208-212: a workspace SOUL.md that
// still matches the LEGACY bundled template is replaced by the current one.
func TestLegacySOULIsUpgraded(t *testing.T) {
	ws := t.TempDir()
	legacy := bundled(t, "legacy/SOUL.md")
	current := bundled(t, "SOUL.md")
	if legacy == current {
		t.Fatal("premise broken: legacy and current SOUL.md are identical")
	}
	mustWrite(t, filepath.Join(ws, "SOUL.md"), legacy)

	got := New(ws).BuildSystemPrompt("cli", nil, ws, true)
	if !strings.Contains(got, current) {
		t.Error("legacy SOUL.md was not upgraded to the current template")
	}
	if strings.Contains(got, legacy) {
		t.Error("legacy SOUL.md content leaked into the prompt")
	}
}

// TestWhitespaceOnlyFileIsSkipped exercises the `.strip()` boundary. Both
// cases are whitespace to Python; only the first is whitespace to Go's
// strings.TrimSpace, so the U+001C case is the one that would regress if the
// guard were ever rewritten with TrimSpace.
func TestWhitespaceOnlyFileIsSkipped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"ascii whitespace", "   \n\t\r\n "},
		{"U+001C..U+001F", "\x1c\x1d\x1e\x1f"},
		{"NEL and NBSP", "\u0085\u00a0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			mustWrite(t, filepath.Join(ws, "SOUL.md"), tc.content)

			got := New(ws).BuildSystemPrompt("cli", nil, ws, true)
			if strings.Contains(got, "## SOUL.md") {
				t.Errorf("blank SOUL.md (%q) must be skipped", tc.content)
			}
		})
	}
}

// TestTemplateWithSurroundingWhitespaceIsStillATemplate: both sides are
// stripped, so padding must not defeat the guard.
func TestTemplateWithSurroundingWhitespaceIsStillATemplate(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "AGENTS.md"), "\n\n  "+bundled(t, "AGENTS.md")+"\t\n\n")
	mustWrite(t, filepath.Join(ws, "USER.md"), bundled(t, "USER.md")+"\n")

	got := New(ws).BuildSystemPrompt("cli", nil, ws, true)
	if strings.Contains(got, "## AGENTS.md") {
		t.Error("padded AGENTS.md still matches the template and must be skipped")
	}
	if strings.Contains(got, "## USER.md") {
		t.Error("trailing-newline USER.md still matches the template and must be skipped")
	}
}

func TestMissingFilesAreSkipped(t *testing.T) {
	ws := t.TempDir()
	got := New(ws).BuildSystemPrompt("cli", nil, ws, true)
	for _, section := range []string{"## AGENTS.md", "## SOUL.md", "## USER.md", "# Memory"} {
		if strings.Contains(got, section) {
			t.Errorf("%s present for an empty workspace", section)
		}
	}
}

// TestMemoryUsesRawTruthiness is the `if memory` half of context.py:129. The
// reference tests the RAW string, not a stripped one, so a memory file holding
// only U+001C is injected verbatim. This is deliberately NOT a stripped
// emptiness test, and the two conditions are separate: the content must also
// fail the template comparison.
func TestMemoryUsesRawTruthiness(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "memory", "MEMORY.md"), "\x1c")

	got := New(ws).BuildSystemPrompt("cli", nil, ws, true)
	if !strings.Contains(got, "# Memory\n\n## Long-term Memory\n\x1c") {
		t.Error("non-empty (raw) memory must be injected even when it strips to blank")
	}
}

func TestMemoryTemplateIsWithheldButCustomisedIsKept(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "memory", "MEMORY.md"), bundled(t, "memory/MEMORY.md"))

	got := New(ws).BuildSystemPrompt("cli", nil, ws, true)
	if strings.Contains(got, "# Memory") {
		t.Error("unmodified MEMORY.md must be withheld")
	}

	mustWrite(t, filepath.Join(ws, "memory", "MEMORY.md"), bundled(t, "memory/MEMORY.md")+"\nextra note\n")
	got = New(ws).BuildSystemPrompt("cli", nil, ws, true)
	if !strings.Contains(got, "# Memory") {
		t.Error("appended-to MEMORY.md is customised and must be included")
	}
}
