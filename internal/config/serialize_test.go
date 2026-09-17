package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/pyjson"
)

// readSaved writes cfg and returns the file contents.
func readSaved(t *testing.T, cfg *Config) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := cfg.Save(p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(raw)
}

// TestSavedFloatsKeepPythonFormatting pins the int/float distinction.
//
// Python writes float 1.0 as "1.0"; Go's encoder writes "1". A config file
// written by this port and read by the reference would turn a float into an
// int, which pydantic accepts but which is not the value that was saved. The
// thresholds below are the reference's own, verified by running json.dumps.
func TestSavedFloatsKeepPythonFormatting(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{1.0, `"temperature": 1.0,`},
		{0.1, `"temperature": 0.1,`},
		{2.5, `"temperature": 2.5,`},
		{100.0, `"temperature": 100.0,`},
		{0.0, `"temperature": 0.0,`},
		// Exponent 15 stays plain, 16 switches to scientific. This boundary is
		// Python's, not Go's, and Go's 'g' verb switches elsewhere.
		{1e15, `"temperature": 1000000000000000.0,`},
		{1e16, `"temperature": 1e+16,`},
		// Exponent -4 stays plain, -5 switches.
		{1e-4, `"temperature": 0.0001,`},
		{1e-5, `"temperature": 1e-05,`},
	}
	for _, tc := range cases {
		cfg := DefaultConfig()
		cfg.Agents.Defaults.Temperature = pyjson.Float(tc.in)
		saved := readSaved(t, cfg)

		if !strings.Contains(saved, tc.want) {
			t.Errorf("temperature %v: saved file lacks %q", tc.in, tc.want)
		}
	}
}

// TestSavedFloatRoundTripsAsFloat verifies the saved value parses back as a
// JSON float, not an integer. This is the property that actually matters to a
// consumer, independent of the exact text.
func TestSavedFloatRoundTripsAsFloat(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Agents.Defaults.Temperature = 1.0
	saved := readSaved(t, cfg)

	dec := json.NewDecoder(strings.NewReader(saved))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	agents, _ := doc["agents"].(map[string]any)
	defaults, _ := agents["defaults"].(map[string]any)
	num, ok := defaults["temperature"].(json.Number)
	if !ok {
		t.Fatalf("temperature is %T, want json.Number", defaults["temperature"])
	}
	if !strings.Contains(num.String(), ".") {
		t.Errorf("temperature round-tripped as %s, want a float", num.String())
	}
}

// TestSavedStringsMatchPythonEscaping pins the three escaping rules that
// differ between Go's encoder and json.dumps(..., ensure_ascii=False).
func TestSavedStringsMatchPythonEscaping(t *testing.T) {
	cfg := DefaultConfig()
	// <, > and & are handled by SetEscapeHTML(false).
	cfg.Agents.Defaults.BotName = "a < b & c > d"
	if saved := readSaved(t, cfg); !strings.Contains(saved, "a < b & c > d") {
		t.Errorf("angle brackets and ampersand must stay literal:\n%s", firstLine(saved))
	}

	// U+2028/U+2029 are escaped by Go unconditionally; Python leaves them
	// literal. The file must contain the actual characters.
	cfg.Agents.Defaults.BotName = "line\u2028sep\u2029end"
	saved := readSaved(t, cfg)
	if strings.Contains(saved, `\u2028`) || strings.Contains(saved, `\u2029`) {
		t.Errorf("U+2028/U+2029 must stay literal:\n%s", firstLine(saved))
	}
	if !strings.Contains(saved, "line\u2028sep\u2029end") {
		t.Error("the separators did not survive as characters")
	}

	// Non-ASCII stays literal under ensure_ascii=False.
	cfg.Agents.Defaults.BotName = "unicode \u2713 \u65e5\u672c"
	if saved := readSaved(t, cfg); !strings.Contains(saved, "unicode \u2713 \u65e5\u672c") {
		t.Errorf("non-ASCII must stay literal:\n%s", firstLine(saved))
	}
}

// TestSavedLiteralBackslashEscapeSurvives guards the trap in the unescaping
// pass: a string that genuinely contains the six characters \u2028 is written
// by Go as \\u2028, and a naive replacement would corrupt it into a real
// separator.
func TestSavedLiteralBackslashEscapeSurvives(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Agents.Defaults.BotName = `literal \u2028 text`
	saved := readSaved(t, cfg)

	if !strings.Contains(saved, `literal \\u2028 text`) {
		t.Fatalf("literal backslash-u2028 was corrupted:\n%s", firstLine(saved))
	}
	// And it must still parse back to exactly what was saved.
	dec := json.NewDecoder(strings.NewReader(saved))
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	agents, _ := doc["agents"].(map[string]any)
	defaults, _ := agents["defaults"].(map[string]any)
	if defaults["botName"] != `literal \u2028 text` {
		t.Errorf("round trip changed the value: %q", defaults["botName"])
	}
}

// TestUnescapeLineSeparators covers the scanner directly, including the
// backslash-run arithmetic that the naive implementation gets wrong.
func TestUnescapeLineSeparators(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`\u2028`, "\u2028"},
		{`\u2029`, "\u2029"},
		{`a\u2028b`, "a\u2028b"},
		{`\\u2028`, `\\u2028`},        // escaped backslash: literal text, untouched
		{`\\\u2028`, `\\` + "\u2028"}, // three backslashes: the last one escapes
		{`\u2027`, `\u2027`},          // a different escape is not touched
		{`\u202`, `\u202`},            // too short
		{"plain", "plain"},
		{`\n\t`, `\n\t`},
	}
	for _, tc := range cases {
		got := string(pyjson.UnescapeLineSeparators([]byte(tc.in)))
		if got != tc.want {
			t.Errorf("UnescapeLineSeparators(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
