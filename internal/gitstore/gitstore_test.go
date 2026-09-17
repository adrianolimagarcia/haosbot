package gitstore

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// These tests pin the behaviours that are easy to get wrong: the degradation
// paths, the exact summary text, and the pieces of Python semantics the port
// reimplements (str.splitlines, errors="replace" decoding, difflib).
//
// The reference is dulwich-based, so nothing here may depend on the git binary;
// TestWorksWithoutGitBinary enforces that.

// withoutGit empties PATH and the git environment for the duration of a test.
func withoutGit(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", "")
	for _, name := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_CONFIG_GLOBAL",
		"GIT_CONFIG_SYSTEM", "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL",
	} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

func TestWorksWithoutGitBinary(t *testing.T) {
	withoutGit(t)
	ws := t.TempDir()
	g := New(ws, tracked)
	if created, err := g.Init(); err != nil || !created {
		t.Fatalf("Init() = %v, %v", created, err)
	}
	writeFile(t, filepath.Join(ws, "SOUL.md"), "no git here\n")
	sha, committed, err := g.AutoCommit("dream: no git")
	if err != nil || !committed {
		t.Fatalf("AutoCommit() = %q, %v, %v", sha, committed, err)
	}
	entries, err := g.Log(20, nil)
	if err != nil || len(entries) != 2 {
		t.Fatalf("Log() = %v, %v", entries, err)
	}
	diff, err := g.DiffCommits(entries[1].SHA, entries[0].SHA)
	if err != nil || !strings.Contains(diff, "+no git here") {
		t.Fatalf("DiffCommits() = %q, %v", diff, err)
	}
	summary, err := g.SummarizeWorkingTree([]string{"SOUL.md"})
	if err != nil || summary != "" {
		t.Fatalf("SummarizeWorkingTree() = %q, %v", summary, err)
	}
	if _, _, _, err := g.ShowCommitDiff(sha, 20, nil); err != nil {
		t.Fatalf("ShowCommitDiff(): %v", err)
	}
	if _, _, err := g.Revert(sha, nil); err != nil {
		t.Fatalf("Revert(): %v", err)
	}
}

func TestUninitializedStoreIsNoOp(t *testing.T) {
	ws := t.TempDir()
	g := New(ws, tracked)

	if g.IsInitialized() {
		t.Error("IsInitialized() = true on a workspace with no .git")
	}
	if sha, committed, err := g.AutoCommit("x"); err != nil || committed || sha != "" {
		t.Errorf("AutoCommit() = %q, %v, %v; want no commit", sha, committed, err)
	}
	if entries, err := g.Log(20, nil); err != nil || len(entries) != 0 {
		t.Errorf("Log() = %v, %v", entries, err)
	}
	if diff, err := g.DiffCommits("a", "b"); err != nil || diff != "" {
		t.Errorf("DiffCommits() = %q, %v", diff, err)
	}
	if summary, err := g.SummarizeWorkingTree([]string{"SOUL.md"}); err != nil || summary != "" {
		t.Errorf("SummarizeWorkingTree() = %q, %v", summary, err)
	}
	if _, _, ok, err := g.ShowCommitDiff("a", 20, nil); err != nil || ok {
		t.Errorf("ShowCommitDiff() ok=%v err=%v", ok, err)
	}
	if sha, reverted, err := g.Revert("a", nil); err != nil || reverted || sha != "" {
		t.Errorf("Revert() = %q, %v, %v", sha, reverted, err)
	}
	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("an uninitialized store wrote %v", names)
	}
}

func TestInitDeclinesInsideExistingRepository(t *testing.T) {
	parent := t.TempDir()
	if err := os.Mkdir(filepath.Join(parent, ".git"), 0o777); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(parent, "inner")
	if err := os.Mkdir(ws, 0o777); err != nil {
		t.Fatal(err)
	}
	g := New(ws, tracked)
	created, err := g.Init()
	if err != nil || created {
		t.Fatalf("Init() = %v, %v; want false, nil", created, err)
	}
	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("Init() wrote %d entries inside an existing repository", len(entries))
	}
}

func TestInitFailsWhenWorkspaceIsAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspace-file")
	writeFile(t, path, "not a directory\n")

	_, err := New(path, []string{"SOUL.md"}).Init()
	var gsErr *GitStoreError
	if !errors.As(err, &gsErr) {
		t.Fatalf("Init() error = %v (%T), want *GitStoreError", err, err)
	}
	want := "Git store init failed for " + path
	if gsErr.Error() != want {
		t.Errorf("Init() error = %q, want %q", gsErr.Error(), want)
	}
}

func TestInitMergesExistingGitignore(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, ".gitignore"), "*.log\n!SOUL.md\n")

	g := New(ws, tracked)
	if _, err := g.Init(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(ws, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	// The reference keeps the existing text, drops entries that are already
	// present (SOUL.md) and appends the rest.
	want := "*.log\n!SOUL.md\n/*\n!memory/\n!USER.md\n!memory/MEMORY.md\n!memory/.dream_cursor\n!.gitignore\n"
	if string(data) != want {
		t.Errorf(".gitignore =\n%q\nwant\n%q", data, want)
	}
}

func TestInitIsIdempotent(t *testing.T) {
	ws := t.TempDir()
	g := New(ws, tracked)
	if created, err := g.Init(); err != nil || !created {
		t.Fatalf("first Init() = %v, %v", created, err)
	}
	if created, err := g.Init(); err != nil || created {
		t.Fatalf("second Init() = %v, %v; want false", created, err)
	}
	entries, err := g.Log(20, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("second Init() created a commit: %v", entries)
	}
	if entries[0].Message != "init: nanobot memory store" {
		t.Errorf("init message = %q", entries[0].Message)
	}
}

func TestInitCreatesTrackedFilesAndGitignore(t *testing.T) {
	ws := t.TempDir()
	g := New(ws, tracked)
	if _, err := g.Init(); err != nil {
		t.Fatal(err)
	}
	for _, rel := range tracked {
		if _, err := os.Stat(filepath.Join(ws, filepath.FromSlash(rel))); err != nil {
			t.Errorf("tracked file %s was not created: %v", rel, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(ws, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	want := "/*\n!memory/\n!SOUL.md\n!USER.md\n!memory/MEMORY.md\n!memory/.dream_cursor\n!.gitignore\n"
	if string(data) != want {
		t.Errorf(".gitignore = %q, want %q", data, want)
	}
}

func TestAutoCommitLifecycle(t *testing.T) {
	ws := t.TempDir()
	g := New(ws, tracked)
	if _, err := g.Init(); err != nil {
		t.Fatal(err)
	}

	// Untracked files are ignored, so nothing is committed.
	writeFile(t, filepath.Join(ws, "notes.txt"), "ignored\n")
	if _, committed, err := g.AutoCommit("dream: nothing"); err != nil || committed {
		t.Fatalf("AutoCommit() with only ignored changes = %v, %v", committed, err)
	}

	writeFile(t, filepath.Join(ws, "SOUL.md"), "first\n")
	sha, committed, err := g.AutoCommit("dream: first")
	if err != nil || !committed || len(sha) != 8 {
		t.Fatalf("AutoCommit() = %q, %v, %v", sha, committed, err)
	}
	// Reverting the working tree to the committed content commits nothing.
	writeFile(t, filepath.Join(ws, "SOUL.md"), "first\n")
	if _, committed, err := g.AutoCommit("dream: same"); err != nil || committed {
		t.Fatalf("AutoCommit() on unchanged content = %v, %v", committed, err)
	}

	// Removing a tracked file is staged as a deletion.
	if err := os.Remove(filepath.Join(ws, "USER.md")); err != nil {
		t.Fatal(err)
	}
	sha2, committed, err := g.AutoCommit("dream: delete")
	if err != nil || !committed {
		t.Fatalf("AutoCommit() after deleting a tracked file = %q, %v, %v", sha2, committed, err)
	}
	diff, err := g.DiffCommits(sha, sha2)
	if err != nil {
		t.Fatal(err)
	}
	// Deleting an empty file produces a header with no hunks at all, which is
	// what the reference emits too.
	if !strings.Contains(diff, "deleted file mode 100644") || !strings.Contains(diff, "index e69de29..0000000") {
		t.Errorf("deletion diff =\n%s", diff)
	}
}

func TestSummarizeWorkingTree(t *testing.T) {
	ws := t.TempDir()
	g := New(ws, tracked)
	if _, err := g.Init(); err != nil {
		t.Fatal(err)
	}
	paths := []string{"SOUL.md", "USER.md", "memory/MEMORY.md"}

	if got, err := g.SummarizeWorkingTree(paths); err != nil || got != "" {
		t.Fatalf("clean summary = %q, %v", got, err)
	}

	writeFile(t, filepath.Join(ws, "SOUL.md"), "hello\nworld\n")
	got, err := g.SummarizeWorkingTree(paths)
	if err != nil {
		t.Fatal(err)
	}
	want := "SOUL.md: +2 -0\n" +
		"1 file changed, 2 insertions(+), 0 deletions(-)\n\n" +
		"```diff\n--- SOUL.md\n+++ SOUL.md\n@@ -0,0 +1,2 @@\n+hello\n+world\n```"
	if got != want {
		t.Errorf("summary =\n%q\nwant\n%q", got, want)
	}

	// A duplicate path is reported twice; the summary counts it twice as well.
	dup, err := g.SummarizeWorkingTree([]string{"SOUL.md", "SOUL.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dup, "SOUL.md: +2 -0\nSOUL.md: +2 -0\n2 files changed, 4 insertions(+), 0 deletions(-)") {
		t.Errorf("duplicate summary = %q", dup)
	}

	// A "./" prefix is kept in the label but resolved away for the lookup.
	dotted, err := g.SummarizeWorkingTree([]string{"./SOUL.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dotted, "./SOUL.md: +2 -0") || !strings.Contains(dotted, "--- ./SOUL.md") {
		t.Errorf("dotted summary = %q", dotted)
	}

	// The comparison is against HEAD, so commit the base content first.
	if _, _, err := g.AutoCommit("dream: base"); err != nil {
		t.Fatal(err)
	}

	// CRLF content equal to the committed LF content is not a change.
	writeFile(t, filepath.Join(ws, "SOUL.md"), "hello\r\nworld\r\n")
	if got, err := g.SummarizeWorkingTree([]string{"SOUL.md"}); err != nil || got != "" {
		t.Errorf("CRLF summary = %q, %v; want empty", got, err)
	}

	// A lone CR is a line break for str.splitlines, so the lines match and the
	// change is counted with no diff block.
	writeFile(t, filepath.Join(ws, "SOUL.md"), "hello\rworld\r")
	got, err = g.SummarizeWorkingTree([]string{"SOUL.md"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "SOUL.md: +0 -0\n1 file changed, 0 insertions(+), 0 deletions(-)" {
		t.Errorf("CR-only summary = %q", got)
	}

	// A file without a trailing newline differs but produces no +/- lines.
	writeFile(t, filepath.Join(ws, "SOUL.md"), "hello\nworld")
	got, err = g.SummarizeWorkingTree([]string{"SOUL.md"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "SOUL.md: +0 -0\n1 file changed, 0 insertions(+), 0 deletions(-)" {
		t.Errorf("no-trailing-newline summary = %q", got)
	}
}

func TestSummarizeWorkingTreeDegradations(t *testing.T) {
	ws := t.TempDir()
	g := New(ws, tracked)
	if _, err := g.Init(); err != nil {
		t.Fatal(err)
	}

	// A non-UTF-8 working-tree file is reported without a diff.
	if err := os.WriteFile(filepath.Join(ws, "USER.md"), []byte("\xff\xfe\x00binary\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := g.SummarizeWorkingTree([]string{"SOUL.md", "USER.md"})
	if err != nil {
		t.Fatal(err)
	}
	want := "USER.md: binary or non-UTF-8 file changed\n1 file changed, 0 insertions(+), 0 deletions(-)"
	if got != want {
		t.Errorf("binary summary = %q, want %q", got, want)
	}
	writeFile(t, filepath.Join(ws, "USER.md"), "")

	// A directory where a file is expected fails the whole summary.
	if err := os.Remove(filepath.Join(ws, "memory", "MEMORY.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ws, "memory", "MEMORY.md"), 0o777); err != nil {
		t.Fatal(err)
	}
	_, err = g.SummarizeWorkingTree([]string{"memory/MEMORY.md"})
	var gsErr *GitStoreError
	if !errors.As(err, &gsErr) || gsErr.Error() != "Git working-tree summary failed" {
		t.Errorf("directory summary error = %v, want GitStoreError", err)
	}
	if err := os.Remove(filepath.Join(ws, "memory", "MEMORY.md")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ws, "memory", "MEMORY.md"), "")

	// An empty path list and a missing file are both "no change".
	if got, err := g.SummarizeWorkingTree(nil); err != nil || got != "" {
		t.Errorf("empty summary = %q, %v", got, err)
	}
	if got, err := g.SummarizeWorkingTree([]string{"memory/MEMORY.md"}); err != nil || got != "" {
		t.Errorf("unchanged file summary = %q, %v", got, err)
	}
}

func TestSummarizeTruncatesLongDiffs(t *testing.T) {
	ws := t.TempDir()
	g := New(ws, tracked)
	if _, err := g.Init(); err != nil {
		t.Fatal(err)
	}
	var big strings.Builder
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&big, "line %d\n", i)
	}
	writeFile(t, filepath.Join(ws, "SOUL.md"), big.String())

	got, err := g.SummarizeWorkingTree([]string{"SOUL.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "...[diff truncated]\n```") {
		t.Fatalf("summary does not end with the truncation marker: %q", got[len(got)-80:])
	}
	// The diff text is cut at exactly 6000 characters, and the marker is added
	// after the cut.
	head, diffText, ok := strings.Cut(got, "```diff\n")
	if !ok {
		t.Fatal("summary has no diff block")
	}
	if len(head) == 0 {
		t.Error("summary has no header")
	}
	body := strings.TrimSuffix(diffText, "\n```")
	if len(body) != 6000+len("\n...[diff truncated]") {
		t.Errorf("diff block is %d bytes, want %d", len(body), 6000+len("\n...[diff truncated]"))
	}
}

func TestLogLimitsAndFilters(t *testing.T) {
	ws := t.TempDir()
	g := New(ws, tracked)
	if _, err := g.Init(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ws, "SOUL.md"), "one\n")
	if _, _, err := g.AutoCommit("dream: one"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ws, "SOUL.md"), "two\n")
	if _, _, err := g.AutoCommit("dream: two"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ws, "SOUL.md"), "three\n")
	if _, _, err := g.AutoCommit("dream: three"); err != nil {
		t.Fatal(err)
	}

	subjects := func(entries []CommitInfo) []string {
		out := make([]string, 0, len(entries))
		for _, e := range entries {
			out = append(out, e.Subject())
		}
		return out
	}

	all, err := g.Log(20, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"dream: three", "dream: two", "dream: one", "init: nanobot memory store"}
	if !reflect.DeepEqual(subjects(all), want) {
		t.Errorf("Log() = %v, want %v", subjects(all), want)
	}
	// Timestamps are "%Y-%m-%d %H:%M" in local time.
	for _, e := range all {
		if !timestampShape(e.Timestamp) {
			t.Errorf("timestamp %q is not in the reference format", e.Timestamp)
		}
		if len(e.SHA) != 8 {
			t.Errorf("short sha %q is not 8 characters", e.SHA)
		}
	}

	if entries, err := g.Log(0, nil); err != nil || len(entries) != 0 {
		t.Errorf("Log(0) = %v, %v; want empty", entries, err)
	}
	if entries, err := g.Log(-1, nil); err != nil || len(entries) != 0 {
		t.Errorf("Log(-1) = %v, %v; want empty", entries, err)
	}
	if entries, err := g.Log(2, nil); err != nil || len(entries) != 2 {
		t.Errorf("Log(2) returned %d entries, %v", len(entries), err)
	}

	prefix := "dream:"
	filtered, err := g.Log(20, &prefix)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"dream: three", "dream: two", "dream: one"}; !reflect.DeepEqual(subjects(filtered), want) {
		t.Errorf("filtered Log() = %v, want %v", subjects(filtered), want)
	}
	// The filter is applied before the limit: max_entries counts matches.
	if entries, err := g.Log(1, &prefix); err != nil || len(entries) != 1 || entries[0].Subject() != "dream: three" {
		t.Errorf("Log(1, prefix) = %v, %v", subjects(entries), err)
	}
	none := "zzz"
	if entries, err := g.Log(20, &none); err != nil || len(entries) != 0 {
		t.Errorf("Log(20, no-match) = %v, %v", entries, err)
	}
	// An empty prefix is not the same as no prefix: it matches every message.
	empty := ""
	if entries, err := g.Log(20, &empty); err != nil || len(entries) != len(all) {
		t.Errorf("Log(20, empty prefix) = %d entries, %v", len(entries), err)
	}
}

func TestRevertBehaviour(t *testing.T) {
	ws := t.TempDir()
	g := New(ws, tracked)
	if _, err := g.Init(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ws, "SOUL.md"), "content\n")
	sha, _, err := g.AutoCommit("dream: add content")
	if err != nil {
		t.Fatal(err)
	}

	revertSHA, reverted, err := g.Revert(sha, nil)
	if err != nil || !reverted || len(revertSHA) != 8 {
		t.Fatalf("Revert() = %q, %v, %v", revertSHA, reverted, err)
	}
	data, err := os.ReadFile(filepath.Join(ws, "SOUL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "" {
		t.Errorf("after revert SOUL.md = %q, want empty", data)
	}
	entries, err := g.Log(20, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The revert message embeds the argument exactly as it was passed.
	if want := "revert: undo " + sha; entries[0].Message != want {
		t.Errorf("revert message = %q, want %q", entries[0].Message, want)
	}
	if got, err := g.SummarizeWorkingTree([]string{"SOUL.md"}); err != nil || got != "" {
		t.Errorf("summary after revert = %q, %v", got, err)
	}

	// The initial commit has no parent, so there is nothing to restore.
	initSHA := entries[len(entries)-1].SHA
	if sha, reverted, err := g.Revert(initSHA, nil); err != nil || reverted || sha != "" {
		t.Errorf("Revert(init) = %q, %v, %v; want no-op", sha, reverted, err)
	}
	// An unknown sha and a message filter that does not match are no-ops too.
	if sha, reverted, err := g.Revert("ffffffff", nil); err != nil || reverted || sha != "" {
		t.Errorf("Revert(unknown) = %q, %v, %v", sha, reverted, err)
	}
	noMatch := "zzz"
	if sha, reverted, err := g.Revert(sha, &noMatch); err != nil || reverted || sha != "" {
		t.Errorf("Revert(prefix no match) = %q, %v, %v", sha, reverted, err)
	}
	// The message filter matches against the subject text. Reverting the same
	// commit twice is a no-op (nothing changes), so this uses a fresh change.
	writeFile(t, filepath.Join(ws, "SOUL.md"), "second change\n")
	secondSHA, _, err := g.AutoCommit("dream: second change")
	if err != nil {
		t.Fatal(err)
	}
	match := "dream:"
	prefixSHA, reverted, err := g.Revert(secondSHA, &match)
	if err != nil || !reverted || len(prefixSHA) != 8 {
		t.Errorf("Revert(prefix match) = %q, %v, %v", prefixSHA, reverted, err)
	}
	data, err = os.ReadFile(filepath.Join(ws, "SOUL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "" {
		t.Errorf("after prefix revert SOUL.md = %q, want empty", data)
	}
}

func TestCorruptIndexIsReported(t *testing.T) {
	ws := t.TempDir()
	g := New(ws, tracked)
	if _, err := g.Init(); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(ws, ".git", "index")
	good, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, append([]byte("GARBAGE"), good[7:]...), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ws, "SOUL.md"), "changed\n")

	_, _, err = g.AutoCommit("dream: corrupt")
	var gsErr *GitStoreError
	if !errors.As(err, &gsErr) {
		t.Fatalf("AutoCommit() error = %v (%T), want *GitStoreError", err, err)
	}
	// The reference embeds the commit message, not the exception text.
	if want := "Git auto-commit failed: dream: corrupt"; gsErr.Error() != want {
		t.Errorf("AutoCommit() error = %q, want %q", gsErr.Error(), want)
	}
	// log and summarize do not read the index, so they still work.
	if _, err := g.Log(20, nil); err != nil {
		t.Errorf("Log() with a corrupt index: %v", err)
	}
	if _, err := g.SummarizeWorkingTree([]string{"SOUL.md"}); err != nil {
		t.Errorf("SummarizeWorkingTree() with a corrupt index: %v", err)
	}
}

func TestGarbageHeadIsReported(t *testing.T) {
	ws := t.TempDir()
	g := New(ws, tracked)
	if _, err := g.Init(); err != nil {
		t.Fatal(err)
	}
	headPath := filepath.Join(ws, ".git", "HEAD")
	if err := os.WriteFile(headPath, []byte("garbage-not-a-ref\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := g.Log(20, nil)
	var gsErr *GitStoreError
	if !errors.As(err, &gsErr) || gsErr.Error() != "Git log failed" {
		t.Errorf("Log() error = %v, want GitStoreError(\"Git log failed\")", err)
	}
	if _, _, err := g.AutoCommit("dream: bad head"); err == nil {
		t.Error("AutoCommit() with a garbage HEAD did not fail")
	}
	if _, _, _, err := g.ShowCommitDiff("aaaa", 20, nil); err == nil {
		t.Error("ShowCommitDiff() with a garbage HEAD did not fail")
	}
}

func TestMissingObjectIsReported(t *testing.T) {
	ws := t.TempDir()
	g := New(ws, tracked)
	if _, err := g.Init(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ws, "SOUL.md"), "hello\n")
	if _, _, err := g.AutoCommit("dream: present"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ws, "SOUL.md"), "hello again\n")

	// Find and remove the committed blob.
	repo := openRepository(ws)
	tree, ok, err := repo.headTree()
	if err != nil || !ok {
		t.Fatalf("headTree() ok=%v err=%v", ok, err)
	}
	blobID, ok, err := readBlobID(repo, tree, "SOUL.md")
	if err != nil || !ok {
		t.Fatalf("readBlobID() ok=%v err=%v", ok, err)
	}
	if err := os.Remove(repo.store.loosePath(blobID)); err != nil {
		t.Fatal(err)
	}

	_, err = g.SummarizeWorkingTree([]string{"SOUL.md"})
	var gsErr *GitStoreError
	if !errors.As(err, &gsErr) || gsErr.Error() != "Git working-tree summary failed" {
		t.Errorf("summary error = %v", err)
	}
	// The log only walks commits, so it still works.
	if entries, err := g.Log(20, nil); err != nil || len(entries) != 2 {
		t.Errorf("Log() = %v, %v", entries, err)
	}
}

// readBlobID resolves a path to a blob id in a tree.
func readBlobID(repo *repository, tree objectID, path string) (objectID, bool, error) {
	parts := pythonPathParts(path)
	current, err := repo.store.get(tree)
	if err != nil {
		return objectID{}, false, err
	}
	var last objectID
	for _, part := range parts {
		entries, err := parseTree(current.data)
		if err != nil {
			return objectID{}, false, err
		}
		var found *treeEntry
		for i := range entries {
			if entries[i].name == part {
				found = &entries[i]
			}
		}
		if found == nil {
			return objectID{}, false, nil
		}
		last = found.id
		current, err = repo.store.get(found.id)
		if err != nil {
			return objectID{}, false, err
		}
	}
	return last, true, nil
}

// timestampShape reports whether a value matches the reference's
// "%Y-%m-%d %H:%M" timestamp format.
func timestampShape(s string) bool {
	if len(s) != 16 || s[4] != '-' || s[7] != '-' || s[10] != ' ' || s[13] != ':' {
		return false
	}
	for i, r := range s {
		if i == 4 || i == 7 || i == 10 || i == 13 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func TestCommitInfoSubjectAndFormat(t *testing.T) {
	cases := []struct {
		message string
		subject string
	}{
		{"single line", "single line"},
		{"first line\nsecond line", "first line"},
		{"", "(no message)"},
		{"\n", ""}, // "\n".splitlines() == [""], and the reference returns lines[0]
		{"\r\n", ""},
		{"  padded  ", "  padded  "},
		{"line\r\nsecond", "line"},
	}
	for _, tc := range cases {
		info := CommitInfo{SHA: "abcd1234", Message: tc.message, Timestamp: "2024-01-02 03:04"}
		if got := info.Subject(); got != tc.subject {
			t.Errorf("Subject(%q) = %q, want %q", tc.message, got, tc.subject)
		}
	}

	info := CommitInfo{SHA: "abcd1234", Message: "subject", Timestamp: "2024-01-02 03:04"}
	if got, want := info.Format(""), "## subject\n`abcd1234` — 2024-01-02 03:04\n\n(no file changes)"; got != want {
		t.Errorf("Format(\"\") = %q, want %q", got, want)
	}
	if got, want := info.Format("diff text"), "## subject\n`abcd1234` — 2024-01-02 03:04\n\n```diff\ndiff text\n```"; got != want {
		t.Errorf("Format(diff) = %q, want %q", got, want)
	}
}

func TestShowCommitDiff(t *testing.T) {
	ws := t.TempDir()
	g := New(ws, tracked)
	if _, err := g.Init(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ws, "SOUL.md"), "shown\n")
	sha, _, err := g.AutoCommit("dream: shown")
	if err != nil {
		t.Fatal(err)
	}

	info, diff, ok, err := g.ShowCommitDiff(sha, 20, nil)
	if err != nil || !ok {
		t.Fatalf("ShowCommitDiff() ok=%v err=%v", ok, err)
	}
	if info.SHA != sha || info.Subject() != "dream: shown" {
		t.Errorf("ShowCommitDiff() info = %+v", info)
	}
	if !strings.Contains(diff, "+shown") {
		t.Errorf("ShowCommitDiff() diff = %q", diff)
	}

	if _, _, ok, err := g.ShowCommitDiff("ffffffff", 20, nil); err != nil || ok {
		t.Errorf("ShowCommitDiff(unknown) ok=%v err=%v", ok, err)
	}
	noMatch := "zzz"
	if _, _, ok, err := g.ShowCommitDiff(sha, 20, &noMatch); err != nil || ok {
		t.Errorf("ShowCommitDiff(no match) ok=%v err=%v", ok, err)
	}
	// The initial commit has no parent: the diff is empty, not an error.
	entries, err := g.Log(20, nil)
	if err != nil {
		t.Fatal(err)
	}
	info, diff, ok, err = g.ShowCommitDiff(entries[len(entries)-1].SHA, 20, nil)
	if err != nil || !ok || diff != "" {
		t.Errorf("ShowCommitDiff(init) = %+v, %q, %v, %v", info, diff, ok, err)
	}
}

// ---------------------------------------------------------------------------
// Python semantics
// ---------------------------------------------------------------------------

func TestSplitLinesMatchesPython(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"\n", []string{""}},
		{"a\nb\n", []string{"a", "b"}},
		{"a\nb", []string{"a", "b"}},
		{"a\r\nb\r", []string{"a", "b"}},
		{"a\vb\fc", []string{"a", "b", "c"}},
		{"a\x1cb\x1dc\x1ed", []string{"a", "b", "c", "d"}},
		{"a\u0085b", []string{"a", "b"}},
		{"a\u2028b\u2029c", []string{"a", "b", "c"}},
		{"\n\n", []string{"", ""}},
		{"a\n\n", []string{"a", ""}},
	}
	for _, tc := range cases {
		got := splitLinesStr(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitLinesStr(%q) = %q, want %q", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitLinesStr(%q) = %q, want %q", tc.in, got, tc.want)
				break
			}
		}
	}

	// bytes.splitlines does NOT break on NEL, U+2028 or U+2029.
	if got := splitLinesBytes("a\u0085b"); len(got) != 1 {
		t.Errorf("splitLinesBytes(NEL) = %q, want a single line", got)
	}
	if got := splitLinesBytes("a\u2028b"); len(got) != 1 {
		t.Errorf("splitLinesBytes(U+2028) = %q, want a single line", got)
	}
	if got := splitLinesBytes("a\vb\fc"); len(got) != 3 {
		t.Errorf("splitLinesBytes(\\v\\f) = %q, want three lines", got)
	}
}

func TestDecodeUTF8ReplaceMatchesPython(t *testing.T) {
	cases := []struct {
		in   []byte
		want string
	}{
		{[]byte("plain"), "plain"},
		{[]byte("caf\xc3\xa9"), "café"},
		{[]byte("\xff\xfe"), "\ufffd\ufffd"},
		// A truncated 4-byte sequence is ONE replacement, not one per byte.
		{[]byte("\xf0\x9f\x98"), "\ufffd"},
		{[]byte("\xf0\x9f\x98ok"), "\ufffdok"},
		// A truncated 3-byte sequence, then valid text.
		{[]byte("\xe2\x82x"), "\ufffdx"},
		// A lone continuation byte.
		{[]byte("\x80\x80"), "\ufffd\ufffd"},
		// Overlong encoding of "/" is invalid.
		{[]byte("\xc0\xaf"), "\ufffd\ufffd"},
		// A surrogate half is invalid.
		{[]byte("\xed\xa0\x80"), "\ufffd\ufffd\ufffd"},
		// A valid 4-byte emoji.
		{[]byte("\xf0\x9f\x98\x80"), "\U0001f600"},
	}
	for _, tc := range cases {
		if got := decodeUTF8Replace(tc.in); got != tc.want {
			t.Errorf("decodeUTF8Replace(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPythonStripAndPaths(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  x  ", "x"},
		{"\t\n x \r\n", "x"},
		{"\x1cx\x1d", "x"}, // str.strip() also strips U+001C..U+001F
		{"\u00a0x", "x"},   // U+00A0 is whitespace for str.strip()
		{"", ""},
		{"\u2028x\u2029", "x"},
	}
	for _, tc := range cases {
		if got := pythonStrip(tc.in); got != tc.want {
			t.Errorf("pythonStrip(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	if got, want := pythonPathJoin("/ws", "a/b.md"), "/ws/a/b.md"; got != want {
		t.Errorf("pythonPathJoin = %q, want %q", got, want)
	}
	// An absolute path discards the base, exactly like pathlib.
	if got, want := pythonPathJoin("/ws", "/etc/hostname"), "/etc/hostname"; got != want {
		t.Errorf("pythonPathJoin(absolute) = %q, want %q", got, want)
	}
	if got, want := pythonPathJoin("/ws", ""), "/ws"; got != want {
		t.Errorf("pythonPathJoin(empty) = %q, want %q", got, want)
	}

	parts := pythonPathParts("./memory/MEMORY.md")
	if !reflect.DeepEqual(parts, []string{"memory", "MEMORY.md"}) {
		t.Errorf("pythonPathParts(./memory/MEMORY.md) = %q", parts)
	}
	if got := pythonPathParts("/a/b"); !reflect.DeepEqual(got, []string{"/", "a", "b"}) {
		t.Errorf("pythonPathParts(/a/b) = %q", got)
	}
	if got := pythonPathParts("a//b/"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("pythonPathParts(a//b/) = %q", got)
	}
}

// TestUnifiedDiffMatchesDifflib uses expected values produced by
// difflib.unified_diff(a, b, fromfile="f", tofile="g", lineterm="").
func TestUnifiedDiffMatchesDifflib(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want []string
	}{
		{
			name: "replace middle",
			a:    []string{"a\n", "b\n", "c\n"},
			b:    []string{"a\n", "x\n", "c\n"},
			want: []string{"--- f", "+++ g", "@@ -1,3 +1,3 @@", " a\n", "-b\n", "+x\n", " c\n"},
		},
		{
			name: "no trailing newline",
			a:    []string{"x"},
			b:    []string{"y"},
			want: []string{"--- f", "+++ g", "@@ -1 +1 @@", "-x", "+y"},
		},
		{
			name: "insert into empty",
			a:    nil,
			b:    []string{"n\n"},
			want: []string{"--- f", "+++ g", "@@ -0,0 +1 @@", "+n\n"},
		},
		{
			name: "trailing change with context",
			a:    []string{"a\n", "b\n", "c\n", "d\n", "e\n", "f\n", "g\n", "h\n"},
			b:    []string{"a\n", "b\n", "c\n", "d\n", "e\n", "f\n", "g\n", "H\n"},
			want: []string{"--- f", "+++ g", "@@ -5,4 +5,4 @@", " e\n", " f\n", " g\n", "-h\n", "+H\n"},
		},
		{
			// The matcher's tie-breaking decides which repeated lines are
			// treated as the common ones, so this pins find_longest_match.
			name: "ambiguous repeated lines",
			a:    []string{"same\n", "same\n", "same\n", "same\n", "same\n", "old\n"},
			b:    []string{"same\n", "same\n", "same\n", "same\n", "same\n", "new\n"},
			want: []string{"--- f", "+++ g", "@@ -3,4 +3,4 @@", " same\n", " same\n", " same\n", "-old\n", "+new\n"},
		},
		{
			name: "identical",
			a:    []string{"a\n"},
			b:    []string{"a\n"},
			want: nil,
		},
	}
	for _, tc := range cases {
		got := unifiedDiff(tc.a, tc.b, "f", "g", 3, "")
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: unifiedDiff =\n%q\nwant\n%q", tc.name, got, tc.want)
		}
	}
}

func TestUnifiedDiffAutojunk(t *testing.T) {
	// difflib's autojunk heuristic (on by default, and what dulwich uses) drops
	// elements that occur more than n//100+1 times once n >= 200. For this input
	// that is the difference between one 124-line hunk and a single changed
	// line: the 240 repeated "a" lines become unmatchable.
	//
	// Expected values come from difflib.unified_diff with autojunk=True; the
	// autojunk=False output would be "@@ -118,7 +118,7 @@" and a one-line
	// change, so this case would catch a port that forgot the heuristic.
	var a, b []string
	for i := 0; i < 120; i++ {
		a = append(a, "a\n")
		b = append(b, "a\n")
	}
	a = append(a, "mid\n")
	b = append(b, "mid2\n")
	for i := 0; i < 120; i++ {
		a = append(a, "a\n")
		b = append(b, "a\n")
	}

	got := unifiedDiff(a, b, "f", "g", 3, "")
	if len(got) != 248 {
		t.Fatalf("unifiedDiff produced %d lines, want 248", len(got))
	}
	if got[2] != "@@ -118,124 +118,124 @@" {
		t.Errorf("hunk header = %q, want @@ -118,124 +118,124 @@", got[2])
	}
	// The hunk keeps three lines of leading context and then replaces the rest.
	if !reflect.DeepEqual(got[3:9], []string{" a\n", " a\n", " a\n", "-mid\n", "-a\n", "-a\n"}) {
		t.Errorf("hunk body = %q", got[3:9])
	}
	// A shorter input of the same shape is below the threshold and diffs as a
	// single replaced line, which is the control for the heuristic.
	smallA := append(append([]string{}, a[:100]...), "mid\n")
	smallB := append(append([]string{}, b[:100]...), "mid2\n")
	small := unifiedDiff(smallA, smallB, "f", "g", 3, "")
	if len(small) != 8 || small[2] != "@@ -98,4 +98,4 @@" {
		t.Errorf("below-threshold diff = %q", small)
	}
}

// ---------------------------------------------------------------------------
// storage layer
// ---------------------------------------------------------------------------

func TestIndexRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index")
	idx := newGitIndex()
	id := objectIDFromHexForTest(t, "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391")
	idx.entries["SOUL.md"] = &indexEntry{
		ctimeSec: 100, ctimeNsec: 200, mtimeSec: 300, mtimeNsec: 400,
		dev: 5, ino: 6, mode: modeRegular, uid: 1000, gid: 1000,
		size: 0, id: id, name: "SOUL.md",
	}
	idx.entries["memory/MEMORY.md"] = &indexEntry{mode: modeExecutable, id: id, name: "memory/MEMORY.md"}
	if err := idx.write(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data[:4]) != "DIRC" {
		t.Errorf("signature = %q", data[:4])
	}
	if len(data)%8 != 0 {
		t.Errorf("index length %d is not a multiple of 8", len(data))
	}

	back, err := readIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.entries) != 2 {
		t.Fatalf("read %d entries, want 2", len(back.entries))
	}
	got := back.entries["SOUL.md"]
	if got == nil {
		t.Fatal("SOUL.md entry is missing")
	}
	if got.mode != modeRegular || got.id != id || got.size != 0 ||
		got.ctimeSec != 100 || got.ctimeNsec != 200 || got.mtimeSec != 300 || got.mtimeNsec != 400 ||
		got.dev != 5 || got.ino != 6 || got.uid != 1000 || got.gid != 1000 {
		t.Errorf("entry round-trip lost data: %+v", got)
	}

	// A missing index file is an empty index, not an error.
	empty, err := readIndex(filepath.Join(dir, "absent"))
	if err != nil || len(empty.entries) != 0 {
		t.Errorf("readIndex(absent) = %v, %v", empty, err)
	}
	// Corruption is reported.
	if err := os.WriteFile(path, []byte("XXXX12345678"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readIndex(path); err == nil {
		t.Error("readIndex accepted a corrupt index")
	}
	if err := os.WriteFile(path, []byte("DIRC\x00\x00\x00\x01\x00\x00\x00\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readIndex(path); err == nil {
		t.Error("readIndex accepted version 1")
	}
}

func objectIDFromHexForTest(t *testing.T, hex string) objectID {
	t.Helper()
	id, err := objectIDFromHex(hex)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestObjectStoreRoundTrip(t *testing.T) {
	ws := t.TempDir()
	repo := openRepository(ws)
	if err := os.MkdirAll(filepath.Join(ws, ".git", "objects"), 0o777); err != nil {
		t.Fatal(err)
	}
	repo.store = newObjectStore(filepath.Join(ws, ".git"))

	blob := []byte("hello\n")
	id, err := repo.store.put(kindBlob, blob)
	if err != nil {
		t.Fatal(err)
	}
	// The id must be the well-known git hash of that blob.
	if want := "ce013625030ba8dba906f756967f9e9ca394464a"; id.String() != want {
		t.Errorf("blob id = %s, want %s", id, want)
	}
	obj, err := repo.store.get(id)
	if err != nil {
		t.Fatal(err)
	}
	if obj.kind != kindBlob || !bytes.Equal(obj.data, blob) {
		t.Errorf("get() = %s %q", obj.kind, obj.data)
	}

	// Writing the same content twice is a no-op that still succeeds.
	again, err := repo.store.put(kindBlob, blob)
	if err != nil || again != id {
		t.Errorf("second put = %s, %v", again, err)
	}

	// A corrupt loose object is reported rather than silently ignored.
	loose := repo.store.loosePath(id)
	if err := os.Chmod(loose, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loose, []byte("not zlib"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.get(id); err == nil {
		t.Error("get() accepted a corrupt loose object")
	}
	// A missing object is an error, not an empty value.
	if _, err := repo.store.get(objectIDFromHexForTest(t, "0000000000000000000000000000000000000001")); err == nil {
		t.Error("get() of a missing object did not fail")
	}
}

func TestTreeEncodingAndParsing(t *testing.T) {
	dir := t.TempDir()
	store := newObjectStore(filepath.Join(dir, "objects"))
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o777); err != nil {
		t.Fatal(err)
	}
	blobID, err := store.put(kindBlob, []byte("x\n"))
	if err != nil {
		t.Fatal(err)
	}
	subID, err := store.put(kindTree, encodeTree([]treeEntry{{name: "MEMORY.md", mode: "100644", id: blobID}}))
	if err != nil {
		t.Fatal(err)
	}
	rootID, err := store.put(kindTree, encodeTree([]treeEntry{
		{name: "memory", mode: "40000", id: subID},
		{name: "SOUL.md", mode: "100644", id: blobID},
		{name: "a.txt", mode: "100644", id: blobID},
	}))
	if err != nil {
		t.Fatal(err)
	}

	obj, err := store.get(rootID)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := parseTree(obj.data)
	if err != nil {
		t.Fatal(err)
	}
	// git's tree order sorts a subtree by name + "/", so "a.txt" and "SOUL.md"
	// come before the "memory" directory.
	want := []string{"SOUL.md", "a.txt", "memory"}
	if len(entries) != len(want) {
		t.Fatalf("parsed %d entries, want %d", len(entries), len(want))
	}
	for i, name := range want {
		if entries[i].name != name {
			t.Errorf("entry %d = %q, want %q", i, entries[i].name, name)
		}
	}
	if entries[2].mode != "40000" {
		t.Errorf("directory mode = %q, want 40000", entries[2].mode)
	}

	// The empty tree has a well-known id.
	emptyID, err := store.put(kindTree, encodeTree(nil))
	if err != nil {
		t.Fatal(err)
	}
	if want := "4b825dc642cb6eb9a060e54bf8d69288fbee4904"; emptyID.String() != want {
		t.Errorf("empty tree id = %s, want %s", emptyID, want)
	}
}

func TestCommitEncodingAndParsing(t *testing.T) {
	ident := commitIdent{name: "nanobot", email: "nanobot@dream"}
	body := encodeCommit(
		objectIDFromHexForTest(t, "4b825dc642cb6eb9a060e54bf8d69288fbee4904"),
		[]objectID{objectIDFromHexForTest(t, "0000000000000000000000000000000000000001")},
		ident, ident, 1700000000, 1700000001, -10800, -10800,
		[]byte("subject line\n\nbody\n"),
	)
	commit, err := parseCommit(body)
	if err != nil {
		t.Fatal(err)
	}
	// The commit time is the committer's, the second of the two stamps.
	if commit.commitTime != 1700000001 {
		t.Errorf("commit time = %d, want 1700000001", commit.commitTime)
	}
	if string(commit.message) != "subject line\n\nbody\n" {
		t.Errorf("message = %q", commit.message)
	}
	if len(commit.parents) != 1 {
		t.Errorf("parents = %v", commit.parents)
	}
	// The timezone is rendered the way git writes it.
	if got := formatTimezone(-10800, false); got != "-0300" {
		t.Errorf("formatTimezone(-10800) = %q", got)
	}
	if got := formatTimezone(0, false); got != "+0000" {
		t.Errorf("formatTimezone(0) = %q", got)
	}
	if got := formatTimezone(19800, false); got != "+0530" {
		t.Errorf("formatTimezone(19800) = %q", got)
	}
}

func TestDefaultBranchNameFromConfig(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(config, []byte("[init]\n\tdefaultBranch = trunk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	if got := defaultBranchName(); got != "trunk" {
		t.Errorf("defaultBranchName() = %q, want trunk", got)
	}

	ws := t.TempDir()
	g := New(ws, []string{"SOUL.md"})
	if _, err := g.Init(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(ws, ".git", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ref: refs/heads/trunk\n" {
		t.Errorf(".git/HEAD = %q", data)
	}
	// Without the setting the reference's default applies.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "absent"))
	if got := defaultBranchName(); got != "master" {
		t.Errorf("defaultBranchName() = %q, want master", got)
	}
}

// ---------------------------------------------------------------------------
// ignore sources
// ---------------------------------------------------------------------------

// hermeticConfig keeps the host's git configuration out of these tests.
func hermeticConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "absent-gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// appendFile adds text to an existing file, creating it when needed.
func appendFile(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestIgnoreSourcesPinStagingBehaviour covers the ignore rules that decide
// whether a tracked file is staged at all. The precedence is dulwich's, not
// git's: matches are collected from the deepest .gitignore outwards to the
// global filters, and the last match wins — so a global ignore file overrides
// the .gitignore the store writes, while a nested .gitignore cannot.
//
// Every expectation here was verified against the reference (see the ignores
// section of the differential dumper).
func TestIgnoreSourcesPinStagingBehaviour(t *testing.T) {
	setup := func(t *testing.T) (string, *GitStore) {
		t.Helper()
		hermeticConfig(t)
		ws := t.TempDir()
		g := New(ws, tracked)
		if _, err := g.Init(); err != nil {
			t.Fatal(err)
		}
		return ws, g
	}
	pointConfigAt := func(t *testing.T, ws, ignoreFile string) {
		t.Helper()
		config := filepath.Join(ws, ".git", "config")
		data, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, []byte("[core]\n\texcludesFile = "+ignoreFile+"\n")...)
		if err := os.WriteFile(config, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("user ignore file wins over .gitignore", func(t *testing.T) {
		ws, g := setup(t)
		userIgnore := filepath.Join(t.TempDir(), "ignore")
		writeFile(t, userIgnore, "*.md\n")
		pointConfigAt(t, ws, userIgnore)
		writeFile(t, filepath.Join(ws, "SOUL.md"), "changed\n")

		// The store's own .gitignore re-includes SOUL.md, but the global filter
		// is consulted last and wins: nothing is staged, so nothing is committed.
		if _, committed, err := g.AutoCommit("dream: ignored"); err != nil || committed {
			t.Fatalf("AutoCommit() = %v, %v; want no commit", committed, err)
		}
		if summary, err := g.SummarizeWorkingTree([]string{"SOUL.md"}); err != nil || summary == "" {
			t.Errorf("summary = %q, %v; the change is still in the working tree", summary, err)
		}
	})

	t.Run("XDG user ignore file is the default", func(t *testing.T) {
		ws, g := setup(t)
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		writeFile(t, filepath.Join(xdg, "git", "ignore"), "*.md\n")
		writeFile(t, filepath.Join(ws, "SOUL.md"), "changed\n")
		if _, committed, err := g.AutoCommit("dream: ignored"); err != nil || committed {
			t.Fatalf("AutoCommit() = %v, %v; want no commit", committed, err)
		}
	})

	t.Run("info/exclude wins over .gitignore", func(t *testing.T) {
		ws, g := setup(t)
		writeFile(t, filepath.Join(ws, ".git", "info", "exclude"), "SOUL.md\n")
		writeFile(t, filepath.Join(ws, "SOUL.md"), "changed\n")
		if _, committed, err := g.AutoCommit("dream: excluded"); err != nil || committed {
			t.Fatalf("AutoCommit() = %v, %v; want no commit", committed, err)
		}
	})

	t.Run("nested .gitignore loses to the root one", func(t *testing.T) {
		ws, g := setup(t)
		writeFile(t, filepath.Join(ws, "memory", ".gitignore"), "MEMORY.md\n")
		writeFile(t, filepath.Join(ws, "memory", "MEMORY.md"), "changed\n")
		// The root .gitignore's "!memory/MEMORY.md" is consulted after the
		// nested pattern, so the file is still staged.
		if _, committed, err := g.AutoCommit("dream: nested"); err != nil || !committed {
			t.Fatalf("AutoCommit() = %v, %v; want a commit", committed, err)
		}
		if summary, err := g.SummarizeWorkingTree([]string{"memory/MEMORY.md"}); err != nil || summary != "" {
			t.Errorf("summary after commit = %q, %v", summary, err)
		}
	})

	t.Run("ignorecase applies to .gitignore only", func(t *testing.T) {
		ws, g := setup(t)
		appendFile(t, filepath.Join(ws, ".git", "config"), "[core]\n\tignorecase = true\n")
		appendFile(t, filepath.Join(ws, ".gitignore"), "soul.md\n")
		writeFile(t, filepath.Join(ws, "SOUL.md"), "changed\n")
		// A .gitignore pattern is matched case-insensitively when the repository
		// says so, so the lower-case pattern hides SOUL.md and nothing is staged.
		if _, committed, err := g.AutoCommit("dream: ignorecase"); err != nil || committed {
			t.Fatalf("AutoCommit() = %v, %v; want no commit", committed, err)
		}
	})

	t.Run("ignorecase leaves info/exclude case-sensitive", func(t *testing.T) {
		ws, g := setup(t)
		appendFile(t, filepath.Join(ws, ".git", "config"), "[core]\n\tignorecase = true\n")
		writeFile(t, filepath.Join(ws, ".git", "info", "exclude"), "soul.md\n")
		writeFile(t, filepath.Join(ws, "SOUL.md"), "changed\n")
		// The global filters are built without the flag (from_repo calls
		// IgnoreFilter.from_path, which defaults it to false), so the same
		// lower-case pattern does NOT hide SOUL.md here.
		if _, committed, err := g.AutoCommit("dream: ignorecase"); err != nil || !committed {
			t.Fatalf("AutoCommit() = %v, %v; want a commit", committed, err)
		}
	})

	t.Run("an unparseable ignorecase fails the commit", func(t *testing.T) {
		ws, g := setup(t)
		// dulwich's get_boolean accepts only "true" and "false": the reference
		// raises ValueError, which auto_commit reports as a GitStoreError.
		appendFile(t, filepath.Join(ws, ".git", "config"), "[core]\n\tignorecase = 1\n")
		writeFile(t, filepath.Join(ws, "SOUL.md"), "changed\n")
		_, committed, err := g.AutoCommit("dream: ignorecase")
		var gse *GitStoreError
		if !errors.As(err, &gse) {
			t.Fatalf("AutoCommit() error = %v, want a *GitStoreError", err)
		}
		if gse.Error() != "Git auto-commit failed: dream: ignorecase" {
			t.Errorf("error = %q", gse.Error())
		}
		if committed {
			t.Errorf("committed = true on a failed commit")
		}
	})

	t.Run("unrelated ignore files change nothing", func(t *testing.T) {
		ws, g := setup(t)
		userIgnore := filepath.Join(t.TempDir(), "ignore")
		writeFile(t, userIgnore, "*.tmp\n")
		pointConfigAt(t, ws, userIgnore)
		writeFile(t, filepath.Join(ws, "SOUL.md"), "changed\n")
		if _, committed, err := g.AutoCommit("dream: tracked"); err != nil || !committed {
			t.Fatalf("AutoCommit() = %v, %v; want a commit", committed, err)
		}
	})
}

func TestStagingASymlink(t *testing.T) {
	ws := t.TempDir()
	g := New(ws, []string{"SOUL.md", "link.md"})
	if _, err := g.Init(); err != nil {
		t.Fatal(err)
	}
	// Replace the tracked file with a symlink and commit: git stores the link
	// target as a blob with mode 120000, and the reference does the same
	// (dulwich/index.py:blob_from_path_and_mode reads os.readlink).
	writeFile(t, filepath.Join(ws, "SOUL.md"), "target content\n")
	if err := os.Remove(filepath.Join(ws, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("SOUL.md", filepath.Join(ws, "link.md")); err != nil {
		t.Skipf("symlinks are not supported here: %v", err)
	}
	sha, committed, err := g.AutoCommit("dream: symlink")
	if err != nil || !committed {
		t.Fatalf("AutoCommit() = %q, %v, %v", sha, committed, err)
	}

	repo := openRepository(ws)
	tree, ok, err := repo.headTree()
	if err != nil || !ok {
		t.Fatalf("headTree() ok=%v err=%v", ok, err)
	}
	// A change of file type is reported as a delete followed by an add, with
	// the new side diffed against /dev/null and the missing trailing newline
	// marked. The expected text below is the reference's own output for this
	// exact scenario.
	log, err := g.Log(20, nil)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := g.DiffCommits(log[1].SHA, log[0].SHA)
	if err != nil {
		t.Fatal(err)
	}
	wantDiff := "diff --git a/SOUL.md b/SOUL.md\n" +
		"index e69de29..2ceb84c 100644\n" +
		"--- a/SOUL.md\n+++ b/SOUL.md\n@@ -0,0 +1 @@\n+target content\n" +
		"diff --git a/link.md b/link.md\n" +
		"deleted file mode 100644\n" +
		"index e69de29..0000000\n" +
		"diff --git a/link.md b/link.md\n" +
		"new file mode 120000\n" +
		"index 0000000..c691c4e\n" +
		"--- /dev/null\n+++ b/link.md\n@@ -0,0 +1 @@\n+SOUL.md\n\\ No newline at end of file\n"
	if diff != wantDiff {
		t.Errorf("symlink diff =\n%q\nwant\n%q", diff, wantDiff)
	}

	entries, err := repo.treeEntries(tree)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.name != "link.md" {
			continue
		}
		if e.mode != "120000" {
			t.Errorf("symlink mode = %q, want 120000", e.mode)
		}
		obj, err := repo.store.get(e.id)
		if err != nil {
			t.Fatal(err)
		}
		if string(obj.data) != "SOUL.md" {
			t.Errorf("symlink blob = %q, want the link target", obj.data)
		}
		return
	}
	t.Fatal("link.md is missing from the committed tree")
}
