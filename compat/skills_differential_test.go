package compat

// Differential harness for the SkillsLoader subsystem.
//
// This file is deliberately self-contained: its own dumper
// (compat/python/dump_skills.py), its own document types and its own loader. It
// shares no declarations with differential_test.go beyond repoRoot, because
// dump_reference.py and differential_test.go are edited concurrently by several
// agents and a read-modify-write race there would destroy work.
//
// WHAT IT PROVES
//
// Before this change the port had NO SkillsLoader. The only stand-in was a
// ~25-line scan of <workspace>/skills that printed bare relative paths and
// emitted NOTHING for the built-in skills — which are the only skills a fresh
// install has. The reference's build_system_prompt therefore produced an ~1.9 KB
// "# Skills" section that the Go prompt did not contain at all.
//
// Every expectation here comes from executing the frozen reference. Nothing is
// transcribed from documentation.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/prompt"
	"github.com/adrianolimagarcia/nanobot-go/internal/pyjson"
	"github.com/adrianolimagarcia/nanobot-go/internal/skills"
)

// --------------------------------------------------------------------------
// dumper document
// --------------------------------------------------------------------------

type skFrontmatter struct {
	Name           string  `json:"name"`
	Content        string  `json:"content"`
	Strip          string  `json:"strip"`
	StripError     *string `json:"strip_error"`
	Metadata       *string `json:"metadata"`
	MetadataIsNone bool    `json:"metadata_is_none"`
	Error          *string `json:"error"`
}

type skValidProbe struct {
	Document  string `json:"document"`
	SkillName string `json:"skill_name"`
	Result    *bool  `json:"result"`
}

type skNanobotMetadata struct {
	Document string `json:"document"`
	RawCanon string `json:"raw_canon"`
	Result   string `json:"result"`
}

type skRequirements struct {
	Bins        []string `json:"bins"`
	Env         []string `json:"env"`
	MissingBins []string `json:"missing_bins"`
	MissingEnv  []string `json:"missing_env"`
}

type skSkillReport struct {
	Description       *string         `json:"description"`
	DescriptionError  *string         `json:"description_error"`
	Available         *bool           `json:"available"`
	Missing           *string         `json:"missing"`
	Requirements      *skRequirements `json:"requirements"`
	LoadSHA256        *string         `json:"load_sha256"`
	LoadLen           *int            `json:"load_len"`
	LoadError         *string         `json:"load_error"`
	MetadataCanon     *string         `json:"metadata_canon"`
	MetadataError     *string         `json:"metadata_error"`
	RequirementsError *string         `json:"requirements_error"`
}

type skExplicit struct {
	Invoked []string `json:"invoked"`
	Error   *string  `json:"error"`
}

type skExplicitContext struct {
	Source  *string `json:"source"`
	Content *string `json:"content"`
	Error   *string `json:"error"`
}

type skCase struct {
	Name               string                       `json:"name"`
	Channel            string                       `json:"channel"`
	IncludeMemory      bool                         `json:"include_memory"`
	AgentWorkspace     string                       `json:"agent_workspace"`
	ProjectWorkspace   string                       `json:"project_workspace"`
	BuiltinSkillsDir   string                       `json:"builtin_skills_dir"`
	DisabledSkills     []string                     `json:"disabled_skills"`
	SymlinkedWorkspace bool                         `json:"symlinked_workspace"`
	ListAll            [][]string                   `json:"list_all"`
	ListAllError       *string                      `json:"list_all_error"`
	ListAvailable      [][]string                   `json:"list_available"`
	ListAvailableError *string                      `json:"list_available_error"`
	Summary            *string                      `json:"summary"`
	SummaryError       *string                      `json:"summary_error"`
	Always             []string                     `json:"always"`
	AlwaysError        *string                      `json:"always_error"`
	ActiveContent      *string                      `json:"active_content"`
	ActiveError        *string                      `json:"active_error"`
	Skills             map[string]skSkillReport     `json:"skills"`
	Explicit           map[string]skExplicit        `json:"explicit"`
	ExplicitContext    map[string]skExplicitContext `json:"explicit_context"`
	PromptLen          *int                         `json:"prompt_len"`
	Prompt             *string                      `json:"prompt"`
	PromptError        *string                      `json:"prompt_error"`
}

type skDoc struct {
	UpstreamCommit   string              `json:"upstream_commit"`
	PythonVersion    string              `json:"python_version"`
	BaseDir          string              `json:"base_dir"`
	Home             string              `json:"home"`
	BuiltinSkillsDir string              `json:"builtin_skills_dir"`
	BundledFiles     map[string]string   `json:"bundled_files"`
	BundledOrder     []string            `json:"bundled_order"`
	ProbeEnv         map[string]string   `json:"probe_env"`
	ProbeBins        map[string]bool     `json:"probe_bins"`
	Frontmatter      []skFrontmatter     `json:"frontmatter"`
	ValidMetadata    []skValidProbe      `json:"valid_metadata"`
	NanobotMetadata  []skNanobotMetadata `json:"nanobot_metadata"`
	ExplicitTexts    []string            `json:"explicit_texts"`
	Cases            []skCase            `json:"cases"`
}

// The dumper builds 21 workspaces and drives the real ContextBuilder for each,
// which costs ~25 s. It is therefore run ONCE per test binary and memoised;
// TestMain removes the scratch directory afterwards.
var (
	skDumpOnce sync.Once
	skDumpDoc  *skDoc
	skDumpBase string
	skDumpErr  error
)

// TestMain exists only to clean up the shared dumper scratch directory. It
// touches nothing else, and m.Run() is the only other statement, so it cannot
// change what any test observes.
func TestMain(m *testing.M) {
	code := m.Run()
	if skDumpBase != "" {
		if err := os.RemoveAll(skDumpBase); err != nil {
			fmt.Fprintf(os.Stderr, "cleanup of skills scratch dir %s failed: %v\n", skDumpBase, err)
		}
	}
	os.Exit(code)
}

// errSkSkip marks a condition that should SKIP rather than fail.
type errSkSkip struct{ reason string }

func (e errSkSkip) Error() string { return e.reason }

// skRepoRoot is repoRoot without the *testing.T dependency, so it can be used
// from the memoised loader.
func skRepoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot resolve caller path")
	}
	return filepath.Dir(filepath.Dir(file)), nil
}

// runSkillsDump performs the one-time dump.
//
// A PID suffix is required: parallel `go test ./compat/` runs have deleted each
// other's scratch directories before.
func runSkillsDump() (*skDoc, string, error) {
	root, err := skRepoRoot()
	if err != nil {
		return nil, "", err
	}
	python := filepath.Join(root, ".tools", "venv", "bin", "python")
	script := filepath.Join(root, "compat", "python", "dump_skills.py")

	if _, err := os.Stat(python); err != nil {
		return nil, "", errSkSkip{fmt.Sprintf("reference venv not present at %s — differential check not run", python)}
	}
	if _, err := os.Stat(script); err != nil {
		return nil, "", errSkSkip{fmt.Sprintf("dumper missing at %s", script)}
	}

	base := filepath.Join(root, ".tools", fmt.Sprintf("skills-diff-%d", os.Getpid()))
	if err := os.RemoveAll(base); err != nil {
		return nil, "", fmt.Errorf("clean scratch dir %s: %w", base, err)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, "", fmt.Errorf("create scratch dir %s: %w", base, err)
	}
	skDumpBase = base

	cmd := exec.Command(python, script, base)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, base, fmt.Errorf("skills dumper failed: %w\nstderr:\n%s", err, stderr.String())
	}

	var doc skDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, base, fmt.Errorf("dumper output is not valid JSON: %w", err)
	}
	if doc.Home != filepath.Join(base, "home") {
		return nil, base, fmt.Errorf("dumper HOME is %q, expected %q — the fixture is not reproducible", doc.Home, filepath.Join(base, "home"))
	}
	return &doc, base, nil
}

// loadSkillsDump returns the memoised dumper document.
//
// HOME and the probe environment are re-applied for EVERY test, because
// t.Setenv restores them when the test ends. HOME matters: the Agent Plugin
// activation marker and the CLI Apps alias registry live under $HOME/.nanobot,
// and the reference's plugin data directory is keyed by a hash of the workspace
// path — without a shared HOME the plugin fixture would not be reproducible.
func loadSkillsDump(t *testing.T) (*skDoc, string) {
	t.Helper()
	skDumpOnce.Do(func() {
		skDumpDoc, skDumpBase, skDumpErr = runSkillsDump()
	})
	if skDumpErr != nil {
		var skip errSkSkip
		if errors.As(skDumpErr, &skip) {
			t.Skipf("SKIP: %s", skip.reason)
		}
		t.Fatal(skDumpErr)
	}
	t.Setenv("HOME", filepath.Join(skDumpBase, "home"))
	for name, value := range skDumpDoc.ProbeEnv {
		t.Setenv(name, value)
	}
	return skDumpDoc, skDumpBase
}

// --------------------------------------------------------------------------
// shared helpers
// --------------------------------------------------------------------------

func skSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func skJSONQuote(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		t.Fatalf("encode string: %v", err)
	}
	return strings.TrimRight(buf.String(), "\n")
}

// skCanon reproduces the dumper's _canon exactly.
//
// It cannot use encoding/json directly: Python distinguishes int from float and
// the port's YAML subset parser produces int64 and float64 separately, so `1`
// and `1.0` would collapse into the same JSON text.
func skCanon(t *testing.T, value any) string {
	t.Helper()
	switch typed := value.(type) {
	case nil:
		return "null"
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case string:
		return skJSONQuote(t, typed)
	case int64:
		return fmt.Sprintf("%d", typed)
	case int:
		return fmt.Sprintf("%d", typed)
	case float64:
		return pyjson.FormatFloat(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, skCanon(t, item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, skJSONQuote(t, key)+": "+skCanon(t, typed[key]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprintf("<%T>", value)
}

// skStringsEqual compares two string slices treating nil and empty as equal,
// which is what a Python `[]` deserialises to on both sides.
func skStringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func skRows(entries []skills.Skill) [][]string {
	rows := make([][]string, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, []string{entry.Name, entry.Source, entry.Path})
	}
	return rows
}

func skRowsEqual(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

func skFormatRows(rows [][]string) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		parts = append(parts, strings.Join(row, " | "))
	}
	return strings.Join(parts, "\n")
}

// skNormalizeRuntime replaces the runtime line so the FULL prompts can be
// compared byte for byte. The reference reports "<OS> <machine>, Python
// <version>" and the port reports the Go toolchain instead — an intentional,
// documented divergence in internal/prompt/context.go. Everything else must
// match exactly.
func skNormalizeRuntime(promptText string) string {
	const marker = "## Runtime\n"
	index := strings.Index(promptText, marker)
	if index < 0 {
		return promptText
	}
	start := index + len(marker)
	end := strings.IndexByte(promptText[start:], '\n')
	if end < 0 {
		return promptText
	}
	return promptText[:start] + "<runtime>" + promptText[start+end:]
}

// skLoader builds the port's loader with EXACTLY the arguments the dumper gave
// the reference: the same agent workspace, the same explicit built-in directory
// and the same disabled set.
func skLoader(c skCase) *skills.Loader {
	loader := skills.NewWithBuiltinDir(c.AgentWorkspace, c.BuiltinSkillsDir)
	if len(c.DisabledSkills) > 0 {
		disabled := make(map[string]bool, len(c.DisabledSkills))
		for _, name := range c.DisabledSkills {
			disabled[name] = true
		}
		loader.DisabledSkills = disabled
	}
	return loader
}

// skGoList renders the port's list_skills the same way the dumper renders the
// reference's: [name, source, path].
func skGoList(loader *skills.Loader, filterUnavailable bool) [][]string {
	return skRows(loader.ListSkills(filterUnavailable))
}

// skSkillNames is the set of names the dumper reported for a case, sorted.
func skSkillNames(c skCase) []string {
	names := make([]string, 0, len(c.Skills))
	for name := range c.Skills {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// --------------------------------------------------------------------------
// bundled assets
// --------------------------------------------------------------------------

// TestSkillsBundledAssetsMatchReference proves the embedded copy of
// nanobot/skills/ is byte-identical to the frozen checkout. The built-in group's
// descriptions and its `(unavailable: ...)` suffixes are read straight out of
// these files, so drift here silently changes what the model is told.
func TestSkillsBundledAssetsMatchReference(t *testing.T) {
	doc, _ := loadSkillsDump(t)
	if len(doc.BundledFiles) == 0 {
		t.Fatal("dumper reported 0 bundled skill files — the harness is broken, not passing")
	}

	embedded := map[string]bool{}
	for _, rel := range skills.BundledSkillFiles() {
		embedded[rel] = true
	}

	checked := 0
	for rel, want := range doc.BundledFiles {
		content, ok := skills.BundledSkillFile(rel)
		if !ok {
			t.Errorf("bundled skill file %q is not embedded in the Go port", rel)
			continue
		}
		if got := skSHA256(content); got != want {
			t.Errorf("bundled skill file %q digest mismatch:\n  go        = %s\n  reference = %s", rel, got, want)
		}
		if !embedded[rel] {
			t.Errorf("bundled skill file %q is not listed by BundledSkillFiles", rel)
		}
		checked++
	}
	for rel := range embedded {
		if _, ok := doc.BundledFiles[rel]; !ok {
			t.Errorf("the port embeds %q but the reference tree does not contain it", rel)
		}
	}
	if checked == 0 {
		t.Fatal("compared 0 bundled skill files — the harness is broken, not passing")
	}
	t.Logf("compared %d bundled skill files against the reference digests", checked)
}

// TestSkillsBundledOrderMatchesReference pins the two things about the embedded
// built-in listing that are actually true on every host: the port ships the same
// SET of built-in skills as the reference, and the port's order is the
// lexicographic one go:embed guarantees.
//
// Byte-for-byte ORDER equality with the reference is NOT achievable, and
// asserting it made this test fail on every CI run while passing on a developer
// machine:
//
//   - go:embed (and fs.ReadDir over an embed.FS) is specified to return entries
//     SORTED BY FILENAME. An embedded tree has no directory order to preserve,
//     so BundledSkillNames() is lexicographic on every host, always.
//   - The reference lists its built-in skills with `base.iterdir()`
//     (agent/skills.py:78) — raw readdir order, which is a property of the
//     HOST, not of the reference. On this machine it happens to come back
//     lexicographic ([README.md clawhub cron github ...]); the CI runner
//     returned [github weather clawhub my ...]. Neither is "the" order.
//
// The reference's order is therefore not a stable target, and the port cannot
// reproduce it even in principle. What must hold — and what a real regression
// breaks — is that the same skills are shipped (a missing or extra built-in
// skill changes what the model is told it can do) and that the port's order is
// deterministic.
func TestSkillsBundledOrderMatchesReference(t *testing.T) {
	doc, _ := loadSkillsDump(t)
	if len(doc.BundledOrder) == 0 {
		t.Fatal("dumper reported 0 built-in entries — the harness is broken, not passing")
	}

	// The reference's listing, filtered to the entries that are skills: a
	// directory holding a SKILL.md. README.md is not one.
	refDirs := make([]string, 0, len(doc.BundledOrder))
	for _, name := range doc.BundledOrder {
		if info, err := os.Stat(filepath.Join(doc.BuiltinSkillsDir, name)); err == nil && info.IsDir() {
			if _, err := os.Stat(filepath.Join(doc.BuiltinSkillsDir, name, "SKILL.md")); err == nil {
				refDirs = append(refDirs, name)
			}
		}
	}
	if len(refDirs) == 0 {
		t.Fatal("the reference's listing contains no skill directories — the harness is broken, not passing")
	}

	got := skills.BundledSkillNames()
	if len(got) == 0 {
		t.Fatal("BundledSkillNames() returned nothing — the harness is broken, not passing")
	}

	// SET equality, which is the meaningful invariant.
	gotSorted := append([]string(nil), got...)
	sort.Strings(gotSorted)
	wantSorted := append([]string(nil), refDirs...)
	sort.Strings(wantSorted)
	if !skStringsEqual(gotSorted, wantSorted) {
		t.Errorf("the embedded built-in tree does not ship the same skills as the reference:\n  go        = %v\n  reference = %v",
			gotSorted, wantSorted)
	}

	// ORDER: the port's order is lexicographic, and that is asserted directly
	// rather than compared against the reference, because the reference's order
	// is raw readdir order.
	if !sort.StringsAreSorted(got) {
		t.Errorf("BundledSkillNames() is not in the lexicographic order fs.ReadDir guarantees:\n  go = %v", got)
	}

	if sort.StringsAreSorted(refDirs) {
		// Say out loud when the run cannot distinguish the two orders: on such a
		// host the old order-equality assertion passed for the wrong reason.
		t.Logf("note: this host's readdir order for the reference tree happens to be lexicographic (%v), so this run cannot tell the two orders apart; the set assertion above is what holds everywhere", refDirs)
	} else {
		t.Logf("note: this host's readdir order for the reference tree is NOT lexicographic (%v) — the condition that used to fail here", refDirs)
	}
	t.Logf("compared %d built-in skill names as a set; go:embed order = %v", len(got), got)
}

// --------------------------------------------------------------------------
// frontmatter
// --------------------------------------------------------------------------

// TestSkillsFrontmatterMatchesReference drives _strip_frontmatter and
// parse_skill_metadata over an adversarial corpus: malformed YAML, tabs,
// duplicate keys, block scalars, flow collections, CRLF, CR-only, BOM, Python
// whitespace padding around the fences, and every scalar type PyYAML resolves.
func TestSkillsFrontmatterMatchesReference(t *testing.T) {
	doc, _ := loadSkillsDump(t)
	if len(doc.Frontmatter) == 0 {
		t.Fatal("dumper returned 0 frontmatter documents — the harness is broken, not passing")
	}

	noneCount, dictCount := 0, 0
	for _, tc := range doc.Frontmatter {
		t.Run(tc.Name, func(t *testing.T) {
			if got := skills.StripFrontmatter(tc.Content); got != tc.Strip {
				t.Errorf("StripFrontmatter differs\n  go        = %q\n  reference = %q", got, tc.Strip)
			}

			meta := skills.ParseSkillMetadata(tc.Content)
			if tc.MetadataIsNone {
				if meta != nil {
					t.Errorf("reference returned None, go returned %s", skCanon(t, meta))
				}
				return
			}
			if meta == nil {
				t.Fatalf("reference returned %s, go returned None", *tc.Metadata)
			}
			if got := skCanon(t, meta); got != *tc.Metadata {
				t.Errorf("parse_skill_metadata differs\n  go        = %s\n  reference = %s", got, *tc.Metadata)
			}
		})
		if tc.MetadataIsNone {
			noneCount++
		} else {
			dictCount++
		}
	}
	if noneCount == 0 || dictCount == 0 {
		t.Fatalf("degenerate frontmatter corpus: %d None, %d mapping", noneCount, dictCount)
	}
	t.Logf("compared %d frontmatter documents (%d None, %d mapping)", len(doc.Frontmatter), noneCount, dictCount)
}

// TestSkillsValidMetadataMatchesReference covers valid_skill_metadata's Agent
// Skills identity contract, including the CODE POINT (not byte) length bounds.
func TestSkillsValidMetadataMatchesReference(t *testing.T) {
	doc, _ := loadSkillsDump(t)
	if len(doc.ValidMetadata) == 0 {
		t.Fatal("dumper returned 0 valid_metadata probes — the harness is broken, not passing")
	}

	byName := map[string]skFrontmatter{}
	for _, entry := range doc.Frontmatter {
		byName[entry.Name] = entry
	}

	trues, falses := 0, 0
	for _, probe := range doc.ValidMetadata {
		source, ok := byName[probe.Document]
		if !ok {
			t.Fatalf("probe references unknown document %q", probe.Document)
		}
		meta := skills.ParseSkillMetadata(source.Content)
		var got *bool
		if meta != nil {
			result := skills.ValidSkillMetadata(meta, probe.SkillName)
			got = &result
		}
		if probe.Result == nil {
			if got != nil {
				t.Errorf("%s/%s: reference raised or returned None, go returned %v", probe.Document, probe.SkillName, *got)
			}
			continue
		}
		if got == nil {
			t.Errorf("%s/%s: reference returned %v, go returned None", probe.Document, probe.SkillName, *probe.Result)
			continue
		}
		if *got != *probe.Result {
			t.Errorf("%s/%s: go = %v, reference = %v", probe.Document, probe.SkillName, *got, *probe.Result)
		}
		if *probe.Result {
			trues++
		} else {
			falses++
		}
	}
	if trues == 0 || falses == 0 {
		t.Fatalf("degenerate valid_metadata matrix: %d true, %d false", trues, falses)
	}
	t.Logf("compared %d valid_skill_metadata probes (%d true, %d false)", len(doc.ValidMetadata), trues, falses)
}

// TestSkillsNanobotMetadataMatchesReference covers _parse_nanobot_metadata: the
// dict/JSON-string inputs, the nanobot-over-openclaw precedence, and every shape
// that must yield {}.
func TestSkillsNanobotMetadataMatchesReference(t *testing.T) {
	doc, _ := loadSkillsDump(t)
	if len(doc.NanobotMetadata) == 0 {
		t.Fatal("dumper returned 0 nanobot_metadata probes — the harness is broken, not passing")
	}

	byName := map[string]skFrontmatter{}
	for _, entry := range doc.Frontmatter {
		byName[entry.Name] = entry
	}

	empty, nonEmpty := 0, 0
	for _, probe := range doc.NanobotMetadata {
		source := byName[probe.Document]
		meta := skills.ParseSkillMetadata(source.Content)
		if meta == nil {
			t.Fatalf("%s: the port could not parse a document the reference parsed", probe.Document)
		}
		got := skCanon(t, skills.ParseNanobotMetadata(meta["metadata"]))
		if got != probe.Result {
			t.Errorf("%s: _parse_nanobot_metadata differs\n  go        = %s\n  reference = %s", probe.Document, got, probe.Result)
		}
		if probe.Result == "{}" {
			empty++
		} else {
			nonEmpty++
		}
	}
	if empty == 0 || nonEmpty == 0 {
		t.Fatalf("degenerate nanobot_metadata matrix: %d empty, %d non-empty", empty, nonEmpty)
	}
	t.Logf("compared %d _parse_nanobot_metadata probes (%d empty, %d non-empty)", len(doc.NanobotMetadata), empty, nonEmpty)
}

// --------------------------------------------------------------------------
// cases
// --------------------------------------------------------------------------

// TestSkillsCasesMatchReference is the decisive end-to-end differential. For
// every workspace it drives the real reference SkillsLoader and ContextBuilder
// (through the dumper) and the port against the SAME workspace, with the SAME
// built-in directory, disabled set, channel and project workspace, and compares:
//
//   - list_skills(filter_unavailable=True/False): names, sources, paths AND order
//   - build_skills_summary, byte for byte
//   - get_always_skills and load_skills_for_context
//   - per skill: description, availability, missing requirements, requirements,
//     load_skill digest, parsed frontmatter
//   - get_explicitly_invoked_skills and build_explicit_skill_runtime_context
//   - the FULL rendered system prompt, byte for byte (runtime line normalised)
func TestSkillsCasesMatchReference(t *testing.T) {
	doc, _ := loadSkillsDump(t)
	if len(doc.Cases) == 0 {
		t.Fatal("dumper returned 0 cases — the harness is broken, not passing")
	}

	compared, skipped := 0, 0
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if c.SummaryError != nil || c.PromptError != nil {
				// The reference RAISED for this fixture. Those cases are
				// asserted explicitly in TestSkillsReferenceCrashCases.
				skipped++
				t.Skipf("reference raised for this fixture (summary=%v, prompt=%v)", c.SummaryError, c.PromptError)
			}
			compared++
			loader := skLoader(c)

			if got := skGoList(loader, false); !skRowsEqual(got, c.ListAll) {
				t.Errorf("list_skills(filter_unavailable=False) differs\n  go        =\n%s\n  reference =\n%s",
					skFormatRows(got), skFormatRows(c.ListAll))
			}
			if got := skGoList(loader, true); !skRowsEqual(got, c.ListAvailable) {
				t.Errorf("list_skills(filter_unavailable=True) differs\n  go        =\n%s\n  reference =\n%s",
					skFormatRows(got), skFormatRows(c.ListAvailable))
			}

			always := loader.GetAlwaysSkills()
			if !skStringsEqual(always, c.Always) {
				t.Errorf("get_always_skills differs\n  go        = %v\n  reference = %v", always, c.Always)
			}

			exclude := map[string]bool{}
			for _, name := range always {
				exclude[name] = true
			}
			summary, err := loader.BuildSkillsSummary(exclude, c.ProjectWorkspace)
			if err != nil {
				t.Errorf("BuildSkillsSummary returned an error: %v", err)
			} else if summary != *c.Summary {
				t.Errorf("build_skills_summary differs (go %d bytes, reference %d bytes)\n--- go ---\n%s\n--- reference ---\n%s",
					len(summary), len(*c.Summary), summary, *c.Summary)
			}

			if got := loader.LoadSkillsForContext(always); got != *c.ActiveContent {
				t.Errorf("load_skills_for_context differs\n  go        = %q\n  reference = %q", got, *c.ActiveContent)
			}

			skCompareSkillReports(t, loader, c)

			for _, text := range doc.ExplicitTexts {
				want := c.Explicit[text]
				if want.Error != nil {
					continue
				}
				if got := loader.GetExplicitlyInvokedSkills(text); !skStringsEqual(got, want.Invoked) {
					t.Errorf("get_explicitly_invoked_skills(%q) differs\n  go        = %v\n  reference = %v", text, got, want.Invoked)
				}
				wantContext := c.ExplicitContext[text]
				if wantContext.Error != nil {
					continue
				}
				gotContext := loader.BuildExplicitSkillRuntimeContext(text)
				switch {
				case wantContext.Source == nil && gotContext == nil:
				case wantContext.Source == nil && gotContext != nil:
					t.Errorf("build_explicit_skill_runtime_context(%q): reference returned None, go returned %+v", text, gotContext)
				case wantContext.Source != nil && gotContext == nil:
					t.Errorf("build_explicit_skill_runtime_context(%q): go returned nil, reference returned %q", text, *wantContext.Source)
				default:
					if gotContext.Source != *wantContext.Source {
						t.Errorf("build_explicit_skill_runtime_context(%q) source: go = %q, reference = %q", text, gotContext.Source, *wantContext.Source)
					}
					if wantContext.Content != nil && gotContext.Content != *wantContext.Content {
						t.Errorf("build_explicit_skill_runtime_context(%q) content differs\n  go        = %q\n  reference = %q",
							text, gotContext.Content, *wantContext.Content)
					}
				}
			}

			builder := prompt.Builder{
				Workspace:        c.AgentWorkspace,
				DisabledSkills:   c.DisabledSkills,
				BuiltinSkillsDir: c.BuiltinSkillsDir,
			}
			full := skNormalizeRuntime(builder.BuildSystemPrompt(c.Channel, nil, c.ProjectWorkspace, c.IncludeMemory))
			// Rebranding is deliberate, so the shipped SOUL line can never match
			// the reference's. Discount exactly that line and nothing else.
			gotPrompt := prompt.NormalizeBranding(full)
			wantPrompt := prompt.NormalizeBranding(*c.Prompt)
			if gotPrompt != wantPrompt {
				t.Errorf("full system prompt differs (go %d bytes, reference %d bytes)\n%s",
					len(gotPrompt), len(wantPrompt), skFirstDifference(gotPrompt, wantPrompt))
			}
			// prompt_len is a Python len(), i.e. a CODE POINT count, while
			// len(*c.Prompt) counts BYTES. Comparing the two directly reported a
			// 44-byte "inconsistency" in the dumper on every case whose prompt
			// contains multi-byte characters.
			if c.PromptLen != nil && utf8.RuneCountInString(*c.Prompt) != *c.PromptLen {
				t.Errorf("dumper prompt length %d does not match its own prompt string length %d code points",
					*c.PromptLen, utf8.RuneCountInString(*c.Prompt))
			}
		})
	}
	if compared == 0 {
		t.Fatal("compared 0 cases — the harness is broken, not passing")
	}
	t.Logf("compared %d workspace cases against the reference (%d skipped because the reference raised)", compared, skipped)
}

// skCompareSkillReports compares every per-skill observation the dumper
// recorded. Fields the reference reached by raising are skipped, because the
// port's documented behaviour there is to degrade rather than crash.
func skCompareSkillReports(t *testing.T, loader *skills.Loader, c skCase) {
	t.Helper()
	for _, name := range skSkillNames(c) {
		want := c.Skills[name]

		if want.LoadError == nil {
			content, found, err := loader.LoadSkillStrict(name)
			if err != nil {
				t.Errorf("%s: LoadSkill returned an error the reference did not: %v", name, err)
			} else if !found {
				t.Errorf("%s: LoadSkill reported not-found, the reference loaded it", name)
			} else {
				if want.LoadSHA256 != nil && skSHA256(content) != *want.LoadSHA256 {
					t.Errorf("%s: load_skill digest differs (go %d bytes, reference %d bytes)", name, len(content), *want.LoadLen)
				}
			}
		}

		if want.DescriptionError == nil && want.Description != nil {
			if got := loader.GetSkillDescription(name); got != *want.Description {
				t.Errorf("%s: get_skill_description differs\n  go        = %q\n  reference = %q", name, got, *want.Description)
			}
		}

		if want.MetadataError == nil {
			meta := loader.GetSkillMetadata(name)
			var got string
			if meta == nil {
				got = "<nil>"
			} else {
				got = skCanon(t, meta)
			}
			wantMeta := "<nil>"
			if want.MetadataCanon != nil {
				wantMeta = *want.MetadataCanon
			}
			if got != wantMeta {
				t.Errorf("%s: get_skill_metadata differs\n  go        = %s\n  reference = %s", name, got, wantMeta)
			}
		}

		if want.Available != nil {
			available, missing := loader.GetSkillAvailability(name)
			if available != *want.Available {
				t.Errorf("%s: get_skill_availability availability: go = %v, reference = %v", name, available, *want.Available)
			}
			if want.Missing != nil && missing != *want.Missing {
				t.Errorf("%s: get_skill_availability missing: go = %q, reference = %q", name, missing, *want.Missing)
			}
		}

		if want.Requirements != nil {
			got := loader.GetSkillRequirements(name)
			if !skStringsEqual(got.Bins, want.Requirements.Bins) {
				t.Errorf("%s: requirements bins: go = %v, reference = %v", name, got.Bins, want.Requirements.Bins)
			}
			if !skStringsEqual(got.Env, want.Requirements.Env) {
				t.Errorf("%s: requirements env: go = %v, reference = %v", name, got.Env, want.Requirements.Env)
			}
			if !skStringsEqual(got.MissingBins, want.Requirements.MissingBins) {
				t.Errorf("%s: requirements missing_bins: go = %v, reference = %v", name, got.MissingBins, want.Requirements.MissingBins)
			}
			if !skStringsEqual(got.MissingEnv, want.Requirements.MissingEnv) {
				t.Errorf("%s: requirements missing_env: go = %v, reference = %v", name, got.MissingEnv, want.Requirements.MissingEnv)
			}
		}
	}
}

// skFirstDifference renders the byte offsets around the first divergence so a
// whitespace-only failure is diagnosable.
func skFirstDifference(got, want string) string {
	limit := len(got)
	if len(want) < limit {
		limit = len(want)
	}
	index := -1
	for i := 0; i < limit; i++ {
		if got[i] != want[i] {
			index = i
			break
		}
	}
	if index < 0 {
		if len(got) == len(want) {
			return "  (strings are equal)"
		}
		index = limit
	}
	lo := index - 60
	if lo < 0 {
		lo = 0
	}
	hiGot, hiWant := index+60, index+60
	if hiGot > len(got) {
		hiGot = len(got)
	}
	if hiWant > len(want) {
		hiWant = len(want)
	}
	return fmt.Sprintf("first difference at byte %d\n  go        ...%q\n  reference ...%q",
		index, got[lo:hiGot], want[lo:hiWant])
}

// --------------------------------------------------------------------------
// the crash cases
// --------------------------------------------------------------------------

// TestSkillsReferenceCrashCases pins the three fixtures where the REFERENCE
// raises out of build_system_prompt, and asserts what the port does instead.
//
// The port's divergence here is deliberate and documented: a Go server that
// dies because one skill file is unreadable is worse than one that omits the
// section. What is NOT acceptable is the divergence being invisible, so this
// test fails if the reference ever stops raising (the fixtures would no longer
// be exercising the path) and if the port ever starts raising.
func TestSkillsReferenceCrashCases(t *testing.T) {
	doc, _ := loadSkillsDump(t)

	want := map[string]string{
		"skillmd_is_dir":                  "IsADirectoryError",
		"invalid_utf8":                    "UnicodeDecodeError",
		"symlinked_workspace_with_plugin": "ValueError",
	}
	seen := map[string]bool{}

	for _, c := range doc.Cases {
		prefix, expected := "", ""
		for name, kind := range want {
			if c.Name == name {
				prefix, expected = name, kind
			}
		}
		if prefix == "" {
			if c.SummaryError != nil || c.PromptError != nil {
				t.Errorf("case %s: the reference raised (%v / %v) but this harness does not expect it",
					c.Name, c.SummaryError, c.PromptError)
			}
			continue
		}
		seen[prefix] = true

		if c.PromptError == nil {
			t.Errorf("case %s: the reference did NOT raise; the fixture no longer exercises the failure path", prefix)
			continue
		}
		if !strings.HasPrefix(*c.PromptError, expected) {
			t.Errorf("case %s: reference raised %q, expected %s", prefix, *c.PromptError, expected)
		}
		t.Logf("case %s: reference raised %s", prefix, *c.PromptError)

		// The port must NOT raise, and must still produce a prompt.
		builder := prompt.Builder{
			Workspace:        c.AgentWorkspace,
			DisabledSkills:   c.DisabledSkills,
			BuiltinSkillsDir: c.BuiltinSkillsDir,
		}
		full := builder.BuildSystemPrompt(c.Channel, nil, c.ProjectWorkspace, c.IncludeMemory)
		if !strings.Contains(full, "## Runtime") {
			t.Errorf("case %s: the port produced a prompt with no identity section", prefix)
		}

		loader := skLoader(c)
		_, err := loader.BuildSkillsSummary(nil, c.ProjectWorkspace)
		switch prefix {
		case "symlinked_workspace_with_plugin":
			var relErr *skills.RelativeToError
			if err == nil {
				t.Errorf("case %s: BuildSkillsSummary returned no error; the reference raised ValueError", prefix)
			} else if !errors.As(err, &relErr) {
				t.Errorf("case %s: BuildSkillsSummary returned %T (%v), expected *skills.RelativeToError", prefix, err, err)
			} else {
				t.Logf("case %s: the port reports %v instead of crashing", prefix, err)
			}
		case "skillmd_is_dir", "invalid_utf8":
			// A read error is surfaced by LoadSkillStrict, and the summary is
			// produced with the unreadable skill degraded to "no metadata".
			_, _, readErr := loader.LoadSkillStrict("broken")
			if prefix == "skillmd_is_dir" && readErr == nil {
				_, _, readErr = loader.LoadSkillStrict("bad-bytes")
			}
			if prefix == "invalid_utf8" {
				_, _, readErr = loader.LoadSkillStrict("bad-bytes")
			}
			if readErr == nil {
				t.Errorf("case %s: LoadSkillStrict reported no error for the unreadable SKILL.md", prefix)
			} else {
				t.Logf("case %s: the port reports %v instead of crashing", prefix, readErr)
			}
			if err != nil {
				t.Errorf("case %s: BuildSkillsSummary returned %v; the port degrades instead of erroring here", prefix, err)
			}
		}
	}

	for name := range want {
		if !seen[name] {
			t.Errorf("the dumper produced no %q case — the harness is broken, not passing", name)
		}
	}
}

// --------------------------------------------------------------------------
// the divergence this change fixes
// --------------------------------------------------------------------------

// skLegacySummary reproduces the port's PRE-FIX behaviour verbatim: the
// ~25-line scan of <workspace>/skills in internal/prompt/context.go that
// printed a root path and bare relative paths, sorted, and knew nothing about
// the built-in skills, frontmatter, availability or always-active skills.
func skLegacySummary(workspace string) string {
	skillsDir := filepath.Join(workspace, "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(skillsDir, e.Name(), "SKILL.md")); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	var sb strings.Builder
	fmt.Fprintf(&sb, "- %s\n", skillsDir)
	for _, n := range names {
		fmt.Fprintf(&sb, "  - %s/SKILL.md\n", n)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// TestSkillsPreFixPortDiverged is the before/after evidence for the reported
// bug. It asserts that the pre-fix summary disagreed with the reference on a
// NON-ZERO number of cases, and that the port under test now agrees on all of
// them. Without the first half the differential would still pass if someone
// reverted the port, because the harness would be comparing a reverted port
// against itself.
func TestSkillsPreFixPortDiverged(t *testing.T) {
	doc, _ := loadSkillsDump(t)

	diverged := []string{}
	agreed := []string{}
	freshBefore := ""
	for _, c := range doc.Cases {
		if c.SummaryError != nil {
			continue
		}
		before := skLegacySummary(c.AgentWorkspace)
		if c.Name == "fresh_builtin_only" {
			freshBefore = before
		}
		if before != *c.Summary {
			diverged = append(diverged, c.Name)
		} else {
			agreed = append(agreed, c.Name)
		}

		loader := skLoader(c)
		exclude := map[string]bool{}
		for _, name := range loader.GetAlwaysSkills() {
			exclude[name] = true
		}
		after, err := loader.BuildSkillsSummary(exclude, c.ProjectWorkspace)
		if err != nil {
			t.Errorf("case %s: BuildSkillsSummary returned %v", c.Name, err)
			continue
		}
		if after != *c.Summary {
			t.Errorf("case %s: the port under test still differs from the reference", c.Name)
		}
	}

	if len(diverged) == 0 {
		t.Fatal("the pre-fix behaviour matched the reference in every case — the harness is not reproducing the reported bug")
	}
	if freshBefore != "" {
		t.Errorf("the pre-fix port emitted a skills summary for a fresh workspace; it should emit nothing (got %q)", freshBefore)
	}
	t.Logf("pre-fix behaviour diverged from the reference in %d/%d cases: %v", len(diverged), len(diverged)+len(agreed), diverged)
	t.Logf("pre-fix behaviour already agreed in %d cases: %v", len(agreed), agreed)
}

// skExpectedUnavailableSuffix renders the ` (unavailable: ...)` suffix the
// reference appends to a skill whose required CLIs are absent, derived from THIS
// HOST's PATH rather than from a recorded verdict.
//
// The suffix is computed at runtime on both sides — shutil.which in
// _get_missing_requirements (skills.py:278-284) and exec.LookPath in
// GetMissingRequirements (loader.go:477-491) — so it is a property of the
// MACHINE, not of the port. The CI runner has tmux on PATH; this developer
// machine does not. Pinning the verdict made TestFreshInstallSkillsSection fail
// on CI with a summary exactly 25 bytes shorter, which is precisely the length
// of " (unavailable: CLI: tmux)".
func skExpectedUnavailableSuffix(bins ...string) string {
	var missing []string
	for _, bin := range bins {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, "CLI: "+bin)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return " (unavailable: " + strings.Join(missing, ", ") + ")"
}

// skBuiltinSkillBlock locates the built-in skills group of a rendered prompt and
// returns its entry lines plus the index range they occupy in the prompt's
// lines. ok is false when the prompt has no built-in group.
func skBuiltinSkillBlock(rendered string) (lines []string, start, end int, ok bool) {
	all := strings.Split(rendered, "\n")
	for i, line := range all {
		if !strings.HasPrefix(line, "### Built-in skills (`") {
			continue
		}
		j := i + 1
		for j < len(all) && strings.HasPrefix(all[j], "- **") {
			j++
		}
		return all, i + 1, j, true
	}
	return nil, 0, 0, false
}

// skNormalizeBuiltinSkillOrder returns the prompt with the entries of its
// built-in skills group sorted, and every other line untouched.
//
// It exists because the two sides of TestSkillsEmbeddedDefaultMatchesExplicitDir
// obtain the SAME skills in different orders: the embedded tree comes back
// lexicographic (go:embed sorts), while an explicit directory comes back in
// readdir order, which is a property of the host filesystem. Sorting that one
// group makes the comparison insensitive to the non-reproducible order while
// leaving the group header, every description, availability suffix and relative
// path, and every other section of the prompt under byte-for-byte comparison.
func skNormalizeBuiltinSkillOrder(rendered string) string {
	all, start, end, ok := skBuiltinSkillBlock(rendered)
	if !ok {
		return rendered
	}
	block := append([]string(nil), all[start:end]...)
	sort.Strings(block)
	copy(all[start:end], block)
	return strings.Join(all, "\n")
}

// skBuiltinSkillNames lists the skill names a rendered prompt advertises in its
// built-in group, in the order they appear.
func skBuiltinSkillNames(rendered string) []string {
	all, start, end, ok := skBuiltinSkillBlock(rendered)
	if !ok {
		return nil
	}
	names := make([]string, 0, end-start)
	for _, line := range all[start:end] {
		rest, found := strings.CutPrefix(line, "- **")
		if !found {
			continue
		}
		if index := strings.Index(rest, "**"); index >= 0 {
			names = append(names, rest[:index])
		}
	}
	return names
}

// TestFreshInstallSkillsSection is the headline measurement: the section the
// model now receives on a fresh install, quoted, with its measured length.
func TestFreshInstallSkillsSection(t *testing.T) {
	doc, _ := loadSkillsDump(t)

	var fresh *skCase
	for i := range doc.Cases {
		if doc.Cases[i].Name == "fresh_builtin_only" {
			fresh = &doc.Cases[i]
		}
	}
	if fresh == nil {
		t.Fatal("dumper produced no fresh_builtin_only case")
	}
	if fresh.Summary == nil || fresh.Prompt == nil || fresh.PromptLen == nil {
		t.Fatalf("the dumper recorded no fresh-install summary/prompt (summary_error=%v, prompt_error=%v)",
			fresh.SummaryError, fresh.PromptError)
	}

	loader := skLoader(*fresh)
	summary, err := loader.BuildSkillsSummary(nil, fresh.ProjectWorkspace)
	if err != nil {
		t.Fatalf("BuildSkillsSummary: %v", err)
	}
	if summary != *fresh.Summary {
		t.Fatalf("fresh-install summary differs from the reference")
	}

	// The exact shape the reference emits: an em dash, TWO spaces before the
	// backticked path, and a group header naming the relative root.
	//
	// The `(unavailable: ...)` suffix is NOT part of that shape: it is derived
	// from this host's PATH at runtime, so it is expected here exactly when the
	// CLI really is missing.
	for _, want := range []string{
		"### Built-in skills (`skills`)",
		"- **clawhub** \u2014 Search and install agent skills from ClawHub, the public skill registry.  `clawhub/SKILL.md`",
		"- **summarize** \u2014 Summarize or extract text/transcripts from URLs, podcasts, and local files (great fallback for \u201ctranscribe this YouTube/video\u201d)." + skExpectedUnavailableSuffix("summarize") + "  `summarize/SKILL.md`",
		"- **tmux** \u2014 Remote-control tmux sessions for interactive CLIs by sending keystrokes and scraping pane output." + skExpectedUnavailableSuffix("tmux") + "  `tmux/SKILL.md`",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("fresh-install summary is missing:\n  %q\nactual:\n%s", want, summary)
		}
	}
	if got := skExpectedUnavailableSuffix("tmux") + skExpectedUnavailableSuffix("summarize"); got == "" {
		t.Logf("note: tmux and summarize are BOTH on PATH here, so this run exercises only the no-suffix branch; a host with one of them absent (CI has tmux, not summarize) exercises the other")
	} else {
		t.Logf("note: unavailable suffix expected on this host: %q", got)
	}

	// Cross-check the helper against the reference's OWN verdict for the same
	// PATH. The reference computed it with shutil.which inside the dumper, so a
	// disagreement means the helper is wrong about this host — and every
	// expectation above derived from it would be wrong with it.
	for _, name := range []string{"tmux", "summarize"} {
		report, ok := fresh.Skills[name]
		if !ok || report.Missing == nil {
			t.Fatalf("the dumper recorded no availability verdict for the built-in skill %q", name)
		}
		want := ""
		if *report.Missing != "" {
			want = " (unavailable: " + *report.Missing + ")"
		}
		if got := skExpectedUnavailableSuffix(name); got != want {
			t.Errorf("the host-derived suffix for %s is %q, but the reference reported %q for the same PATH", name, got, want)
		}
	}

	// A fresh install must never have a workspace or plugin group.
	if strings.Contains(summary, "Workspace skills") || strings.Contains(summary, "Agent Plugin skills") {
		t.Errorf("a fresh install produced a workspace or plugin group:\n%s", summary)
	}

	builder := prompt.Builder{Workspace: fresh.AgentWorkspace, BuiltinSkillsDir: fresh.BuiltinSkillsDir}
	full := builder.BuildSystemPrompt("cli", nil, fresh.AgentWorkspace, true)
	if !strings.Contains(full, "# Skills\n\nThe following skills extend your capabilities.") {
		t.Fatal("the rendered prompt is missing the # Skills section")
	}
	index := strings.Index(full, "# Skills")
	section := full[index:]
	if !strings.Contains(section, summary) {
		t.Error("the rendered # Skills section does not contain the summary verbatim")
	}

	// The full prompt against the reference's, with the built-in group's ORDER
	// normalised (both sides read the same directory here, so this is a no-op in
	// practice; it is what keeps the comparison from depending on a filesystem
	// property) and with the two documented divergences discounted: the runtime
	// line and the shipped branding. Everything else — every section, every
	// description, availability suffix and relative path — must match exactly,
	// which is what catches a missing section or altered skill content.
	gotPrompt := skNormalizeBuiltinSkillOrder(prompt.NormalizeBranding(skNormalizeRuntime(full)))
	wantPrompt := skNormalizeBuiltinSkillOrder(prompt.NormalizeBranding(*fresh.Prompt))
	if gotPrompt != wantPrompt {
		t.Errorf("the fresh-install prompt differs from the reference's (go %d bytes, reference %d bytes)\n%s",
			len(gotPrompt), len(wantPrompt), skFirstDifference(gotPrompt, wantPrompt))
	}

	// *fresh.PromptLen is a Python len(), i.e. a CODE POINT count, while len() in
	// Go counts BYTES. Reporting the two side by side produced a phantom 44-byte
	// "difference" on every host: this prompt carries 11 em dashes, two curly
	// quotes and 8 CJK characters, and each costs one extra byte in UTF-8.
	t.Logf("reference prompt: %d code points / %d bytes; port prompt: %d code points / %d bytes; skills summary: %d bytes",
		*fresh.PromptLen, len(*fresh.Prompt), utf8.RuneCountInString(gotPrompt), len(gotPrompt), len(summary))
	t.Logf("fresh-install skills summary:\n%s", summary)
}

// TestActiveSkillsSectionFromWorkspaceSkill proves the "# Active Skills" path
// end to end. The bundled skills carry no `always`, so the section is invisible
// on a fresh install for BOTH implementations — it only appears once a
// workspace skill sets it, which is what this fixture does.
func TestActiveSkillsSectionFromWorkspaceSkill(t *testing.T) {
	doc, _ := loadSkillsDump(t)

	checked := 0
	for _, c := range doc.Cases {
		if c.ActiveError != nil || len(c.Always) == 0 || c.ActiveContent == nil {
			continue
		}
		checked++
		builder := prompt.Builder{
			Workspace:        c.AgentWorkspace,
			DisabledSkills:   c.DisabledSkills,
			BuiltinSkillsDir: c.BuiltinSkillsDir,
		}
		full := skNormalizeRuntime(builder.BuildSystemPrompt(c.Channel, nil, c.ProjectWorkspace, c.IncludeMemory))
		if !strings.Contains(full, "# Active Skills\n\n"+*c.ActiveContent) {
			t.Errorf("case %s: the prompt is missing the # Active Skills section", c.Name)
		}
		// The active skills are EXCLUDED from the summary below.
		for _, name := range c.Always {
			if strings.Contains(*c.Summary, "- **"+name+"**") {
				t.Errorf("case %s: always-active skill %q is still advertised in the summary", c.Name, name)
			}
		}
		t.Logf("case %s: %d always-active skills (%v), # Active Skills is %d bytes",
			c.Name, len(c.Always), c.Always, len(*c.ActiveContent))
	}
	if checked == 0 {
		t.Fatal("no case produced a non-empty # Active Skills section — the harness is broken, not passing")
	}
	t.Logf("verified the # Active Skills section in %d cases", checked)
}

// TestSkillsEmbeddedDefaultMatchesExplicitDir is a Go-only check with no
// reference involved: the EMBEDDED built-in tree must produce the same prompt as
// an explicit on-disk directory, for a case whose group roots are relative (so
// the root path itself never appears). It is what makes the production default
// trustworthy, since the differential harness always passes an explicit
// directory in order to compare absolute roots byte for byte.
//
// The two prompts CANNOT be compared byte for byte as rendered, because the two
// sides get the built-in skills in different orders: the embedded tree comes
// back lexicographic (go:embed sorts by filename, by specification) while an
// explicit directory comes back in readdir order, which is a property of the
// host filesystem and not reproducible from an embedded tree. So the built-in
// group's entries are sorted on both sides first — see
// skNormalizeBuiltinSkillOrder. The real intent is preserved and still checked:
// the group header, every entry's description, availability suffix and relative
// path, the workspace group above it, and every other section of the prompt must
// still match byte for byte.
func TestSkillsEmbeddedDefaultMatchesExplicitDir(t *testing.T) {
	doc, _ := loadSkillsDump(t)

	checked, permuted := 0, 0
	for _, c := range doc.Cases {
		if c.SummaryError != nil || c.Name != "fresh_builtin_only" && c.Name != "workspace_skills_creation_order" {
			continue
		}
		checked++
		embedded := prompt.Builder{Workspace: c.AgentWorkspace, DisabledSkills: c.DisabledSkills}
		explicit := prompt.Builder{
			Workspace:        c.AgentWorkspace,
			DisabledSkills:   c.DisabledSkills,
			BuiltinSkillsDir: c.BuiltinSkillsDir,
		}
		gotEmbedded := skNormalizeRuntime(embedded.BuildSystemPrompt(c.Channel, nil, c.ProjectWorkspace, c.IncludeMemory))
		gotExplicit := skNormalizeRuntime(explicit.BuildSystemPrompt(c.Channel, nil, c.ProjectWorkspace, c.IncludeMemory))

		embeddedNames := skBuiltinSkillNames(gotEmbedded)
		explicitNames := skBuiltinSkillNames(gotExplicit)
		if len(embeddedNames) == 0 {
			t.Fatalf("case %s: the embedded built-in tree rendered no built-in skills group — the harness is broken, not passing", c.Name)
		}
		if len(explicitNames) == 0 {
			t.Fatalf("case %s: the explicit directory rendered no built-in skills group", c.Name)
		}
		if !sort.StringsAreSorted(embeddedNames) {
			t.Errorf("case %s: the embedded built-in group is not lexicographic: %v", c.Name, embeddedNames)
		}
		if !sort.StringsAreSorted(explicitNames) {
			permuted++
		}

		normalizedEmbedded := skNormalizeBuiltinSkillOrder(gotEmbedded)
		normalizedExplicit := skNormalizeBuiltinSkillOrder(gotExplicit)
		if normalizedEmbedded != normalizedExplicit {
			t.Errorf("case %s: the embedded built-in tree and the explicit directory produce different prompts once the built-in group's order is normalised\n%s",
				c.Name, skFirstDifference(normalizedEmbedded, normalizedExplicit))
		}
	}
	if checked == 0 {
		t.Fatal("no relative-root case was exercised — the harness is broken, not passing")
	}
	if permuted == 0 {
		// Same honesty as the previous fix in this series: say out loud when the
		// host's readdir order is already lexicographic, because then this run
		// cannot distinguish "order-insensitive" from "order happened to agree".
		t.Logf("note: the explicit directory's readdir order was already lexicographic on this host in all %d cases, so this run cannot distinguish the two orders; the normalisation above is what makes the assertion hold on a host where it is not", checked)
	}
	t.Logf("the embedded built-in tree matched an explicit directory in %d cases (%d of them with a non-lexicographic readdir order)", checked, permuted)
}

// TestSkillsSummaryShapeGuards pins the two formatting details that are easy to
// "clean up" by accident: the EM DASH separator and the TWO spaces before the
// backticked relative path. Both come straight from
// skills.py:261 `f"- **{skill_name}** — {desc}{suffix}  \`{relative_path}\`"`.
func TestSkillsSummaryShapeGuards(t *testing.T) {
	doc, _ := loadSkillsDump(t)

	loader := skills.NewWithBuiltinDir(doc.BuiltinSkillsDir, doc.BuiltinSkillsDir)
	summary, err := loader.BuildSkillsSummary(nil, doc.BuiltinSkillsDir)
	if err != nil {
		t.Fatalf("BuildSkillsSummary: %v", err)
	}
	if summary == "" {
		t.Fatal("the built-in directory produced an empty summary")
	}

	if !strings.Contains(summary, "\u2014") {
		t.Error("the summary is missing the U+2014 em dash separator")
	}
	if strings.Contains(summary, " - ") {
		t.Error("the summary used an ASCII hyphen instead of the em dash")
	}
	if !strings.Contains(summary, ".  `") {
		t.Error("the summary is missing the two spaces before the backticked path")
	}
	// Every entry must carry a NON-EMPTY skill name between its bold markers.
	//
	// The guard cannot be a bare `strings.Contains(summary, "** \u2014")`: the
	// correct format is "- **clawhub** \u2014 desc", where the CLOSING "**" is
	// itself followed by a space and the em dash, so that substring appears in
	// every well-formed line and the check failed on correct output. Anchor on
	// the list bullet and inspect the bold span instead.
	entries := 0
	for _, line := range strings.Split(summary, "\n") {
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		entries++
		rest := line[2:]
		if !strings.HasPrefix(rest, "**") {
			t.Errorf("summary entry is not bold-prefixed: %q", line)
			continue
		}
		end := strings.Index(rest[2:], "**")
		if end < 0 {
			t.Errorf("summary entry has no closing bold marker: %q", line)
			continue
		}
		if strings.TrimSpace(rest[2:2+end]) == "" {
			t.Errorf("summary entry has an empty skill name inside the bold markers: %q", line)
		}
	}
	if entries == 0 {
		t.Fatal("the summary has no entry lines — the guard is broken, not passing")
	}
	if !strings.HasPrefix(summary, "### ") {
		t.Errorf("the summary does not start with a group header: %q", summary[:skMin(40, len(summary))])
	}
}

func skMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestSkillsProbeEnvironmentMatchesReference is a control. Availability is
// computed from PATH and the environment, so if the two sides ran with
// different ones every `(unavailable: ...)` suffix comparison would be
// meaningless while still looking like a pass.
func TestSkillsProbeEnvironmentMatchesReference(t *testing.T) {
	doc, _ := loadSkillsDump(t)

	for name, want := range doc.ProbeBins {
		_, err := exec.LookPath(name)
		got := err == nil
		if got != want {
			t.Errorf("shutil.which(%q) = %v in the reference but exec.LookPath reports %v here", name, want, got)
		}
	}
	if !doc.ProbeBins["gh"] {
		t.Log("NOTE: `gh` is absent, so the github skill is unavailable on this machine")
	}
	if !doc.ProbeBins["tmux"] || !doc.ProbeBins["summarize"] {
		t.Log("NOTE: tmux and/or summarize are absent, so those skills carry an (unavailable: CLI: ...) suffix")
	}
	if len(doc.ProbeEnv) == 0 {
		t.Fatal("dumper reported no probe environment — the harness is broken")
	}
	for name, want := range doc.ProbeEnv {
		if got := os.Getenv(name); got != want {
			t.Errorf("probe env %s: go = %q, reference = %q", name, got, want)
		}
	}
	t.Logf("compared %d command probes and %d environment probes", len(doc.ProbeBins), len(doc.ProbeEnv))
}
