package prompt

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// TestRenderDreamPromptSubstitutesAndRstrips pins the two things
// render_template(name, strip=True, ...) does: substitute the one variable and
// then apply Python's str.rstrip().
//
// The trailing whitespace matters because the embedded file ends with a
// newline: a port that used strings.TrimRight("\n") would leave a trailing
// space, and one that used TrimSpace would also strip a LEADING separator
// character that Python's rstrip leaves alone.
func TestRenderDreamPromptSubstitutesAndRstrips(t *testing.T) {
	const path = "/skills/skill-creator/SKILL.md"
	got := RenderDreamPrompt(path)

	if !strings.Contains(got, "`"+path+"` for format") {
		t.Errorf("skill_creator_path was not substituted:\n%s", got[:200])
	}
	if strings.Contains(got, dreamSkillCreatorPlaceholder) {
		t.Error("the placeholder survived rendering")
	}
	if strings.HasSuffix(got, "\n") || strings.HasSuffix(got, " ") {
		t.Errorf("rendered prompt has trailing whitespace: %q", got[len(got)-10:])
	}
	if want := textutil.PyRStrip(strings.ReplaceAll(dreamTemplate, dreamSkillCreatorPlaceholder, path)); got != want {
		t.Error("rendering is not PyRStrip(substituted template)")
	}
}

// TestRenderDreamPromptKeepsLeadingSeparator checks the trim is a RIGHT trim:
// U+001C is whitespace to Python but not to Go, and rstrip must not touch it at
// the start of the string.
func TestRenderDreamPromptKeepsLeadingSeparator(t *testing.T) {
	// The template does not start with whitespace, so this asserts the
	// property on the helper the renderer uses rather than on the file.
	if got := textutil.PyRStrip("\x1cbody"); got != "\x1cbody" {
		t.Errorf("PyRStrip removed a leading separator: %q", got)
	}
	if got := textutil.PyRStrip("body\x1c"); got != "body" {
		t.Errorf("PyRStrip kept a trailing separator: %q", got)
	}
}

// TestEmbeddedDreamTemplateIsStatic guards the template policy: the embedded
// copy must carry no Jinja2 construct other than the one substitution this
// package reimplements. A {% %} block or a second {{ }} would be silently
// shipped to the model as literal text.
func TestEmbeddedDreamTemplateIsStatic(t *testing.T) {
	if strings.Contains(dreamTemplate, "{%") || strings.Contains(dreamTemplate, "{#") {
		t.Error("the template contains a Jinja2 block or comment that is not rendered")
	}
	if n := strings.Count(dreamTemplate, "{{"); n != 1 {
		t.Errorf("template has %d substitutions, want 1", n)
	}
}

// TestDreamTemplateMatchesUpstreamFile checks the embedded copy is the upstream
// file byte for byte. compat.TestDreamMatchesPythonReference compares it against
// what the reference itself reads; this one works without the venv.
func TestDreamTemplateMatchesUpstreamFile(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller path")
	}
	upstream := filepath.Join(filepath.Dir(file), "..", "..", "upstream", "nanobot",
		"nanobot", "templates", "agent", "dream.md")
	raw, err := os.ReadFile(upstream)
	if err != nil {
		t.Skipf("SKIP: upstream template not present at %s", upstream)
	}
	if string(raw) != dreamTemplate {
		t.Errorf("embedded template differs from %s", upstream)
	}
}
