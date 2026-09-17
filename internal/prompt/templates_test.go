package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// repoRootForTest resolves the module root from this file's location
// (<root>/internal/prompt/templates_test.go).
func repoRootForTest(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller path")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// bundledTemplateNames is every file the embed directive carries. It is
// written out by hand rather than derived from the FS so that dropping a file
// from the directive is a test failure rather than a silently smaller set.
var bundledTemplateNames = []string{
	"AGENTS.md",
	"SOUL.md",
	"USER.md",
	"HEARTBEAT.md",
	"legacy/SOUL.md",
	"memory/MEMORY.md",
	"prompts/README.md",
}

// TestBundledTemplatesMatchReference is the anti-drift check: the embedded
// bytes must be identical to the frozen reference's templates/, because
// IsTemplateContent decides whether to WITHHOLD a user's file from the prompt
// by comparing against them. A one-byte drift here would silently start
// including (or excluding) content the reference includes (or excludes), and
// nothing else in the test suite would notice.
//
// It compares SHA-256 rather than only bytes so the failure message names the
// two digests, which is what makes "which side moved?" answerable.
func TestBundledTemplatesMatchReference(t *testing.T) {
	refDir := filepath.Join(repoRootForTest(t), "upstream", "nanobot", "nanobot", "templates")
	if _, err := os.Stat(refDir); err != nil {
		t.Skipf("SKIP: reference templates not present at %s — drift check not run", refDir)
	}

	for _, name := range bundledTemplateNames {
		embedded, ok := BundledTemplate(name)
		if !ok {
			t.Errorf("template %q is not embedded", name)
			continue
		}

		refPath := filepath.Join(refDir, filepath.FromSlash(name))
		raw, err := os.ReadFile(refPath)
		if err != nil {
			t.Errorf("read reference template %s: %v", refPath, err)
			continue
		}

		if embedded != string(raw) {
			gotSum := sha256.Sum256([]byte(embedded))
			wantSum := sha256.Sum256(raw)
			t.Errorf("template %q differs from the reference:\n  embedded sha256 = %s\n  reference sha256 = %s",
				name, hex.EncodeToString(gotSum[:]), hex.EncodeToString(wantSum[:]))
		}
	}
}

// TestBundledTemplateMissing pins the direction of the `tpl is None` case. The
// reference returns None for an unbundled path and _is_template_content then
// returns False, i.e. "not a template" — the content IS included. Inverting
// this would drop every user file whose name has no bundled counterpart.
func TestBundledTemplateMissing(t *testing.T) {
	if content, ok := BundledTemplate("nonexistent/path.md"); ok {
		t.Errorf("BundledTemplate(nonexistent) = (%q, true), want (\"\", false)", content)
	}
	if IsTemplateContent("anything", "nonexistent/path.md") {
		t.Error("IsTemplateContent with a missing template must be false")
	}
}

// TestIsTemplateContentMatchesReferenceSemantics covers the cases the
// differential harness also covers, but locally, so a failure is diagnosable
// without the Python venv.
func TestIsTemplateContentMatchesReferenceSemantics(t *testing.T) {
	agents, ok := BundledTemplate("AGENTS.md")
	if !ok {
		t.Fatal("AGENTS.md not embedded")
	}

	cases := []struct {
		name     string
		content  string
		template string
		want     bool
	}{
		{"identical", agents, "AGENTS.md", true},
		{"identical after strip", "\n\n\t " + agents + " \n\t\n", "AGENTS.md", true},
		{"leading whitespace only differs", "  " + agents, "AGENTS.md", true},
		{"trailing whitespace only differs", agents + "\n\n", "AGENTS.md", true},
		{"customised", "my own rules", "AGENTS.md", false},
		{"customised with padding", "\n  my own rules  \n", "AGENTS.md", false},
		{"empty vs non-empty template", "", "AGENTS.md", false},
		{"missing template", agents, "no/such.md", false},

		// The .strip()-vs-TrimSpace boundary. U+001C is whitespace to Python
		// (bidi class S) and NOT to Go's unicode.IsSpace. Both sides strip it,
		// so "\x1c" reduces to "" and cannot equal a non-empty template. The
		// behavioural half of this boundary — that a file holding only "\x1c"
		// is skipped as BLANK — is asserted in context_guards_test.go, because
		// strings.TrimSpace would not skip it.
		{"U+001C only vs non-empty template", "\x1c", "prompts/README.md", false},
		{"U+001C around the template", "\x1c" + agents + "\x1c", "AGENTS.md", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTemplateContent(tc.content, tc.template); got != tc.want {
				t.Errorf("IsTemplateContent(%q, %q) = %v, want %v", tc.content, tc.template, got, tc.want)
			}
		})
	}

	// U+001C is the whole reason textutil.PyStrip exists; assert the premise so
	// this test fails loudly if the helper is ever "simplified" to TrimSpace.
	if textutil.PyStrip("\x1c") != "" {
		t.Fatal("premise broken: PyStrip must strip U+001C")
	}
	if textutil.PyStrip("\x1c") == "" && textutil.PyStrip("\x1d\x1e\x1f") != "" {
		t.Fatal("premise broken: PyStrip must strip U+001D..U+001F")
	}
}
