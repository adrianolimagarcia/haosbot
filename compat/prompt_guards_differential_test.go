package compat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/prompt"
)

// This file is deliberately self-contained: its own dumper
// (compat/python/dump_prompt_guards.py) and its own loader. It shares no
// declarations with differential_test.go beyond repoRoot, because
// dump_reference.py and differential_test.go are edited concurrently by several
// agents and a read-modify-write race there would destroy work.
//
// It covers the bundled-template guards in the reference's ContextBuilder:
// _is_template_content decides whether a workspace file is still the SHIPPED
// default, and three inclusions are gated on it — the "# Memory" section, the
// legacy-SOUL substitution, and the AGENTS.md/USER.md skips. On a fresh install
// every workspace file IS the shipped default, so the reference withholds three
// sections that a naive port emits.
//
// Every expectation below comes from executing the frozen reference. Nothing is
// transcribed from documentation.

// --------------------------------------------------------------------------
// dumper document
// --------------------------------------------------------------------------

// pgProbe is one direct probe of ContextBuilder._is_template_content.
type pgProbe struct {
	Name     string `json:"name"`
	Content  string `json:"content"`
	Template string `json:"template"`
	Result   bool   `json:"result"`
}

// pgCase is one workspace driven through the real reference ContextBuilder.
type pgCase struct {
	Name             string          `json:"name"`
	Channel          string          `json:"channel"`
	IncludeMemory    bool            `json:"include_memory"`
	AgentWorkspace   string          `json:"agent_workspace"`
	ProjectWorkspace string          `json:"project_workspace"`
	Sections         map[string]bool `json:"sections"`
	BootstrapBlock   string          `json:"bootstrap_block"`
	BootstrapSHA256  string          `json:"bootstrap_sha256"`
	MemoryRaw        string          `json:"memory_raw"`
	PromptLen        int             `json:"prompt_len"`
}

// pgDoc is the dumper's whole output.
type pgDoc struct {
	UpstreamCommit    string             `json:"upstream_commit"`
	PythonVersion     string             `json:"python_version"`
	BaseDir           string             `json:"base_dir"`
	Templates         map[string]*string `json:"templates"`
	IsTemplateContent []pgProbe          `json:"is_template_content"`
	Cases             []pgCase           `json:"cases"`
}

// pgSectionMarkers is the set of markers compared in every case. "## Format
// Hint" is a CONTROL: it is not guarded by _is_template_content, so it must
// agree in every case. If a harness ever passed different arguments to the two
// sides, the control would catch it instead of the mismatch hiding as a
// "clean" pass.
var pgSectionMarkers = []string{
	"## AGENTS.md",
	"## USER.md",
	"## SOUL.md",
	"# Memory",
	"## Format Hint",
}

// loadPromptGuardsDump creates a PID-suffixed scratch directory under .tools/,
// runs the dumper against it, and returns the parsed document. The scratch
// directory is removed on cleanup. A PID suffix is required: parallel
// `go test ./compat/` runs have deleted each other's scratch directories
// before.
func loadPromptGuardsDump(t *testing.T) *pgDoc {
	t.Helper()
	root := repoRoot(t)
	python := filepath.Join(root, ".tools", "venv", "bin", "python")
	script := filepath.Join(root, "compat", "python", "dump_prompt_guards.py")

	if _, err := os.Stat(python); err != nil {
		t.Skipf("SKIP: reference venv not present at %s — differential check not run", python)
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("SKIP: dumper missing at %s", script)
	}

	base := filepath.Join(root, ".tools", fmt.Sprintf("prompt-guards-%d", os.Getpid()))
	if err := os.RemoveAll(base); err != nil {
		t.Fatalf("clean scratch dir %s: %v", base, err)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("create scratch dir %s: %v", base, err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Logf("cleanup of scratch dir %s failed: %v", base, err)
		}
	})

	cmd := exec.Command(python, script, base)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("prompt-guards dumper failed: %v\nstderr:\n%s", err, stderr)
	}

	var doc pgDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("dumper output is not valid JSON: %v", err)
	}
	return &doc
}

// --------------------------------------------------------------------------
// shared helpers
// --------------------------------------------------------------------------

// pgSections computes the section-presence map from a prompt, using the same
// substring test the dumper uses. Both sides must apply the identical rule or
// the comparison is meaningless.
func pgSections(promptText string) map[string]bool {
	got := make(map[string]bool, len(pgSectionMarkers))
	for _, marker := range pgSectionMarkers {
		got[marker] = strings.Contains(promptText, marker)
	}
	return got
}

func pgSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// pgLegacyMarkers reproduces the Go port's behaviour BEFORE the template
// guards were ported, so the test can show — and assert — that the divergence
// really existed and that the fix is load-bearing.
//
// This is the pre-fix code, transcribed from internal/prompt/context.go at the
// revision that had no _is_template_content: every non-blank bootstrap file is
// included, every non-blank memory file is included, and "blank" is
// strings.TrimSpace. It is deliberately written with strings.TrimSpace so that
// the whitespace trap shows up in the table.
//
// Only the four guarded markers are reported. "## Format Hint" is NOT part of
// the pre-fix logic at all — it is driven by the channel argument — so folding
// it in here would fake a comparison the old code never made.
func pgLegacyMarkers(c pgCase) map[string]bool {
	var parts []string
	for _, s := range []struct{ name, root string }{
		{"AGENTS.md", c.ProjectWorkspace},
		{"SOUL.md", c.AgentWorkspace},
		{"USER.md", c.AgentWorkspace},
	} {
		data, err := os.ReadFile(filepath.Join(s.root, s.name))
		if err != nil {
			continue
		}
		content := string(data)
		if strings.TrimSpace(content) == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("## %s\n\n%s", s.name, content))
	}
	bootstrap := strings.Join(parts, "\n\n")

	got := map[string]bool{
		"## AGENTS.md": strings.Contains(bootstrap, "## AGENTS.md"),
		"## SOUL.md":   strings.Contains(bootstrap, "## SOUL.md"),
		"## USER.md":   strings.Contains(bootstrap, "## USER.md"),
		"# Memory":     false,
	}
	if c.IncludeMemory {
		if data, err := os.ReadFile(filepath.Join(c.AgentWorkspace, "memory", "MEMORY.md")); err == nil {
			if memory := string(data); strings.TrimSpace(memory) != "" {
				got["# Memory"] = true
			}
		}
	}
	return got
}

// pgGuardedMarkers is the subset of markers the pre-fix code could get wrong.
var pgGuardedMarkers = []string{"## AGENTS.md", "## USER.md", "## SOUL.md", "# Memory"}

// pgGoSections drives the port under test with EXACTLY the arguments the
// dumper passed to the reference.
func pgGoSections(t *testing.T, c pgCase) (sections map[string]bool, bootstrap, memory, full string) {
	t.Helper()
	builder := prompt.New(c.AgentWorkspace)
	full = builder.BuildSystemPrompt(c.Channel, nil, c.ProjectWorkspace, c.IncludeMemory)
	return pgSections(full), builder.LoadBootstrapFiles(c.ProjectWorkspace), builder.ReadMemory(), full
}

func pgSortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --------------------------------------------------------------------------
// tests
// --------------------------------------------------------------------------

// TestPromptGuardTemplatesMatchReference proves the Go embed is byte-identical
// to the templates the reference compares against. Every guard decision rests
// on these bytes, so a drift here silently changes which sections are withheld.
func TestPromptGuardTemplatesMatchReference(t *testing.T) {
	doc := loadPromptGuardsDump(t)

	checked := 0
	for name, want := range doc.Templates {
		if want == nil {
			t.Errorf("reference reports template %q as unbundled", name)
			continue
		}
		content, ok := prompt.BundledTemplate(name)
		if !ok {
			t.Errorf("template %q is not embedded in the Go port", name)
			continue
		}
		if got := pgSHA256(content); got != *want {
			t.Errorf("template %q digest mismatch:\n  go        = %s\n  reference = %s", name, got, *want)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("compared 0 templates — the harness is broken, not passing")
	}
	t.Logf("compared %d bundled templates against the reference digests", checked)
}

// TestIsTemplateContentMatchesReference probes the ported predicate directly.
// These cases isolate the two details that are easy to get wrong: `.strip()` on
// BOTH sides (U+001C..U+001F, NBSP) and `tpl is None -> False`.
func TestIsTemplateContentMatchesReference(t *testing.T) {
	doc := loadPromptGuardsDump(t)
	if len(doc.IsTemplateContent) == 0 {
		t.Fatal("dumper returned 0 is_template_content probes — the harness is broken, not passing")
	}

	for _, p := range doc.IsTemplateContent {
		got := prompt.IsTemplateContent(p.Content, p.Template)
		if got != p.Result {
			t.Errorf("IsTemplateContent(%q, %q) = %v, reference = %v",
				p.Content, p.Template, got, p.Result)
		}
	}

	// A predicate that always returned the same answer would "pass" a matrix
	// of identical expectations, so require both outcomes to be represented.
	var trues, falses int
	for _, p := range doc.IsTemplateContent {
		if p.Result {
			trues++
		} else {
			falses++
		}
	}
	if trues == 0 || falses == 0 {
		t.Fatalf("degenerate probe matrix: %d true, %d false", trues, falses)
	}
	t.Logf("compared %d is_template_content probes (%d true, %d false)", len(doc.IsTemplateContent), trues, falses)
}

// TestPromptGuardsMatchReference is the decisive end-to-end differential. For
// every case it drives the real reference ContextBuilder (via the dumper) and
// the Go Builder against the SAME workspace, with the SAME channel,
// include_memory and project workspace, and compares:
//
//   - presence/absence of each guarded section in the FULL prompt;
//   - the byte-exact bootstrap block, which is pure string concatenation with
//     no Jinja2 involvement, so unlike the full prompt it CAN be compared
//     byte for byte;
//   - the raw memory file contents.
func TestPromptGuardsMatchReference(t *testing.T) {
	doc := loadPromptGuardsDump(t)
	if len(doc.Cases) == 0 {
		t.Fatal("dumper returned 0 cases — the harness is broken, not passing")
	}

	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			gotSections, gotBootstrap, gotMemory, full := pgGoSections(t, c)

			for _, marker := range pgSortedKeys(c.Sections) {
				if gotSections[marker] != c.Sections[marker] {
					t.Errorf("section %q: go = %v, reference = %v", marker, gotSections[marker], c.Sections[marker])
				}
			}

			if gotBootstrap != c.BootstrapBlock {
				t.Errorf("bootstrap block differs (go %d bytes, reference %d bytes):\n--- go ---\n%q\n--- reference ---\n%q",
					len(gotBootstrap), len(c.BootstrapBlock), gotBootstrap, c.BootstrapBlock)
			} else if pgSHA256(gotBootstrap) != c.BootstrapSHA256 {
				t.Errorf("bootstrap block matches but its digest does not; dumper is inconsistent")
			}

			if gotMemory != c.MemoryRaw {
				t.Errorf("raw memory differs:\n  go        = %q\n  reference = %q", gotMemory, c.MemoryRaw)
			}

			t.Logf("prompt: go %d bytes, reference %d bytes (identity line differs by design: Go reports the Go toolchain)",
				len(full), c.PromptLen)
		})
	}
	t.Logf("compared %d workspace cases against the reference ContextBuilder", len(doc.Cases))
}

// TestFreshInstallGuardTable prints the before/after table for a fresh install
// and asserts that the fix is load-bearing: the pre-fix port must disagree with
// the reference on the guarded sections, and the port under test must agree.
//
// The "before" column is computed by pgLegacySections, which reproduces the
// pre-fix Go code verbatim. Without this assertion the differential test would
// still pass if someone reverted the guards, because the harness would simply
// be comparing a reverted port against itself.
func TestFreshInstallGuardTable(t *testing.T) {
	doc := loadPromptGuardsDump(t)

	var fresh *pgCase
	for i := range doc.Cases {
		if doc.Cases[i].Name == "fresh_install" {
			fresh = &doc.Cases[i]
		}
	}
	if fresh == nil {
		t.Fatal("dumper produced no fresh_install case")
	}

	after, _, _, _ := pgGoSections(t, *fresh)
	before := pgLegacyMarkers(*fresh)

	t.Logf("fresh install (workspace created by the reference's own sync_workspace_templates)")
	t.Logf("%-18s %-12s %-12s %-12s", "section", "reference", "go BEFORE", "go AFTER")
	for _, marker := range pgSectionMarkers {
		// The pre-fix shim models only the four guarded markers; "## Format
		// Hint" was never part of the old guard logic, so reporting a value for
		// it would invent a divergence that did not exist.
		beforeCell := "n/a (not guarded)"
		if v, modelled := before[marker]; modelled {
			beforeCell = pgPresence(v)
		}
		t.Logf("%-18s %-12s %-12s %-12s",
			marker, pgPresence(fresh.Sections[marker]), beforeCell, pgPresence(after[marker]))
	}
	t.Logf("reference prompt length: %d bytes", fresh.PromptLen)

	// The port under test must match the reference exactly, on every marker.
	for _, marker := range pgSectionMarkers {
		if after[marker] != fresh.Sections[marker] {
			t.Errorf("AFTER fix, section %q: go = %v, reference = %v", marker, after[marker], fresh.Sections[marker])
		}
	}

	// The pre-fix port must NOT match: that is the divergence this change fixes.
	diverged := 0
	for _, marker := range []string{"## AGENTS.md", "## USER.md", "# Memory"} {
		if before[marker] != fresh.Sections[marker] {
			diverged++
		}
	}
	if diverged != 3 {
		t.Fatalf("pre-fix port diverges from the reference on %d of 3 guarded sections, expected 3 — "+
			"the harness is no longer reproducing the reported bug", diverged)
	}
}

// TestPreFixPortDivergenceCount reports how many cases the pre-fix behaviour
// got wrong. It is informational for the report, but the non-zero assertion
// makes it a real check: a harness that silently stopped exercising the guards
// would otherwise look like a pass.
func TestPreFixPortDivergenceCount(t *testing.T) {
	doc := loadPromptGuardsDump(t)

	var divergent, agree []string
	for _, c := range doc.Cases {
		before := pgLegacyMarkers(c)
		after, _, _, _ := pgGoSections(t, c)

		bad := false
		for _, marker := range pgGuardedMarkers {
			if after[marker] != c.Sections[marker] {
				t.Errorf("case %s: section %q after fix: go = %v, reference = %v",
					c.Name, marker, after[marker], c.Sections[marker])
			}
			if before[marker] != c.Sections[marker] {
				bad = true
			}
		}
		// The control marker must agree for the port under test in every case;
		// if it did not, the harness would be passing different arguments to
		// the two sides and every other comparison here would be suspect.
		if after["## Format Hint"] != c.Sections["## Format Hint"] {
			t.Errorf("case %s: control marker %q: go = %v, reference = %v",
				c.Name, "## Format Hint", after["## Format Hint"], c.Sections["## Format Hint"])
		}
		if bad {
			divergent = append(divergent, c.Name)
		} else {
			agree = append(agree, c.Name)
		}
	}

	if len(divergent) == 0 {
		t.Fatal("the pre-fix behaviour matched the reference in every case — the harness is not exercising the guards")
	}
	t.Logf("pre-fix behaviour diverged from the reference in %d/%d cases: %v",
		len(divergent), len(doc.Cases), divergent)
	t.Logf("pre-fix behaviour already agreed in %d/%d cases: %v", len(agree), len(doc.Cases), agree)
}

// TestSOULSubstitutionIsDiscriminating checks the one guard the section
// booleans cannot see on their own: the legacy-SOUL substitution. The current
// SOUL.md template is a PREFIX of the legacy one, so "the current SOUL text is
// in the prompt" is true with or without the substitution — the discriminating
// question is whether the legacy-only tail is gone.
func TestSOULSubstitutionIsDiscriminating(t *testing.T) {
	doc := loadPromptGuardsDump(t)

	current, ok := prompt.BundledTemplate("SOUL.md")
	if !ok {
		t.Fatal("SOUL.md is not embedded")
	}
	legacy, ok := prompt.BundledTemplate("legacy/SOUL.md")
	if !ok {
		t.Fatal("legacy/SOUL.md is not embedded")
	}
	if !strings.Contains(legacy, current) {
		t.Skip("premise changed: the current SOUL template is no longer a prefix of the legacy one")
	}

	checked := 0
	for _, c := range doc.Cases {
		// Read the workspace SOUL.md and let the ported predicate say whether
		// this case exercises the substitution. Using the predicate rather than
		// a hardcoded case list keeps the check honest if the case list changes.
		raw, err := os.ReadFile(filepath.Join(c.AgentWorkspace, "SOUL.md"))
		if err != nil {
			continue
		}
		if !prompt.IsTemplateContent(string(raw), "legacy/SOUL.md") {
			continue
		}
		checked++

		_, _, _, full := pgGoSections(t, c)
		if !strings.Contains(full, "## SOUL.md\n\n"+current) {
			t.Errorf("case %s: the prompt does not carry the CURRENT SOUL template", c.Name)
		}
		if strings.Contains(full, "## Execution Rules") {
			t.Errorf("case %s: the prompt kept the legacy-only SOUL section that the reference drops", c.Name)
		}
		// And the reference agrees that the legacy tail is gone.
		if strings.Contains(c.BootstrapBlock, "## Execution Rules") {
			t.Errorf("case %s: the reference kept the legacy SOUL section — the dumper is wrong", c.Name)
		}
	}
	if checked == 0 {
		t.Fatal("no case exercised the SOUL substitution — the harness is broken, not passing")
	}
	t.Logf("verified the SOUL substitution in %d cases", checked)
}

func pgPresence(present bool) string {
	if present {
		return "present"
	}
	return "absent"
}
