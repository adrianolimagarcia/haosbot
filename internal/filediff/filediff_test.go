package filediff

import (
	"os"
	"reflect"
	"testing"
)

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// The expectations in this file were produced by executing the frozen
// reference (nanobot/utils/file_edit_events.py at
// HKUDS/nanobot@1bb712d3488915ca4ed9ccc1a93067ff722f5ab9) through
// compat/python/dump_apply_patch.py. They are reproduced here so the package
// can be checked without the reference venv.

func TestFileDiffFromTextReferenceCase(t *testing.T) {
	before := "one\ntwo\nthree\n"
	after := "one\n2\nthree\nfour\n"
	diff := FileDiffFromText(before, after)

	if want := []string{"one", "two", "three"}; !reflect.DeepEqual(diff.BeforeLines, want) {
		t.Errorf("BeforeLines = %q, want %q", diff.BeforeLines, want)
	}
	if want := []string{"one", "2", "three", "four"}; !reflect.DeepEqual(diff.AfterLines, want) {
		t.Errorf("AfterLines = %q, want %q", diff.AfterLines, want)
	}
	wantOps := []Opcode{
		op(TagEqual, 0, 1, 0, 1),
		op(TagInsert, 1, 1, 1, 2),
		op(TagDelete, 1, 2, 2, 2),
		op(TagEqual, 2, 3, 2, 3),
		op(TagInsert, 3, 3, 3, 4),
	}
	if !reflect.DeepEqual(diff.Opcodes, wantOps) {
		t.Errorf("Opcodes\n got %v\nwant %v", diff.Opcodes, wantOps)
	}
	if diff.Added != 2 || diff.Deleted != 1 {
		t.Errorf("counts = (+%d/-%d), want (+2/-1)", diff.Added, diff.Deleted)
	}
	if !diff.Matches(before, after) {
		t.Error("Matches must be true for the text the diff was built from")
	}
	if diff.Matches(before, "different") {
		t.Error("Matches must be false for different text")
	}
}

func TestFileDiffUnifiedLinesReferenceCases(t *testing.T) {
	before := "one\ntwo\nthree\n"
	after := "one\n2\nthree\nfour\n"
	diff := FileDiffFromText(before, after)

	tests := []struct {
		context int
		want    []string
	}{
		{
			// context 0 splits into two hunks and, crucially, emits the
			// insertion BEFORE the deletion: the opcode order is the
			// alignment's, not a sorted one.
			context: 0,
			want: []string{
				"--- before",
				"+++ after",
				"@@ -2 +2 @@",
				"+2",
				"-two",
				"@@ -3,0 +4 @@",
				"+four",
			},
		},
		{
			context: 1,
			want: []string{
				"--- before",
				"+++ after",
				"@@ -1,3 +1,4 @@",
				" one",
				"+2",
				"-two",
				" three",
				"+four",
			},
		},
		{
			context: 3,
			want: []string{
				"--- before",
				"+++ after",
				"@@ -1,3 +1,4 @@",
				" one",
				"+2",
				"-two",
				" three",
				"+four",
			},
		},
	}
	for _, tc := range tests {
		got := diff.UnifiedLines("before", "after", tc.context)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("UnifiedLines(context=%d)\n got %q\nwant %q", tc.context, got, tc.want)
		}
	}
}

// TestFileDiffGroupsReferenceCases pins _groups, which unified_lines builds on.
//
// Note that a group carries the EQUAL run that precedes its first change, even
// when that run is empty: the reference emits a zero-length equal opcode such
// as ['equal', 1, 1, 1, 1] as the first element of the second group here. That
// is observable through unified_lines, but pinning it directly documents why
// the group boundary is where it is.
func TestFileDiffGroupsReferenceCases(t *testing.T) {
	diff := FileDiffFromText("one\ntwo\nthree\n", "one\n2\nthree\nfour\n")

	got := diff.Groups(0)
	if len(got) != 2 {
		t.Fatalf("Groups(0) has %d groups, want 2: %v", len(got), got)
	}
	want0 := []Opcode{
		op(TagEqual, 1, 1, 1, 1),
		op(TagInsert, 1, 1, 1, 2),
		op(TagDelete, 1, 2, 2, 2),
		op(TagEqual, 2, 2, 2, 2),
	}
	if !reflect.DeepEqual(got[0], want0) {
		t.Errorf("Groups(0)[0]\n got %v\nwant %v", got[0], want0)
	}
	want1 := []Opcode{
		op(TagEqual, 3, 3, 3, 3),
		op(TagInsert, 3, 3, 3, 4),
	}
	if !reflect.DeepEqual(got[1], want1) {
		t.Errorf("Groups(0)[1]\n got %v\nwant %v", got[1], want1)
	}

	// With context 3 the two changes merge into a single group because the
	// equal run between them is shorter than twice the context.
	want3 := []Opcode{
		op(TagEqual, 0, 1, 0, 1),
		op(TagInsert, 1, 1, 1, 2),
		op(TagDelete, 1, 2, 2, 2),
		op(TagEqual, 2, 3, 2, 3),
		op(TagInsert, 3, 3, 3, 4),
	}
	if got3 := diff.Groups(3); !reflect.DeepEqual(got3, [][]Opcode{want3}) {
		t.Errorf("Groups(3)\n got %v\nwant %v", got3, [][]Opcode{want3})
	}
}

// TestFileDiffGroupsOmitEqualOnlyGroups verifies that a diff with no changes
// yields no hunks at all rather than one all-equal group.
func TestFileDiffGroupsOmitEqualOnlyGroups(t *testing.T) {
	diff := FileDiffFromText("a\nb\n", "a\nb\n")
	if got := diff.Groups(3); len(got) != 0 {
		t.Fatalf("Groups(3) = %v, want none for an unchanged file", got)
	}
	if got := diff.UnifiedLines("before", "after", 3); len(got) != 0 {
		t.Fatalf("UnifiedLines = %q, want none for an unchanged file", got)
	}
}

// TestFileDiffFromTextEmptyFile pins the create-file diff, which is what
// edit_file and apply_patch report as "(+N/-0)".
func TestFileDiffFromTextEmptyFile(t *testing.T) {
	diff := FileDiffFromText("", "hello\n")
	if diff.Added != 1 || diff.Deleted != 0 {
		t.Fatalf("counts = (+%d/-%d), want (+1/-0)", diff.Added, diff.Deleted)
	}
	if len(diff.BeforeLines) != 0 {
		t.Fatalf("BeforeLines = %q, want empty", diff.BeforeLines)
	}
	if want := []string{"hello"}; !reflect.DeepEqual(diff.AfterLines, want) {
		t.Fatalf("AfterLines = %q, want %q", diff.AfterLines, want)
	}
}

// TestFileDiffFromTextNormalisesCRLF pins that the diff is computed on
// LF-normalised text, so a pure line-ending change is invisible. The reference
// does this in FileDiff.from_text; callers pass the raw file bytes.
func TestFileDiffFromTextNormalisesCRLF(t *testing.T) {
	crlf := FileDiffFromText("one\r\ntwo\r\n", "one\r\n2\r\n")
	lf := FileDiffFromText("one\ntwo\n", "one\n2\n")
	if crlf.Added != lf.Added || crlf.Deleted != lf.Deleted {
		t.Fatalf("CRLF counts (+%d/-%d) != LF counts (+%d/-%d)",
			crlf.Added, crlf.Deleted, lf.Added, lf.Deleted)
	}
	if !reflect.DeepEqual(crlf.Opcodes, lf.Opcodes) {
		t.Fatalf("CRLF opcodes %v != LF opcodes %v", crlf.Opcodes, lf.Opcodes)
	}
}

// TestFileDiffSplitsOnPythonLineBoundaries pins that line splitting follows
// str.splitlines() rather than a plain "\n" split. \v, \f and \x1c..\x1e are
// line boundaries for Python and are not for bufio.ScanLines.
func TestFileDiffSplitsOnPythonLineBoundaries(t *testing.T) {
	diff := FileDiffFromText("a\vb\n", "a\vb\n")
	if want := []string{"a", "b"}; !reflect.DeepEqual(diff.BeforeLines, want) {
		t.Fatalf("BeforeLines = %q, want %q (\\v must split)", diff.BeforeLines, want)
	}

	diff = FileDiffFromText("a\x0cb\n", "a\x0cb\n")
	if want := []string{"a", "b"}; !reflect.DeepEqual(diff.BeforeLines, want) {
		t.Fatalf("BeforeLines = %q, want %q (\\f must split)", diff.BeforeLines, want)
	}

	diff = FileDiffFromText("a\x1cb\n", "a\x1cb\n")
	if want := []string{"a", "b"}; !reflect.DeepEqual(diff.BeforeLines, want) {
		t.Fatalf("BeforeLines = %q, want %q (\\x1c must split)", diff.BeforeLines, want)
	}

	// \u0085, \u2028 and \u2029 split too, but they are NOT CRLF-normalised
	// away, so they must not be treated as \n by the CRLF pass either.
	diff = FileDiffFromText("a\u2028b\n", "a\u2028b\n")
	if want := []string{"a", "b"}; !reflect.DeepEqual(diff.BeforeLines, want) {
		t.Fatalf("BeforeLines = %q, want %q (U+2028 must split)", diff.BeforeLines, want)
	}
}

// TestLineDiffStatsReferenceCases pins line_diff_stats against values read out
// of the reference. Note that a nil side yields (0, 0) rather than a count: the
// reference returns early when either text is absent.
func TestLineDiffStats(t *testing.T) {
	s := func(v string) *string { return &v }

	tests := []struct {
		name           string
		before, after  *string
		added, deleted int
	}{
		{"both_nil", nil, nil, 0, 0},
		{"before_nil", nil, s("a\n"), 0, 0},
		{"after_nil", s("a\n"), nil, 0, 0},
		{"identical", s("a\n"), s("a\n"), 0, 0},
		{"one_replaced", s("a\n"), s("b\n"), 1, 1},
		{"one_added", s("one\ntwo\n"), s("one\n2\ntwo\n"), 1, 0},
		{"one_removed", s("one\ntwo\n"), s("two\n"), 0, 1},
		{"from_empty", s(""), s("a\n"), 1, 0},
		{"to_empty", s("a\n"), s(""), 0, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			added, deleted := LineDiffStats(tc.before, tc.after)
			if added != tc.added || deleted != tc.deleted {
				t.Fatalf("LineDiffStats = (+%d/-%d), want (+%d/-%d)", added, deleted, tc.added, tc.deleted)
			}
		})
	}
}

func TestTrackedFileEditTools(t *testing.T) {
	want := []string{"apply_patch", "edit_file", "write_file"}
	for _, name := range want {
		if !IsFileEditTool(name) {
			t.Errorf("IsFileEditTool(%q) = false, want true", name)
		}
		if _, ok := TrackedFileEditTools[name]; !ok {
			t.Errorf("TrackedFileEditTools is missing %q", name)
		}
	}
	if len(TrackedFileEditTools) != len(want) {
		t.Errorf("TrackedFileEditTools has %d entries, want %d: %v",
			len(TrackedFileEditTools), len(want), TrackedFileEditTools)
	}
	for _, name := range []string{"read_file", "exec", "", "APPLY_PATCH"} {
		if IsFileEditTool(name) {
			t.Errorf("IsFileEditTool(%q) = true, want false", name)
		}
	}
}

func TestDisplayFileEditPath(t *testing.T) {
	root := t.TempDir()
	ws := root + "/ws"
	inside := ws + "/sub/file.txt"
	outside := root + "/outside.txt"
	mustMkdirAll(t, ws+"/sub")
	mustWriteFile(t, inside, "x")
	mustWriteFile(t, outside, "x")

	tests := []struct {
		name      string
		path      string
		workspace string
		want      string
	}{
		{"inside", inside, ws, "sub/file.txt"},
		{"workspace_itself", ws, ws, "."},
		{"outside", outside, ws, outside},
		{"no_workspace", inside, "", inside},
		{"no_workspace_outside", outside, "", outside},
		{"subdirectory", ws + "/sub", ws, "sub"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DisplayFileEditPath(tc.path, tc.workspace); got != tc.want {
				t.Fatalf("DisplayFileEditPath(%q, %q) = %q, want %q",
					tc.path, tc.workspace, got, tc.want)
			}
		})
	}
}
