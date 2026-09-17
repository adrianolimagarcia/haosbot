package workspaceprompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every expectation in this file was produced by running the reference
// (nanobot.utils.workspace_prompts) in the project venv, not by reading it.
// The values are reproduced verbatim so a future edit that "cleans up" the
// port has to argue with the interpreter.

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFile(t *testing.T) {
	if got, want := File("/ws", "dream"), filepath.Join("/ws", "prompts", "dream.md"); got != want {
		t.Fatalf("File = %q, want %q", got, want)
	}
}

func TestMaxChars(t *testing.T) {
	if MaxChars != 32000 {
		t.Fatalf("MaxChars = %d, reference WORKSPACE_PROMPT_MAX_CHARS is 32000", MaxChars)
	}
}

func TestLoadAbsence(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		data []byte
	}{
		{"missing", nil},
		{"empty", []byte("")},
		{"spaces only", []byte("   \n\t ")},
		// U+001C is whitespace to Python and NOT to strings.TrimSpace. This is
		// the case that separates a correct port from a plausible one.
		{"unit sep only", []byte("\x1c\x1d")},
		{"nbsp only", []byte("\u00a0")},
		{"invalid utf8", []byte("hello \xff\xfe")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, tc.name+".md")
			if tc.data != nil {
				writeFile(t, p, tc.data)
			}
			text, n, ok := Load(p, MaxChars)
			if ok || text != "" || n != 0 {
				t.Fatalf("Load = (%q, %d, %v), want (\"\", 0, false)", text, n, ok)
			}
			if Has(p) {
				t.Fatalf("Has = true, want false")
			}
		})
	}
}

func TestLoadTrimsPythonWhitespace(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		data []byte
	}{
		{"plain", []byte("hello")},
		{"trailing ws", []byte("hello   \n\t")},
		{"trailing unit sep", []byte("hello\x1c")},
		{"trailing nbsp", []byte("hello\u00a0")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, tc.name+".md")
			writeFile(t, p, tc.data)
			text, n, ok := Load(p, MaxChars)
			if !ok {
				t.Fatal("Load reported no override")
			}
			if text != "hello" {
				t.Errorf("text = %q, want %q", text, "hello")
			}
			// original_chars counts characters AFTER the rstrip, which the
			// reference confirms (5, not 6, for "hello\x1c").
			if n != 5 {
				t.Errorf("originalChars = %d, want 5", n)
			}
		})
	}
}

func TestLoadCountsCharactersNotBytes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cjk.md")
	writeFile(t, p, []byte("\u65e5\u672c\u8a9e"))
	text, n, ok := Load(p, MaxChars)
	if !ok || text != "\u65e5\u672c\u8a9e" {
		t.Fatalf("Load = (%q, %d, %v)", text, n, ok)
	}
	if n != 3 {
		t.Fatalf("originalChars = %d, want 3 (characters, not the 9 bytes)", n)
	}
}

func TestLoadTruncatesAndReportsPreTruncationLength(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.md")
	writeFile(t, p, []byte(strings.Repeat("Z", MaxChars+500)))

	text, n, ok := Load(p, MaxChars)
	if !ok {
		t.Fatal("Load reported no override")
	}
	// The reference reports 32500: the length BEFORE truncation.
	if n != MaxChars+500 {
		t.Errorf("originalChars = %d, want %d", n, MaxChars+500)
	}
	// And the result is the limit plus the suffix, because truncate_text cuts
	// first and appends afterwards.
	if len([]rune(text)) != MaxChars+len([]rune("\n... (truncated)")) {
		t.Errorf("len(text) = %d runes, want %d", len([]rune(text)), MaxChars+16)
	}
	if !strings.HasSuffix(text, "\n... (truncated)") {
		t.Errorf("text does not end with the reference suffix: %q", text[len(text)-24:])
	}
}

func TestLoadExplicitMaxChars(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "small.md")
	writeFile(t, p, []byte("abcdefghij"))
	text, n, ok := Load(p, 4)
	if !ok {
		t.Fatal("Load reported no override")
	}
	if text != "abcd\n... (truncated)" {
		t.Errorf("text = %q, want %q", text, "abcd\n... (truncated)")
	}
	if n != 10 {
		t.Errorf("originalChars = %d, want 10", n)
	}
}

// TestLoadNonPositiveMaxCharsFallsBackToDefault pins the choice made for Go's
// missing keyword argument: a non-positive limit selects MaxChars rather than
// disabling the cap. Disabling it would let a pathological file reach the
// model's context window unbounded, which the reference never allows.
func TestLoadNonPositiveMaxCharsFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.md")
	writeFile(t, p, []byte(strings.Repeat("Z", MaxChars+500)))

	text, _, ok := Load(p, 0)
	if !ok {
		t.Fatal("Load reported no override")
	}
	if len([]rune(text)) != MaxChars+16 {
		t.Fatalf("maxChars=0 did not apply the default cap: %d runes", len([]rune(text)))
	}
}

func TestInitialize(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name      string
		data      []byte
		asDir     bool
		wantWrite bool
		wantBody  string
	}{
		{"missing", nil, false, true, "DEFAULT\n"},
		{"empty", []byte(""), false, true, "DEFAULT\n"},
		{"spaces only", []byte("   "), false, true, "DEFAULT\n"},
		// Python's .strip() empties this, so the reference OVERWRITES. A port
		// using strings.TrimSpace would refuse to write.
		{"unit sep only", []byte("\x1c"), false, true, "DEFAULT\n"},
		{"nonempty", []byte("existing"), false, false, "existing"},
		{"invalid utf8", []byte("\xff\xfe"), false, false, "\xff\xfe"},
		{"is a directory", nil, true, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, "init_"+tc.name)
			if tc.asDir {
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
			} else if tc.data != nil {
				writeFile(t, p, tc.data)
			}

			wrote, err := Initialize(p, "DEFAULT")
			if err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			if wrote != tc.wantWrite {
				t.Fatalf("Initialize returned %v, want %v", wrote, tc.wantWrite)
			}
			if tc.asDir {
				info, err := os.Stat(p)
				if err != nil || !info.IsDir() {
					t.Fatalf("directory was replaced: %v", err)
				}
				return
			}
			got, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.wantBody {
				t.Fatalf("file content = %q, want %q", got, tc.wantBody)
			}
		})
	}
}

func TestInitializeCreatesParentDirectories(t *testing.T) {
	dir := t.TempDir()
	p := File(dir, "dream") // <dir>/prompts/dream.md — prompts/ does not exist
	wrote, err := Initialize(p, "BODY")
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if !wrote {
		t.Fatal("Initialize did not write into a missing parent directory")
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "BODY\n" {
		t.Fatalf("content = %q, want %q", got, "BODY\n")
	}
}
