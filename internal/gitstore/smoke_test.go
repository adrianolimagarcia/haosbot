package gitstore

import (
	"os"
	"path/filepath"
	"testing"
)

// tracked mirrors the reference's tracked file list (docs/spec-memory.md).
var tracked = []string{"SOUL.md", "USER.md", "memory/MEMORY.md", "memory/.dream_cursor"}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSmokeLifecycle(t *testing.T) {
	ws := t.TempDir()
	g := New(ws, tracked)

	if g.IsInitialized() {
		t.Fatal("fresh workspace must not be initialized")
	}
	created, err := g.Init()
	if err != nil || !created {
		t.Fatalf("Init() = %v, %v; want true, nil", created, err)
	}
	if created, err := g.Init(); err != nil || created {
		t.Fatalf("second Init() = %v, %v; want false, nil", created, err)
	}
	entries, err := g.Log(20, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("log after init = %d entries, want 1", len(entries))
	}
	t.Logf("init commit: %+v", entries[0])

	// Nothing changed: auto_commit is a no-op.
	sha, committed, err := g.AutoCommit("noop")
	if err != nil || committed || sha != "" {
		t.Fatalf("clean AutoCommit() = %q, %v, %v; want empty, false, nil", sha, committed, err)
	}

	writeFile(t, filepath.Join(ws, "SOUL.md"), "hello\nworld\n")
	summary, err := g.SummarizeWorkingTree([]string{"SOUL.md", "USER.md", "memory/MEMORY.md"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("summary:\n%s", summary)

	sha, committed, err = g.AutoCommit("dream: test")
	if err != nil || !committed {
		t.Fatalf("AutoCommit() = %q, %v, %v; want a commit", sha, committed, err)
	}
	t.Logf("commit sha: %s", sha)

	diff, err := g.DiffCommits(entries[0].SHA, sha)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("diff:\n%s", diff)

	entry, d, ok, err := g.ShowCommitDiff(sha, 20, nil)
	if err != nil || !ok {
		t.Fatalf("ShowCommitDiff() ok=%v err=%v", ok, err)
	}
	t.Logf("show: %+v\n%s", entry, d)

	revertSHA, reverted, err := g.Revert(sha, nil)
	if err != nil || !reverted {
		t.Fatalf("Revert() = %q, %v, %v", revertSHA, reverted, err)
	}
	data, err := os.ReadFile(filepath.Join(ws, "SOUL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "" {
		t.Fatalf("after revert SOUL.md = %q, want empty", data)
	}
}
