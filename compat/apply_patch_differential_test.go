package compat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/filediff"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools/builtin"
)

// This file is deliberately self-contained: its own dumper
// (compat/python/dump_apply_patch.py) and its own loader. It shares no
// declarations with differential_test.go beyond repoRoot, because
// dump_reference.py and differential_test.go are edited concurrently by several
// agents and a read-modify-write race there would destroy work.
//
// Every expectation below comes from executing the frozen reference. Nothing is
// transcribed from documentation or from reading the Python source.

// applyPatchDumperPath is the dumper this file drives.
func applyPatchDumperPath(root string) string {
	return filepath.Join(root, "compat", "python", "dump_apply_patch.py")
}

// loadApplyPatchDump runs the dumper and returns its parsed document.
//
// Numbers are decoded as json.Number so that a case whose arguments contain a
// number is re-marshalled byte-identically on the way into the tool.
func loadApplyPatchDump(t *testing.T) map[string]any {
	t.Helper()
	root := repoRoot(t)
	python := filepath.Join(root, ".tools", "venv", "bin", "python")
	script := applyPatchDumperPath(root)

	if _, err := os.Stat(python); err != nil {
		t.Skipf("SKIP: reference venv not present at %s — differential check not run", python)
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("SKIP: dumper missing at %s", script)
	}

	out, err := runReferenceCommand("apply-patch dumper", []string{python, script}, root, nil)
	if err != nil {
		t.Fatalf("dumper failed: %v", err)
	}

	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("dumper output is not valid JSON: %v", err)
	}
	return doc
}

// dumpList returns doc[key] as a list of objects, failing when it is empty:
// a differential test that compares zero cases is a failure, not a pass.
func dumpList(t *testing.T, doc map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := doc[key].([]any)
	if !ok {
		t.Fatalf("dump section %q is missing or not a list", key)
	}
	if len(raw) == 0 {
		t.Fatalf("dump section %q is empty — nothing would be compared", key)
	}
	out := make([]map[string]any, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("dump section %q entry %d is not an object", key, i)
		}
		out = append(out, m)
	}
	return out
}

func dumpStrings(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("expected a list of strings, got %T", v)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("expected a string element, got %T", item)
		}
		out = append(out, s)
	}
	return out
}

func dumpString(t *testing.T, v any) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("expected a string, got %T", v)
	}
	return s
}

func dumpInt(t *testing.T, v any) int {
	t.Helper()
	n, ok := v.(json.Number)
	if !ok {
		t.Fatalf("expected a number, got %T", v)
	}
	i, err := n.Int64()
	if err != nil {
		t.Fatalf("expected an integer, got %v", n)
	}
	return int(i)
}

func dumpBool(t *testing.T, v any) bool {
	t.Helper()
	b, ok := v.(bool)
	if !ok {
		t.Fatalf("expected a bool, got %T", v)
	}
	return b
}

// opcodesToAny renders a Go opcode sequence the same way the dumper renders
// rapidfuzz's, so the two can be compared with reflect.DeepEqual. The dumper's
// JSON is decoded with UseNumber, so integers must be json.Number here too.
func opcodesToAny(ops []filediff.Opcode) []any {
	out := make([]any, 0, len(ops))
	for _, op := range ops {
		out = append(out, []any{
			string(op.Tag),
			json.Number(strconv.Itoa(op.SrcStart)),
			json.Number(strconv.Itoa(op.SrcEnd)),
			json.Number(strconv.Itoa(op.DestStart)),
			json.Number(strconv.Itoa(op.DestEnd)),
		})
	}
	return out
}

// apEqualStringSlices compares element-wise and treats nil as equal to empty.
// The reference dumps Python lists, and JSON cannot distinguish [] from a
// missing list, while Go distinguishes a nil slice from an empty one. Python's
// str.splitlines() returns [] for "" and the port returns nil; those are the
// same value.
func apEqualStringSlices(a, b []string) bool {
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

// apEqualAnySlices is apEqualStringSlices for the opcode rows the dumper emits.
func apEqualAnySlices(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !reflect.DeepEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Indel.opcodes
// ---------------------------------------------------------------------------

// TestApplyPatchIndelDifferential compares the ported bit-parallel alignment
// against rapidfuzz.distance.Indel.opcodes for the whole adversarial matrix,
// an exhaustive sweep over a 3-letter alphabet, and seeded random sequences.
//
// The FULL opcode sequence is compared, not only added/deleted: the sequence
// feeds FileDiff._groups and unified_lines, so equal counts would not prove the
// alignment matches.
func TestApplyPatchIndelDifferential(t *testing.T) {
	doc := loadApplyPatchDump(t)
	cases := dumpList(t, doc, "indel")

	compared := 0
	for i, c := range cases {
		before := dumpStrings(t, c["before"])
		after := dumpStrings(t, c["after"])
		wantOps := c["opcodes"].([]any)
		wantAdded := dumpInt(t, c["added"])
		wantDeleted := dumpInt(t, c["deleted"])

		ops := filediff.IndelOpcodes(before, after)
		gotOps := opcodesToAny(ops)
		if !apEqualAnySlices(gotOps, wantOps) {
			t.Errorf("case %d: IndelOpcodes(%q, %q)\n got %v\nwant %v",
				i, before, after, gotOps, wantOps)
			break
		}

		// The added/deleted derivation is reproduced independently here so a
		// mismatch in the counts is reported separately from the alignment.
		added, deleted := 0, 0
		for _, op := range ops {
			if op.Tag == filediff.TagReplace || op.Tag == filediff.TagDelete {
				deleted += op.SrcEnd - op.SrcStart
			}
			if op.Tag == filediff.TagReplace || op.Tag == filediff.TagInsert {
				added += op.DestEnd - op.DestStart
			}
		}
		if added != wantAdded || deleted != wantDeleted {
			t.Errorf("case %d: IndelOpcodes(%q, %q) counts = (+%d/-%d), want (+%d/-%d)",
				i, before, after, added, deleted, wantAdded, wantDeleted)
			break
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("compared 0 Indel cases — the differential did not run")
	}
	t.Logf("indel_opcodes: %d cases compared", compared)
}

// ---------------------------------------------------------------------------
// FileDiff
// ---------------------------------------------------------------------------

func TestApplyPatchFileDiffDifferential(t *testing.T) {
	doc := loadApplyPatchDump(t)
	cases := dumpList(t, doc, "file_diff")

	compared := 0
	for i, c := range cases {
		before := dumpString(t, c["before"])
		after := dumpString(t, c["after"])
		diff := filediff.FileDiffFromText(before, after)

		if got, want := diff.BeforeLines, dumpStrings(t, c["before_lines"]); !apEqualStringSlices(got, want) {
			t.Errorf("case %d (%q -> %q): before_lines = %q, want %q", i, before, after, got, want)
			break
		}
		if got, want := diff.AfterLines, dumpStrings(t, c["after_lines"]); !apEqualStringSlices(got, want) {
			t.Errorf("case %d (%q -> %q): after_lines = %q, want %q", i, before, after, got, want)
			break
		}
		if got, want := opcodesToAny(diff.Opcodes), c["opcodes"].([]any); !apEqualAnySlices(got, want) {
			t.Errorf("case %d (%q -> %q): opcodes\n got %v\nwant %v", i, before, after, got, want)
			break
		}
		if got, want := diff.Added, dumpInt(t, c["added"]); got != want {
			t.Errorf("case %d (%q -> %q): added = %d, want %d", i, before, after, got, want)
			break
		}
		if got, want := diff.Deleted, dumpInt(t, c["deleted"]); got != want {
			t.Errorf("case %d (%q -> %q): deleted = %d, want %d", i, before, after, got, want)
			break
		}
		if got, want := diff.Matches(before, after), dumpBool(t, c["matches_self"]); got != want {
			t.Errorf("case %d (%q -> %q): matches = %v, want %v", i, before, after, got, want)
			break
		}
		for _, arm := range []struct {
			key     string
			context int
		}{
			{"groups_0", 0},
			{"groups_1", 1},
			{"groups_3", 3},
		} {
			got := diff.Groups(arm.context)
			wantGroups := c[arm.key].([]any)
			if len(got) != len(wantGroups) {
				t.Errorf("case %d (%q -> %q) groups(%d): %d groups, want %d\n got %v\nwant %v",
					i, before, after, arm.context, len(got), len(wantGroups), got, wantGroups)
				continue
			}
			for gi, wantGroup := range wantGroups {
				gotOps := opcodesToAny(got[gi])
				wantOps := wantGroup.([]any)
				if !apEqualAnySlices(gotOps, wantOps) {
					t.Errorf("case %d (%q -> %q) groups(%d)[%d]\n got %v\nwant %v",
						i, before, after, arm.context, gi, gotOps, wantOps)
				}
			}
		}
		for _, arm := range []struct {
			key     string
			context int
		}{
			{"unified_context_0", 0},
			{"unified_context_1", 1},
			{"unified_context_3", 3},
		} {
			got := diff.UnifiedLines("before", "after", arm.context)
			want := dumpStrings(t, c[arm.key])
			if !apEqualStringSlices(got, want) {
				t.Errorf("case %d (%q -> %q) context %d: unified lines\n got %q\nwant %q",
					i, before, after, arm.context, got, want)
			}
		}
		if t.Failed() {
			break
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("compared 0 FileDiff cases — the differential did not run")
	}
	t.Logf("file_diff: %d cases compared (x4 unified-line arms each)", compared)
}

func TestApplyPatchUnifiedPayloadDifferential(t *testing.T) {
	doc := loadApplyPatchDump(t)
	cases := dumpList(t, doc, "unified_payload")

	compared := 0
	for i, c := range cases {
		name := dumpString(t, c["name"])
		before := dumpString(t, c["before"])
		after := dumpString(t, c["after"])

		opts := filediff.DefaultUnifiedDiffOptions()
		switch name {
		case "defaults":
		case "context_0":
			opts.ContextLines = 0
		case "context_neg":
			opts.ContextLines = -1
		case "context_1":
			opts.ContextLines = 1
		case "max_lines_1":
			opts.MaxLines = 1
		case "max_lines_2":
			opts.MaxLines = 2
		case "max_lines_0":
			opts.MaxLines = 0
		case "max_line_chars_0":
			opts.MaxLineChars = 0
		case "max_line_chars_3":
			opts.MaxLineChars = 3
		case "max_line_chars_1":
			opts.MaxLineChars = 1
		case "labels":
			opts.FromFile = "a.txt"
			opts.ToFile = "a.txt"
		case "none_before", "none_after":
			// No option override; only the nil argument differs.
		default:
			t.Fatalf("case %d: unknown option arm %q", i, name)
		}

		var beforePtr, afterPtr *string
		if name != "none_before" {
			beforePtr = &before
		}
		if name != "none_after" {
			afterPtr = &after
		}

		got := filediff.BuildUnifiedDiffPayload(beforePtr, afterPtr, opts)
		want, _ := c["payload"].(map[string]any)

		if (got == nil) != (want == nil) {
			t.Errorf("case %d (%s): payload = %v, want %v", i, name, got, want)
			break
		}
		if got != nil {
			if got.Format != dumpString(t, want["format"]) {
				t.Errorf("case %d (%s): format = %q", i, name, got.Format)
			}
			if got.Context != dumpInt(t, want["context"]) {
				t.Errorf("case %d (%s): context = %d, want %v", i, name, got.Context, want["context"])
			}
			if got.Truncated != dumpBool(t, want["truncated"]) {
				t.Errorf("case %d (%s): truncated = %v, want %v", i, name, got.Truncated, want["truncated"])
			}
			if got.Text != dumpString(t, want["text"]) {
				t.Errorf("case %d (%s): text mismatch\n got %q\nwant %q", i, name, got.Text, dumpString(t, want["text"]))
			}
		}
		if t.Failed() {
			break
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("compared 0 unified-payload cases — the differential did not run")
	}
	t.Logf("unified_payload: %d cases compared", compared)
}

func TestApplyPatchLineDiffStatsDifferential(t *testing.T) {
	doc := loadApplyPatchDump(t)
	cases := dumpList(t, doc, "line_diff_stats")

	compared := 0
	for i, c := range cases {
		var before, after *string
		if c["before"] != nil {
			s := dumpString(t, c["before"])
			before = &s
		}
		if c["after"] != nil {
			s := dumpString(t, c["after"])
			after = &s
		}
		added, deleted := filediff.LineDiffStats(before, after)
		if added != dumpInt(t, c["added"]) || deleted != dumpInt(t, c["deleted"]) {
			t.Errorf("case %d: line_diff_stats = (+%d/-%d), want (+%v/-%v)",
				i, added, deleted, c["added"], c["deleted"])
			break
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("compared 0 line_diff_stats cases")
	}
	t.Logf("line_diff_stats: %d cases compared", compared)
}

func TestApplyPatchTrackedToolsDifferential(t *testing.T) {
	doc := loadApplyPatchDump(t)
	tracked, ok := doc["tracked"].(map[string]any)
	if !ok {
		t.Fatal("dump section \"tracked\" is missing")
	}
	want := dumpStrings(t, tracked["tools"])

	got := make([]string, 0, len(filediff.TrackedFileEditTools))
	for name := range filediff.TrackedFileEditTools {
		got = append(got, name)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TRACKED_FILE_EDIT_TOOLS = %v, want %v", got, want)
	}

	probes, ok := tracked["is_file_edit_tool"].(map[string]any)
	if !ok || len(probes) == 0 {
		t.Fatal("dump section tracked.is_file_edit_tool is missing or empty")
	}
	compared := 0
	for name, want := range probes {
		if got := filediff.IsFileEditTool(name); got != dumpBool(t, want) {
			t.Errorf("is_file_edit_tool(%q) = %v, want %v", name, got, want)
		}
		compared++
	}
	t.Logf("tracked_file_edit_tools: %d tools, %d probes compared", len(want), compared)
}

func TestApplyPatchDisplayPathDifferential(t *testing.T) {
	doc := loadApplyPatchDump(t)
	cases := dumpList(t, doc, "display_path")

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "display", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "display", "sub", "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "outside.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	compared := 0
	for i, c := range cases {
		path := filepath.Join(root, dumpString(t, c["input_rel"]))
		workspace := ""
		if c["workspace_rel"] != nil {
			workspace = filepath.Join(root, dumpString(t, c["workspace_rel"]))
		}
		want := strings.ReplaceAll(dumpString(t, c["display"]), "<ROOT>", root)
		if got := filediff.DisplayFileEditPath(path, workspace); got != want {
			t.Errorf("case %d: DisplayFileEditPath(%q, %q) = %q, want %q", i, path, workspace, got, want)
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("compared 0 display_file_edit_path cases")
	}
	t.Logf("display_file_edit_path: %d cases compared", compared)
}

// ---------------------------------------------------------------------------
// apply_patch executions
// ---------------------------------------------------------------------------

// setupWorkspace materialises a dumped case's initial files.
func setupWorkspace(t *testing.T, ws string, entries []any) {
	t.Helper()
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("setup entry is not an object: %T", raw)
		}
		target := filepath.Join(ws, filepath.FromSlash(dumpString(t, entry["path"])))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(target), err)
		}
		data, err := base64.StdEncoding.DecodeString(dumpString(t, entry["b64"]))
		if err != nil {
			t.Fatalf("decode setup %s: %v", entry["path"], err)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", target, err)
		}
	}
}

// workspaceFiles lists every file under ws, mirroring the dumper's rglob.
func workspaceFiles(t *testing.T, ws string) []map[string]any {
	t.Helper()
	var out []map[string]any
	err := filepath.WalkDir(ws, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(ws, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, map[string]any{
			"path": filepath.ToSlash(rel),
			"b64":  base64.StdEncoding.EncodeToString(data),
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", ws, err)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["path"].(string) < out[j]["path"].(string)
	})
	return out
}

func compareFiles(t *testing.T, label string, got []map[string]any, want []any) {
	t.Helper()
	wantMaps := make([]map[string]any, 0, len(want))
	for _, raw := range want {
		wantMaps = append(wantMaps, raw.(map[string]any))
	}
	if len(got) != len(wantMaps) {
		t.Errorf("%s: %d files on disk, want %d\n got %v\nwant %v", label, len(got), len(wantMaps), got, wantMaps)
		return
	}
	for i := range got {
		if got[i]["path"] != wantMaps[i]["path"] || got[i]["b64"] != wantMaps[i]["b64"] {
			t.Errorf("%s: file %d = %v, want %v", label, i, got[i], wantMaps[i])
		}
	}
}

// compareFileDiffs checks the FileEditResult.file_diffs equivalent.
//
// The port keys FileDiffs by the RESOLVED absolute path (the reference keys its
// dict by a resolved Path for the same reason), while the dumper reports keys
// relative to the workspace, so the Go keys are relativised here first.
func compareFileDiffs(t *testing.T, ws, label string, got map[string]filediff.FileDiff, want any) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Errorf("%s: FileDiffs = %v, want nil (the reference attached none)", label, got)
		}
		return
	}
	wantMap, ok := want.(map[string]any)
	if !ok {
		t.Fatalf("%s: dumped file_diffs is not an object", label)
	}

	byRel := make(map[string]filediff.FileDiff, len(got))
	for abs, diff := range got {
		rel, err := filepath.Rel(ws, abs)
		if err != nil {
			t.Fatalf("%s: FileDiffs key %q is not under the workspace: %v", label, abs, err)
		}
		byRel[filepath.ToSlash(rel)] = diff
	}

	if len(byRel) != len(wantMap) {
		t.Errorf("%s: %d diffs, want %d\n got %v\nwant %v", label, len(byRel), len(wantMap), byRel, wantMap)
		return
	}
	for rel, raw := range wantMap {
		wantDiff := raw.(map[string]any)
		diff, ok := byRel[rel]
		if !ok {
			t.Errorf("%s: missing diff for %q (have %v)", label, rel, byRel)
			continue
		}
		if diff.Added != dumpInt(t, wantDiff["added"]) || diff.Deleted != dumpInt(t, wantDiff["deleted"]) {
			t.Errorf("%s[%s]: counts = (+%d/-%d), want (+%v/-%v)",
				label, rel, diff.Added, diff.Deleted, wantDiff["added"], wantDiff["deleted"])
		}
		if gotLines, wantLines := diff.BeforeLines, dumpStrings(t, wantDiff["before_lines"]); !apEqualStringSlices(gotLines, wantLines) {
			t.Errorf("%s[%s]: before_lines = %q, want %q", label, rel, gotLines, wantLines)
		}
		if gotLines, wantLines := diff.AfterLines, dumpStrings(t, wantDiff["after_lines"]); !apEqualStringSlices(gotLines, wantLines) {
			t.Errorf("%s[%s]: after_lines = %q, want %q", label, rel, gotLines, wantLines)
		}
		if gotOps, wantOps := opcodesToAny(diff.Opcodes), wantDiff["opcodes"].([]any); !apEqualAnySlices(gotOps, wantOps) {
			t.Errorf("%s[%s]: opcodes\n got %v\nwant %v", label, rel, gotOps, wantOps)
		}
	}
}

// TestApplyPatchToolDifferential drives the real ApplyPatchTool in Python and
// the ported ApplyPatch in Go over the same workspace states and compares the
// returned TEXT and the resulting FILE BYTES.
//
// The text carries the "(+N/-M)" suffix, so this transitively exercises the
// Indel port as well as every message path of the tool.
func TestApplyPatchToolDifferential(t *testing.T) {
	doc := loadApplyPatchDump(t)
	cases := dumpList(t, doc, "apply_patch")

	compared := 0
	for _, c := range cases {
		name := dumpString(t, c["name"])
		ws := t.TempDir()
		setupWorkspace(t, ws, c["setup"].([]any))

		args := map[string]any{"dry_run": dumpBool(t, c["dry_run"])}
		if !dumpBool(t, c["omit_edits"]) {
			args["edits"] = c["edits"]
		}
		rawArgs, err := json.Marshal(args)
		if err != nil {
			t.Fatalf("%s: marshal args: %v", name, err)
		}

		tool := builtin.NewApplyPatch(builtin.PathPolicy{Workspace: ws, AllowedDir: ws})
		res, execErr := tool.Execute(context.Background(), rawArgs)
		if execErr != nil {
			t.Errorf("%s: Execute returned a Go error: %v", name, execErr)
			continue
		}

		if dumpBool(t, c["compare_text"]) {
			want := strings.ReplaceAll(dumpString(t, c["text"]), "<WS>", ws)
			if res.Content != want {
				t.Errorf("%s: text mismatch\n got %q\nwant %q", name, res.Content, want)
			}
		} else {
			// The failure that triggers the rollback is an OS error, whose
			// wording is Go's rather than Python's; only the shape is checked
			// here, and the file state below proves the rollback happened.
			if !res.IsError || !strings.HasPrefix(res.Content, "Error applying patch: ") {
				t.Errorf("%s: want an error result, got %q (isError=%v)", name, res.Content, res.IsError)
			}
		}
		if res.IsError != dumpBool(t, c["is_error"]) {
			t.Errorf("%s: isError = %v, want %v (text %q)", name, res.IsError, c["is_error"], res.Content)
		}

		compareFiles(t, name, workspaceFiles(t, ws), c["files"].([]any))

		// The reference attaches file_diffs only on a successful, non-dry-run
		// FileEditResult; a dry run returns a plain str.
		wantDiffs := c["file_diffs"]
		if dumpString(t, c["result_type"]) != "FileEditResult" {
			wantDiffs = nil
		}
		compareFileDiffs(t, ws, name, res.FileDiffs, wantDiffs)

		compared++
	}
	if compared == 0 {
		t.Fatal("compared 0 apply_patch executions — the differential did not run")
	}
	t.Logf("apply_patch: %d executions compared (text + file bytes + diffs)", compared)
}

// TestApplyPatchEditFileDifferential covers Part C: the "(+N/-M)" suffix of the
// edit_file summary, which this port omitted while FileDiff was unported.
func TestApplyPatchEditFileDifferential(t *testing.T) {
	doc := loadApplyPatchDump(t)
	cases := dumpList(t, doc, "edit_file")

	compared := 0
	for _, c := range cases {
		name := dumpString(t, c["name"])
		ws := t.TempDir()
		setupWorkspace(t, ws, c["setup"].([]any))

		rawArgs, err := json.Marshal(c["args"])
		if err != nil {
			t.Fatalf("%s: marshal args: %v", name, err)
		}
		tool := builtin.NewEditFile(builtin.PathPolicy{Workspace: ws, AllowedDir: ws})
		res, execErr := tool.Execute(context.Background(), rawArgs)
		if execErr != nil {
			t.Errorf("%s: Execute returned a Go error: %v", name, execErr)
			continue
		}

		want := strings.ReplaceAll(dumpString(t, c["text"]), "<WS>", ws)
		if res.Content != want {
			t.Errorf("%s: text mismatch\n got %q\nwant %q", name, res.Content, want)
		}
		if res.IsError != dumpBool(t, c["is_error"]) {
			t.Errorf("%s: isError = %v, want %v", name, res.IsError, c["is_error"])
		}
		compareFiles(t, name, workspaceFiles(t, ws), c["files"].([]any))
		compareFileDiffs(t, ws, name, res.FileDiffs, c["file_diffs"])

		compared++
	}
	if compared == 0 {
		t.Fatal("compared 0 edit_file executions — the differential did not run")
	}
	t.Logf("edit_file: %d executions compared (text + file bytes + diffs)", compared)
}
