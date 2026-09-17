package filediff

import (
	"strings"
	"testing"
)

// Expectations here were read out of the frozen reference
// (build_unified_diff_payload in nanobot/utils/file_edit_events.py at
// HKUDS/nanobot@1bb712d3488915ca4ed9ccc1a93067ff722f5ab9) via
// compat/python/dump_apply_patch.py.

func TestBuildUnifiedDiffPayloadDefaults(t *testing.T) {
	before := "one\ntwo\nthree\n"
	after := "one\n2\nthree\nfour\n"
	got := BuildUnifiedDiffPayload(&before, &after, DefaultUnifiedDiffOptions())
	if got == nil {
		t.Fatal("payload is nil")
	}
	if got.Format != "unified" {
		t.Errorf("Format = %q, want \"unified\"", got.Format)
	}
	if got.Context != 3 {
		t.Errorf("Context = %d, want 3", got.Context)
	}
	if got.Truncated {
		t.Error("Truncated = true, want false")
	}
	want := "--- before\n+++ after\n@@ -1,3 +1,4 @@\n one\n+2\n-two\n three\n+four"
	if got.Text != want {
		t.Errorf("Text\n got %q\nwant %q", got.Text, want)
	}
}

func TestBuildUnifiedDiffPayloadLabels(t *testing.T) {
	before := "one\ntwo\nthree\n"
	after := "one\n2\nthree\nfour\n"
	opts := DefaultUnifiedDiffOptions()
	opts.FromFile = "a.txt"
	opts.ToFile = "a.txt"
	got := BuildUnifiedDiffPayload(&before, &after, opts)
	want := "--- a.txt\n+++ a.txt\n@@ -1,3 +1,4 @@\n one\n+2\n-two\n three\n+four"
	if got == nil || got.Text != want {
		t.Fatalf("Text\n got %v\nwant %q", got, want)
	}
}

// TestBuildUnifiedDiffPayloadContext pins that a negative context is reported
// verbatim in the payload while the rendered body uses max(0, context).
func TestBuildUnifiedDiffPayloadContext(t *testing.T) {
	before := "one\ntwo\nthree\n"
	after := "one\n2\nthree\nfour\n"
	wantText := "--- before\n+++ after\n@@ -2 +2 @@\n+2\n-two\n@@ -3,0 +4 @@\n+four"

	for _, context := range []int{0, -1} {
		opts := DefaultUnifiedDiffOptions()
		opts.ContextLines = context
		got := BuildUnifiedDiffPayload(&before, &after, opts)
		if got == nil {
			t.Fatalf("context %d: payload is nil", context)
		}
		if got.Context != context {
			t.Errorf("context %d: Context = %d, want %d (passed through verbatim)", context, got.Context, context)
		}
		if got.Text != wantText {
			t.Errorf("context %d: Text\n got %q\nwant %q", context, got.Text, wantText)
		}
	}
}

// TestBuildUnifiedDiffPayloadMaxLines pins the truncation contract, including
// the fact that the hunk header is REWRITTEN to describe the truncated body.
func TestBuildUnifiedDiffPayloadMaxLines(t *testing.T) {
	before := "one\ntwo\nthree\n"
	after := "one\n2\nthree\nfour\n"

	opts := DefaultUnifiedDiffOptions()
	opts.MaxLines = 1
	got := BuildUnifiedDiffPayload(&before, &after, opts)
	if got == nil {
		t.Fatal("max_lines=1: payload is nil")
	}
	if !got.Truncated {
		t.Error("max_lines=1: Truncated = false, want true")
	}
	if want := "--- before\n+++ after\n@@ -1 +1 @@\n one"; got.Text != want {
		t.Errorf("max_lines=1: Text\n got %q\nwant %q", got.Text, want)
	}

	opts.MaxLines = 2
	got = BuildUnifiedDiffPayload(&before, &after, opts)
	if want := "--- before\n+++ after\n@@ -1 +1,2 @@\n one\n+2"; got == nil || got.Text != want {
		t.Errorf("max_lines=2: Text\n got %v\nwant %q", got, want)
	}

	// max_lines=0 yields NO payload at all, not an empty one.
	opts.MaxLines = 0
	if got := BuildUnifiedDiffPayload(&before, &after, opts); got != nil {
		t.Errorf("max_lines=0: payload = %+v, want nil", got)
	}
}

// TestBuildUnifiedDiffPayloadMaxLineChars pins that 0 means "unlimited" and
// that truncation is counted in CHARACTERS, including the +/-/space prefix.
func TestBuildUnifiedDiffPayloadMaxLineChars(t *testing.T) {
	before := "one\ntwo\nthree\n"
	after := "one\n2\nthree\nfour\n"

	opts := DefaultUnifiedDiffOptions()
	opts.MaxLineChars = 0
	got := BuildUnifiedDiffPayload(&before, &after, opts)
	if got == nil {
		t.Fatal("max_line_chars=0: payload is nil")
	}
	if got.Truncated {
		t.Error("max_line_chars=0: Truncated = true, want false (0 means unlimited)")
	}
	if want := "--- before\n+++ after\n@@ -1,3 +1,4 @@\n one\n+2\n-two\n three\n+four"; got.Text != want {
		t.Errorf("max_line_chars=0: Text\n got %q\nwant %q", got.Text, want)
	}

	opts.MaxLineChars = 3
	got = BuildUnifiedDiffPayload(&before, &after, opts)
	if want := "--- before\n+++ after\n@@ -1,3 +1,4 @@\n one\n+2\n-two\n thr\n+fou"; got == nil || got.Text != want {
		t.Errorf("max_line_chars=3: Text\n got %v\nwant %q", got, want)
	}
	if got != nil && !got.Truncated {
		t.Error("max_line_chars=3: Truncated = false, want true")
	}

	opts.MaxLineChars = 1
	got = BuildUnifiedDiffPayload(&before, &after, opts)
	if want := "--- before\n+++ after\n@@ -1,3 +1,4 @@\n o\n+2\n-t\n t\n+f"; got == nil || got.Text != want {
		t.Errorf("max_line_chars=1: Text\n got %v\nwant %q", got, want)
	}
}

// TestBuildUnifiedDiffPayloadDefaultLineCharCap pins the 1200-character default
// against a 1300-character line.
func TestBuildUnifiedDiffPayloadDefaultLineCharCap(t *testing.T) {
	before := strings.Repeat("l", 1300) + "\n"
	after := "m\n"
	got := BuildUnifiedDiffPayload(&before, &after, DefaultUnifiedDiffOptions())
	if got == nil {
		t.Fatal("payload is nil")
	}
	if !got.Truncated {
		t.Error("Truncated = false, want true for a 1300-char line at the 1200 default")
	}
	lines := strings.Split(got.Text, "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d: %q", len(lines), lines)
	}
	if lines[3] != "+m" {
		t.Errorf("line 3 = %q, want \"+m\"", lines[3])
	}
	// The cap applies to the CONTENT only: the leading +/-/space marker is kept
	// in addition to max_line_chars characters, so the emitted line is one
	// character longer than the cap. That is the reference's behaviour
	// (utils/file_edit_events.py:296-300), verified by running it.
	if got := len([]rune(lines[4])); got != MaxDiffLineChars+1 {
		t.Errorf("truncated line has %d characters, want %d (marker + %d)",
			got, MaxDiffLineChars+1, MaxDiffLineChars)
	}
	if !strings.HasPrefix(lines[4], "-lll") {
		t.Errorf("truncated line = %q..., want it to start with \"-lll\"", lines[4][:min(8, len(lines[4]))])
	}
}

// TestBuildUnifiedDiffPayloadNilInputs pins that a missing side yields no
// payload rather than an empty one.
func TestBuildUnifiedDiffPayloadNilInputs(t *testing.T) {
	after := "a\n"
	if got := BuildUnifiedDiffPayload(nil, &after, DefaultUnifiedDiffOptions()); got != nil {
		t.Errorf("before=nil: payload = %+v, want nil", got)
	}
	before := "a\n"
	if got := BuildUnifiedDiffPayload(&before, nil, DefaultUnifiedDiffOptions()); got != nil {
		t.Errorf("after=nil: payload = %+v, want nil", got)
	}
	if got := BuildUnifiedDiffPayload(nil, nil, DefaultUnifiedDiffOptions()); got != nil {
		t.Errorf("both nil: payload = %+v, want nil", got)
	}
}

// TestBuildUnifiedDiffPayloadLargeFileContextWindow checks the context window
// on a 601-line file: only the neighbourhood of the change is emitted, and no
// truncation is needed.
func TestBuildUnifiedDiffPayloadLargeFileContextWindow(t *testing.T) {
	before := strings.Repeat("x\n", 300) + "y\n" + strings.Repeat("x\n", 300)
	after := strings.Repeat("x\n", 300) + "z\n" + strings.Repeat("x\n", 300)
	got := BuildUnifiedDiffPayload(&before, &after, DefaultUnifiedDiffOptions())
	if got == nil {
		t.Fatal("payload is nil")
	}
	if got.Truncated {
		t.Error("Truncated = true, want false: only 11 lines are emitted")
	}
	want := "--- before\n+++ after\n@@ -298,7 +298,7 @@\n" +
		" x\n x\n x\n+z\n-y\n x\n x\n x"
	if got.Text != want {
		t.Errorf("Text\n got %q\nwant %q", got.Text, want)
	}
}

// TestRewriteHunkHeaderForBody exercises the header rewrite directly. Every
// expectation below was produced by calling the reference's
// _rewrite_hunk_header_for_body with the same arguments, including the arms
// unified_lines never produces on its own.
func TestRewriteHunkHeaderForBody(t *testing.T) {
	tests := []struct {
		name   string
		header string
		body   []string
		want   string
	}{
		{"single_line", "@@ -1,3 +1,4 @@", []string{" one"}, "@@ -1 +1 @@"},
		{"two_lines", "@@ -1,3 +1,4 @@", []string{" one", "+2"}, "@@ -1 +1,2 @@"},
		{"full_body", "@@ -1,3 +1,4 @@", []string{" one", "+2", "-two", " three", "+four"}, "@@ -1,3 +1,4 @@"},
		// An insert-only body keeps the original header untouched.
		{"only_insert", "@@ -3,0 +4 @@", []string{"+four"}, "@@ -3,0 +4 @@"},
		{"only_insert_long", "@@ -5,2 +5,2 @@", []string{"+a", "+b"}, "@@ -5,0 +5,2 @@"},
		{"only_delete", "@@ -2 +2 @@", []string{"-two"}, "@@ -2 +2,0 @@"},
		{"only_delete_long", "@@ -5,2 +5,2 @@", []string{"-a", "-b"}, "@@ -5,2 +5,0 @@"},
		{"empty_body", "@@ -1,3 +1,4 @@", nil, "@@ -1,0 +1,0 @@"},
		{"header_already_single", "@@ -1 +1 @@", []string{"+x", "-y"}, "@@ -1 +1 @@"},
		{"mixed", "@@ -5,2 +5,2 @@", []string{" a", "-b", "+c"}, "@@ -5,2 +5,2 @@"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rewriteHunkHeaderForBody(tc.header, tc.body); got != tc.want {
				t.Fatalf("rewriteHunkHeaderForBody(%q, %q) = %q, want %q", tc.header, tc.body, got, tc.want)
			}
		})
	}
}

// TestPySplitLines pins Python's str.splitlines() semantics, which the diff
// relies on. Go's bufio.ScanLines and strings.Split both differ.
func TestPySplitLines(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a\n", []string{"a"}},
		{"a\nb", []string{"a", "b"}},
		{"a\r\nb", []string{"a", "b"}},
		{"a\rb", []string{"a", "b"}},
		{"a\vb", []string{"a", "b"}},
		{"a\fb", []string{"a", "b"}},
		{"a\x1cb", []string{"a", "b"}},
		{"a\x1db", []string{"a", "b"}},
		{"a\x1eb", []string{"a", "b"}},
		{"a\u0085b", []string{"a", "b"}},
		{"a\u2028b", []string{"a", "b"}},
		{"a\u2029b", []string{"a", "b"}},
		{"\n", []string{""}},
		{"\n\n", []string{"", ""}},
		{"a\n\n", []string{"a", ""}},
		{"\r\n", []string{""}},
		{"a\r\n\r\nb", []string{"a", "", "b"}},
		{"caf\u00e9\n", []string{"caf\u00e9"}},
	}
	for _, tc := range tests {
		got := pySplitLines(tc.in)
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("pySplitLines(%q) = %q, want %q", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("pySplitLines(%q) = %q, want %q", tc.in, got, tc.want)
				break
			}
		}
	}
}
