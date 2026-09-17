package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// repoRootForSkills resolves the module root from this file's location
// (<root>/internal/skills/builtin_test.go).
func repoRootForSkills(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller path")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// bundledSkillNamesPinned is every built-in skill directory the reference
// ships, written out by hand rather than derived from the FS so that dropping a
// skill from the embedded tree is a test failure rather than a silently smaller
// set. The prompt's "# Skills" section is built from exactly these names.
var bundledSkillNamesPinned = []string{
	"clawhub",
	"cron",
	"github",
	"image-generation",
	"memory",
	"my",
	"skill-creator",
	"summarize",
	"tmux",
	"update-setup",
	"weather",
}

// bundledFileCountPinned is the number of files under the reference's
// nanobot/skills/ (README.md plus each skill's SKILL.md, the my/ reference doc
// and the two skills' scripts).
const bundledFileCountPinned = 18

// TestBundledSkillsMatchReference is the anti-drift check: the embedded bytes
// must be identical to the frozen reference's nanobot/skills/, because the
// built-in group's descriptions and its `(unavailable: ...)` suffixes are read
// straight out of these files and shown to the model.
//
// It compares SHA-256 rather than only bytes so the failure message names the
// two digests, which is what makes "which side moved?" answerable.
func TestBundledSkillsMatchReference(t *testing.T) {
	refDir := filepath.Join(repoRootForSkills(t), "upstream", "nanobot", "nanobot", "skills")
	if _, err := os.Stat(refDir); err != nil {
		t.Skipf("SKIP: reference skills not present at %s — drift check not run", refDir)
	}

	var refFiles []string
	err := filepath.Walk(refDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(refDir, path)
		if relErr != nil {
			return relErr
		}
		refFiles = append(refFiles, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk reference skills: %v", err)
	}
	sort.Strings(refFiles)

	embedded := BundledSkillFiles()
	if len(embedded) != len(refFiles) {
		t.Errorf("embedded file count %d != reference file count %d\n  embedded  = %v\n  reference = %v",
			len(embedded), len(refFiles), embedded, refFiles)
	}
	if len(refFiles) != bundledFileCountPinned {
		t.Errorf("the reference now ships %d files, the test pins %d — update the pin deliberately",
			len(refFiles), bundledFileCountPinned)
	}

	for _, rel := range refFiles {
		content, ok := BundledSkillFile(rel)
		if !ok {
			t.Errorf("reference file %q is not embedded", rel)
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(refDir, filepath.FromSlash(rel)))
		if readErr != nil {
			t.Errorf("read reference file %s: %v", rel, readErr)
			continue
		}
		if content != string(raw) {
			gotSum := sha256.Sum256([]byte(content))
			wantSum := sha256.Sum256(raw)
			t.Errorf("bundled file %q differs from the reference:\n  embedded sha256 = %s\n  reference sha256 = %s",
				rel, hex.EncodeToString(gotSum[:]), hex.EncodeToString(wantSum[:]))
		}
	}
	if len(refFiles) == 0 {
		t.Fatal("the reference tree is empty — the drift check cannot pass vacuously")
	}
	t.Logf("verified %d embedded files against %s", len(refFiles), refDir)
}

// TestBundledSkillNamesPinned pins the skill directory list. A skill appearing
// or disappearing here changes what the model is told it can do.
func TestBundledSkillNamesPinned(t *testing.T) {
	got := BundledSkillNames()
	want := append([]string(nil), bundledSkillNamesPinned...)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("BundledSkillNames() = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("BundledSkillNames() = %v, want %v", got, want)
		}
	}
}

// TestExtractBuiltinSkills writes the embedded tree to disk and checks it byte
// for byte. internal/memory/dream.go points the Dream prompt and the Dream
// tool's read policy at <BUILTIN_SKILLS_DIR>/skill-creator/SKILL.md, so a
// deployment that wants the reference's behaviour needs real files.
func TestExtractBuiltinSkills(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "skills")
	if err := ExtractBuiltinSkills(dest); err != nil {
		t.Fatalf("ExtractBuiltinSkills: %v", err)
	}
	for _, rel := range BundledSkillFiles() {
		want, _ := BundledSkillFile(rel)
		raw, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("extracted file %s: %v", rel, err)
			continue
		}
		if string(raw) != want {
			t.Errorf("extracted file %s differs from the embedded copy", rel)
		}
	}
	// Re-extraction must not overwrite a modified file.
	target := filepath.Join(dest, "cron", "SKILL.md")
	if err := os.WriteFile(target, []byte("customised"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ExtractBuiltinSkills(dest); err != nil {
		t.Fatalf("second ExtractBuiltinSkills: %v", err)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "customised" {
		t.Error("ExtractBuiltinSkills overwrote an existing file")
	}
}

// TestDefaultBuiltinSkillsDirIsExeRelative pins the shape of the default. The
// reference's BUILTIN_SKILLS_DIR is `Path(__file__).parent.parent / "skills"`,
// which for a single binary has no direct analogue; the closest is the skills/
// directory beside the executable, and internal/memory/dream.go uses the same
// rule for its own copy of the variable.
func TestDefaultBuiltinSkillsDirIsExeRelative(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("cannot resolve the executable path")
	}
	want := filepath.Join(filepath.Dir(exe), "skills")
	if got := DefaultBuiltinSkillsDir(); got != want {
		t.Errorf("DefaultBuiltinSkillsDir() = %q, want %q", got, want)
	}
}

// --------------------------------------------------------------------------
// pathlib semantics
// --------------------------------------------------------------------------

func TestPyPathParts(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/a/b", "[/ a b]"},
		{"a/b/", "[a b]"},
		{"/", "[/]"},
		{"", "[]"},
		{"//a//b//", "[/ a b]"},
		{"a/./b", "[a . b]"},
		{"a/../b", "[a .. b]"},
	}
	for _, tc := range cases {
		got := "[" + strings.Join(pyPathParts(tc.path), " ") + "]"
		if got != tc.want {
			t.Errorf("pyPathParts(%q) = %s, want %s", tc.path, got, tc.want)
		}
	}
}

func TestPyJoinDoesNotClean(t *testing.T) {
	// PurePath does not normalise "..", unlike filepath.Join.
	if got, want := pyJoin("/a/b/../c", "d"), "/a/b/../c/d"; got != want {
		t.Errorf("pyJoin = %q, want %q", got, want)
	}
	if got, want := pyJoin("/a", "b", "SKILL.md"), "/a/b/SKILL.md"; got != want {
		t.Errorf("pyJoin = %q, want %q", got, want)
	}
	if got, want := pyJoin("", "b"), "b"; got != want {
		t.Errorf("pyJoin = %q, want %q", got, want)
	}
	if got, want := pyJoin("/", "b"), "/b"; got != want {
		t.Errorf("pyJoin = %q, want %q", got, want)
	}
}

func TestPyRelativeTo(t *testing.T) {
	cases := []struct {
		path, root string
		want       string
		ok         bool
	}{
		{"/a/b/c", "/a/b", "c", true},
		{"/a/b", "/a/b", "", true},
		{"/a/bc", "/a/b", "", false},
		{"/x/b", "/a", "", false},
		{"a/b", "a", "b", true},
		// Purely lexical: ".." is not resolved.
		{"/a/../b", "/a", "../b", true},
		// A symlinked spelling does not match a resolved one.
		{"/real/ws/p/x.md", "/link/ws/p", "", false},
	}
	for _, tc := range cases {
		got, ok := pyRelativeTo(tc.path, tc.root)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("pyRelativeTo(%q, %q) = (%q, %v), want (%q, %v)", tc.path, tc.root, got, ok, tc.want, tc.ok)
		}
	}
}

// --------------------------------------------------------------------------
// Python string semantics
// --------------------------------------------------------------------------

// TestStripFrontmatterUsesPythonStrip pins the .strip() vs TrimSpace boundary.
// U+001C..U+001F are whitespace to Python and not to Go, so a frontmatter
// delimiter padded with them must still be stripped.
func TestStripFrontmatterUsesPythonStrip(t *testing.T) {
	if textutil.PyStrip("\x1c") != "" {
		t.Fatal("premise broken: PyStrip must strip U+001C")
	}
	content := "---\nname: x\n---\n\x1c\x1d body \x1e\x1f"
	if got, want := StripFrontmatter(content), "body"; got != want {
		t.Errorf("StripFrontmatter = %q, want %q (strings.TrimSpace would leave %q)",
			got, want, strings.TrimSpace("body"))
	}
	// Content that does not start with "---" is returned unchanged.
	if got := StripFrontmatter("# plain\n"); got != "# plain\n" {
		t.Errorf("StripFrontmatter changed content with no frontmatter: %q", got)
	}
	// "---" with no newline: startswith passes, the regex does not match.
	if got := StripFrontmatter("---"); got != "---" {
		t.Errorf("StripFrontmatter(\"---\") = %q, want %q", got, "---")
	}
}

// TestPySpaceClassCoversPythonWhitespace is the premise behind the explicit
// character class in stripSkillFrontmatter: Go's `\s` is ASCII-only, so the
// class has to carry the code points that differ.
func TestPySpaceClassCoversPythonWhitespace(t *testing.T) {
	// Every code point textutil.PyIsSpace accepts must be matched by the
	// compiled pattern's `\s`-equivalent run.
	for _, cp := range []rune{'\t', '\n', '\v', '\f', '\r', '\x1c', '\x1d', '\x1e', '\x1f',
		' ', '\u0085', '\u00a0', '\u1680', '\u2000', '\u200a', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000'} {
		if !textutil.PyIsSpace(cp) {
			t.Fatalf("premise broken: PyIsSpace(%U) is false", cp)
		}
		content := "---" + string(cp) + "\nname: x\n---\n"
		if meta := ParseSkillMetadata(content); meta == nil {
			t.Errorf("frontmatter with U+%04X after the opening fence did not parse", cp)
		}
	}
	// A non-whitespace code point there must NOT parse: it would mean the
	// opening fence is not a fence.
	if meta := ParseSkillMetadata("---x\nname: x\n---\n"); meta != nil {
		t.Errorf("frontmatter with a non-whitespace character after the fence parsed: %v", meta)
	}
}

// --------------------------------------------------------------------------
// the YAML subset
// --------------------------------------------------------------------------

// TestYAMLSubsetKnownValues covers the shapes skill frontmatter actually uses.
// The adversarial corpus is compared against the real yaml.safe_load in
// compat/skills_differential_test.go; this test is the offline safety net.
func TestYAMLSubsetKnownValues(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"plain mapping", "name: x\ndescription: A skill.", `{"description": "A skill.", "name": "x"}`},
		{"flow mapping", `metadata: {"nanobot":{"always":true}}`, `{"metadata": {"nanobot": {"always": true}}}`},
		{"nested block", "metadata:\n  nanobot:\n    always: true", `{"metadata": {"nanobot": {"always": true}}}`},
		{"block sequence", "bins:\n  - gh\n  - tmux", `{"bins": ["gh", "tmux"]}`},
		{"sequence same indent", "bins:\n- gh\n- tmux", `{"bins": ["gh", "tmux"]}`},
		{"flow sequence", "bins: [gh, tmux]", `{"bins": ["gh", "tmux"]}`},
		{"double quoted", `description: "a: b"`, `{"description": "a: b"}`},
		{"single quoted", "description: 'a ''b'' c'", `{"description": "a 'b' c"}`},
		{"escapes", `description: "tab\there"`, `{"description": "tab\there"}`},
		{"comment", "name: x # trailing\ndescription: d", `{"description": "d", "name": "x"}`},
		{"hash in value", "description: a # b", `{"description": "a"}`},
		{"url value", "homepage: https://example.com/:x", `{"homepage": "https://example.com/:x"}`},
		{"bool true", "always: true", `{"always": true}`},
		{"bool yes", "always: yes", `{"always": true}`},
		{"bool off", "always: off", `{"always": false}`},
		{"int", "count: 42", `{"count": 42}`},
		{"negative int", "count: -7", `{"count": -7}`},
		{"octal", "mode: 0755", `{"mode": 493}`},
		{"hex", "mode: 0x1f", `{"mode": 31}`},
		{"float", "ratio: 1.5", `{"ratio": 1.5}`},
		{"float needs signed exponent", "ratio: 1e5", `{"ratio": "1e5"}`},
		{"float signed exponent", "ratio: 1.0e+3", `{"ratio": 1000.0}`},
		{"null tilde", "x: ~", `{"x": null}`},
		{"null empty", "x:", `{"x": null}`},
		{"empty string quoted", `x: ""`, `{"x": ""}`},
		{"literal block", "d: |\n  one\n  two", `{"d": "one\ntwo\n"}`},
		{"literal strip", "d: |-\n  one\n  two", `{"d": "one\ntwo"}`},
		{"folded block", "d: >\n  one\n  two", `{"d": "one two\n"}`},
		{"folded strip", "d: >-\n  one\n  two", `{"d": "one two"}`},
		{"duplicate keys last wins", "a: 1\na: 2", `{"a": 2}`},
		{"numeric key stringified", "1: one", `{"1": "one"}`},
		{"bool key stringified", "true: yes", `{"True": true}`},
		{"empty document", "", `null`},
		{"null document", "null", `null`},
		{"sequence document", "- a\n- b", `["a", "b"]`},
		{"compact sequence mapping", "items:\n  - id: a\n    label: A", `{"items": [{"id": "a", "label": "A"}]}`},
		// Verified against the reference: yaml.safe_load("<<: {a: 1}") is
		// {"a": 1} — a top-level merge key with nothing to merge into yields
		// its own mapping, and parse_skill_metadata returns it as a dict.
		{"top-level merge key", "<<: {a: 1}", `{"a": 1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := yamlLoad(tc.src)
			if err != nil {
				t.Fatalf("yamlLoad(%q) error: %v", tc.src, err)
			}
			if rendered := renderCanon(got); rendered != tc.want {
				t.Errorf("yamlLoad(%q) = %s, want %s", tc.src, rendered, tc.want)
			}
		})
	}
}

// TestYAMLSubsetRejectsUnsupported pins the direction of the unsupported
// constructs: they must ERROR, which makes parse_skill_metadata return None,
// rather than be silently mis-parsed into a wrong description or a wrong
// availability verdict.
//
// EXCEPTION (verified against the reference, PyYAML 6 + the frozen
// parse_skill_metadata): a merge key at the TOP LEVEL of a mapping is NOT an
// error — yaml.safe_load("<<: {a: 1}") returns {"a": 1} because with nothing to
// merge into, the merge key's own mapping becomes the result. The reference
// therefore returns {'a': 1, ...} for the merge-key frontmatter document, and
// the port must agree, so this case is asserted in TestYAMLSubsetKnownValues
// instead of here.
func TestYAMLSubsetRejectsUnsupported(t *testing.T) {
	unsupported := []struct{ name, src string }{
		{"tab indentation", "name: x\n\tmetadata: y"},
		{"complex key", "? [a]\n: b"},
		{"unterminated quote", `a: "unterminated`},
		{"unterminated flow", "a: {b: 1"},
		{"two documents", "a: 1\n---\nb: 2"},
		{"bad indentation", "a: 1\n  b: 2"},
		{"line with no colon", "just a line\nand another"},
	}
	for _, tc := range unsupported {
		t.Run(tc.name, func(t *testing.T) {
			if value, err := yamlLoad(tc.src); err == nil {
				t.Errorf("yamlLoad(%q) = %v with no error; unsupported constructs must be rejected", tc.src, value)
			}
		})
	}
}

// renderCanon is a test-local canonical renderer matching the dumper's _canon
// for the value shapes the YAML subset produces. Strings are emitted as
// canonical JSON (double-quoted, like json.dumps on the Python side), not
// Python repr — the want literals below are JSON.
func renderCanon(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case string:
		return skJSONQuoteForTest(typed)
	case int64:
		return itoa(typed)
	case float64:
		if typed == float64(int64(typed)) {
			return itoa(int64(typed)) + ".0"
		}
		return trimFloat(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, renderCanon(item))
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
			parts = append(parts, skJSONQuoteForTest(key)+": "+renderCanon(typed[key]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return "<?>"
}

// skJSONQuoteForTest renders a string as canonical JSON, matching the dumper's
// json.dumps(value, ensure_ascii=False) and the compat harness's skJSONQuote.
func skJSONQuoteForTest(s string) string {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return "<?>"
	}
	return strings.TrimRight(buf.String(), "\n")
}

func trimFloat(f float64) string {
	s := itoa(int64(f * 1e6))
	for len(s) < 7 {
		s = "0" + s
	}
	return s[:len(s)-6] + "." + strings.TrimRight(s[len(s)-6:], "0")
}

// --------------------------------------------------------------------------
// frontmatter predicates
// --------------------------------------------------------------------------

func TestParseSkillMetadataNoneCases(t *testing.T) {
	noneCases := map[string]string{
		"no frontmatter":       "# body\n",
		"unclosed":             "---\nname: x\n",
		"empty body":           "---\n---\n",
		"sequence root":        "---\n- a\n---\n",
		"scalar root":          "---\nhello\n---\n",
		"null root":            "---\nnull\n---\n",
		"tab indentation":      "---\nname: x\n\tbad: y\n---\n",
		"only dashes":          "---",
		"four dashes":          "----\nname: x\n---\n",
		"bom before the fence": "\ufeff---\nname: x\n---\n",
		// A frontmatter block holding only a YAML comment parses to None, not
		// to a mapping: yaml.safe_load("# nothing") is None, and
		// _load_frontmatter rejects anything that is not a dict with
		// "Frontmatter must be a YAML dictionary". Verified by running the
		// reference (skills/skill-creator/scripts/quick_validate.py):
		//
		//   _load_frontmatter("# nothing")
		//     -> (None, 'Frontmatter must be a YAML dictionary')
		//
		// The same is true of "", "   \n  ", "- a\n- b" and "just a string".
		"comments only": "---\n# nothing\n---\n",
	}
	for name, content := range noneCases {
		if meta := ParseSkillMetadata(content); meta != nil {
			t.Errorf("%s: ParseSkillMetadata returned %v, want nil", name, meta)
		}
	}

	mappingCases := map[string]string{
		"basic":               "---\nname: x\n---\n",
		"crlf":                "---\r\nname: x\r\n---\r\n",
		"no trailing newline": "---\nname: x\n---",
	}
	for name, content := range mappingCases {
		if meta := ParseSkillMetadata(content); meta == nil {
			t.Errorf("%s: ParseSkillMetadata returned nil, want a mapping", name)
		}
	}
}

func TestValidSkillMetadata(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"name": "demo", "description": "A demo skill."}
	}
	if !ValidSkillMetadata(base(), "demo") {
		t.Error("a well-formed identity was rejected")
	}
	if ValidSkillMetadata(base(), "other") {
		t.Error("a mismatched name was accepted")
	}

	meta := base()
	meta["name"] = true
	if ValidSkillMetadata(meta, "demo") {
		t.Error("a non-string name was accepted")
	}

	meta = base()
	meta["description"] = 123
	if ValidSkillMetadata(meta, "demo") {
		t.Error("a non-string description was accepted")
	}

	meta = base()
	meta["description"] = ""
	if ValidSkillMetadata(meta, "demo") {
		t.Error("an empty description was accepted")
	}

	meta = base()
	meta["description"] = "   "
	if ValidSkillMetadata(meta, "demo") {
		t.Error("a whitespace-only description was accepted")
	}

	// The bounds are CODE POINT counts. 1024 é characters is 2048 bytes but
	// 1024 code points, so it is valid; 1025 is not.
	meta = base()
	meta["description"] = strings.Repeat("é", 1024)
	if !ValidSkillMetadata(meta, "demo") {
		t.Error("a 1024-code-point description was rejected")
	}
	meta["description"] = strings.Repeat("é", 1025)
	if ValidSkillMetadata(meta, "demo") {
		t.Error("a 1025-code-point description was accepted")
	}

	if !ValidSkillMetadata(map[string]any{"name": "a", "description": "d"}, "a") {
		t.Error("a single-character name was rejected")
	}
	if !ValidSkillMetadata(map[string]any{"name": strings.Repeat("a", 64), "description": "d"}, strings.Repeat("a", 64)) {
		t.Error("a 64-character name was rejected")
	}
	if ValidSkillMetadata(map[string]any{"name": strings.Repeat("a", 65), "description": "d"}, strings.Repeat("a", 65)) {
		t.Error("a 65-character name was accepted")
	}
	for _, bad := range []string{"BadName", "bad_name", "bad-", "-bad", "bad--name", "bäd"} {
		if ValidSkillMetadata(map[string]any{"name": bad, "description": "d"}, bad) {
			t.Errorf("invalid skill name %q was accepted", bad)
		}
	}
}

func TestParseNanobotMetadata(t *testing.T) {
	// A mapping wins over a JSON string only in the sense that both are
	// accepted; nanobot wins over openclaw.
	got := ParseNanobotMetadata(map[string]any{
		"nanobot":  map[string]any{"always": true},
		"openclaw": map[string]any{"always": false},
	})
	if !pyTruthy(got["always"]) {
		t.Errorf("nanobot must win over openclaw, got %v", got)
	}

	got = ParseNanobotMetadata(map[string]any{"openclaw": map[string]any{"always": true}})
	if !pyTruthy(got["always"]) {
		t.Errorf("the openclaw fallback was not used, got %v", got)
	}

	got = ParseNanobotMetadata(`{"nanobot":{"always":true}}`)
	if !pyTruthy(got["always"]) {
		t.Errorf("a JSON string payload was not parsed, got %v", got)
	}

	for _, raw := range []any{
		nil,
		"not json",
		"true",
		"[1,2]",
		42,
		[]any{1},
		map[string]any{"nanobot": nil},
		map[string]any{"nanobot": []any{1}},
		map[string]any{"other": map[string]any{"always": true}},
	} {
		if got := ParseNanobotMetadata(raw); len(got) != 0 {
			t.Errorf("ParseNanobotMetadata(%#v) = %v, want {}", raw, got)
		}
	}
}
