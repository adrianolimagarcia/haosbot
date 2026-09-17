package openai

import (
	"encoding/json"
	"testing"
)

// ptrInt is a local helper so the nestedInt expectations read as pointers.
func ptrInt(v int) *int { return &v }

// TestIntOrZeroMatchesPython pins `int(value or 0)` from
// openai_compat_provider.py:1481-1484. Every want was produced by running
// Python 3.14; the three unparseable-string rows are deliberate divergences and
// are asserted explicitly rather than skipped, so that a future change which
// silently makes them parseable is caught.
func TestIntOrZeroMatchesPython(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
		note string
	}{
		{"plain string", "150", 150, ""},
		{"padded string", " 150 ", 150, ""},
		{"underscore string", "1_0", 10, "Python int() allows underscores between digits"},
		{"arabic-indic string", "\u0661\u0662", 12, "Python int() accepts every Nd digit"},
		{"fullwidth string", "\uff15", 5, ""},
		{"empty string", "", 0, `"" is falsy, so ` + "`or 0`" + ` yields 0`},
		{"nil", nil, 0, ""},
		{"json int", json.Number("7"), 7, ""},
		{"json float truncates", json.Number("1.9"), 1, "Python int(1.9) is 1"},
		{"json negative float truncates toward zero", json.Number("-1.9"), -1, "Python int(-1.9) is -1"},
		{"json zero", json.Number("0"), 0, ""},
		{"true is 1", true, 1, "int(True or 0) is 1, not 0"},
		{"false is 0", false, 0, ""},

		// DELIBERATE DIVERGENCES — the reference raises ValueError out of
		// _parse for each of these, because every non-empty string is truthy
		// and int() then rejects it. This port reports 0 instead of failing
		// the whole request over one malformed usage field.
		{"whitespace-only string", "   ", 0, "Python: int('   ' or 0) raises ValueError"},
		{"non-numeric string", "abc", 0, "Python: int('abc' or 0) raises ValueError"},
		{"float string", "1.5", 0, "Python: int('1.5' or 0) raises ValueError"},
		{"hex string", "0x10", 0, "Python: int('0x10' or 0) raises ValueError"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := intOrZero(tc.in); got != tc.want {
				t.Errorf("intOrZero(%#v) = %d, want %d%s", tc.in, got, tc.want, noteSuffix(tc.note))
			}
		})
	}
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return " (" + note + ")"
}

// TestNestedIntMatchesPython pins _get_nested_int
// (openai_compat_provider.py:1520-1540). The function's contract is subtle: an
// explicit zero is a PRESENT value and must survive, while a boolean counts as
// absent.
func TestNestedIntMatchesPython(t *testing.T) {
	cases := []struct {
		name string
		obj  map[string]any
		path []string
		want *int
	}{
		{
			"explicit zero is preserved",
			map[string]any{"total_tokens": json.Number("0")},
			[]string{"total_tokens"},
			ptrInt(0),
		},
		{
			"bool counts as absent",
			map[string]any{"total_tokens": true},
			[]string{"total_tokens"},
			nil,
		},
		{
			"false counts as absent",
			map[string]any{"total_tokens": false},
			[]string{"total_tokens"},
			nil,
		},
		{
			"nil is absent",
			map[string]any{"total_tokens": nil},
			[]string{"total_tokens"},
			nil,
		},
		{
			"missing key",
			map[string]any{},
			[]string{"total_tokens"},
			nil,
		},
		{
			"plain string",
			map[string]any{"total_tokens": "150"},
			[]string{"total_tokens"},
			ptrInt(150),
		},
		{
			"underscore string",
			map[string]any{"total_tokens": "1_0"},
			[]string{"total_tokens"},
			ptrInt(10),
		},
		{
			// int() tolerates NBSP, unlike the four file-separator characters
			// below. This is exactly the PyInt/PyAtoi split.
			"nbsp-padded string",
			map[string]any{"total_tokens": "5\u00a0"},
			[]string{"total_tokens"},
			ptrInt(5),
		},
		{
			"file separator is rejected",
			map[string]any{"total_tokens": "5\x1c"},
			[]string{"total_tokens"},
			nil,
		},
		{
			"non-numeric string",
			map[string]any{"total_tokens": "abc"},
			[]string{"total_tokens"},
			nil,
		},
		{
			"float string is not an int",
			map[string]any{"total_tokens": "1.5"},
			[]string{"total_tokens"},
			nil,
		},
		{
			"json float truncates",
			map[string]any{"total_tokens": json.Number("1.9")},
			[]string{"total_tokens"},
			ptrInt(1),
		},
		{
			"nested path",
			map[string]any{"prompt_tokens_details": map[string]any{"cached_tokens": json.Number("9")}},
			[]string{"prompt_tokens_details", "cached_tokens"},
			ptrInt(9),
		},
		{
			"intermediate is not a map",
			map[string]any{"prompt_tokens_details": "not-a-map"},
			[]string{"prompt_tokens_details", "cached_tokens"},
			nil,
		},
		{
			"empty path returns the object itself, which is not an int",
			map[string]any{"total_tokens": json.Number("3")},
			nil,
			nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nestedInt(tc.obj, tc.path...)
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("nestedInt(%v, %v) = %d, want nil", tc.obj, tc.path, *got)
			case tc.want != nil && got == nil:
				t.Errorf("nestedInt(%v, %v) = nil, want %d", tc.obj, tc.path, *tc.want)
			case tc.want != nil && got != nil && *got != *tc.want:
				t.Errorf("nestedInt(%v, %v) = %d, want %d", tc.obj, tc.path, *got, *tc.want)
			}
		})
	}
}
