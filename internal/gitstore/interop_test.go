package gitstore

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests check the repository this port writes against the real git CLI.
// The reference does not use git at all (it uses dulwich), but the repository it
// produces is a normal git repository: git must be able to read it, and this
// port must be able to read a repository git has packed.

func gitPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed; skipping interop check")
	}
	return path
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gitPath(t), args...)
	cmd.Dir = dir
	// Keep the check hermetic: no user config, no global ignore file.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=nanobot", "GIT_AUTHOR_EMAIL=nanobot@dream",
		"GIT_COMMITTER_NAME=nanobot", "GIT_COMMITTER_EMAIL=nanobot@dream",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func TestGitCLIReadsGoRepository(t *testing.T) {
	ws := t.TempDir()
	g := New(ws, tracked)
	if _, err := g.Init(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ws, "SOUL.md"), "hello\nworld\n")
	if _, _, err := g.AutoCommit("dream: go commit"); err != nil {
		t.Fatal(err)
	}

	if out := gitRun(t, ws, "fsck", "--strict"); strings.TrimSpace(out) != "" {
		t.Errorf("git fsck reported problems:\n%s", out)
	}
	if out := gitRun(t, ws, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("git status is not clean:\n%s", out)
	}
	log := gitRun(t, ws, "log", "--format=%s")
	if !strings.Contains(log, "dream: go commit") || !strings.Contains(log, "init: nanobot memory store") {
		t.Errorf("git log is missing commits:\n%s", log)
	}
	// The tracked files must be committed with the right modes, and nothing else.
	ls := gitRun(t, ws, "ls-files", "-s")
	for _, want := range []string{"100644", "SOUL.md", "USER.md", "memory/MEMORY.md", "memory/.dream_cursor", ".gitignore"} {
		if !strings.Contains(ls, want) {
			t.Errorf("git ls-files output is missing %q:\n%s", want, ls)
		}
	}
	if n := len(strings.Split(strings.TrimSpace(ls), "\n")); n != 5 {
		t.Errorf("git ls-files lists %d paths, want 5:\n%s", n, ls)
	}
	if out := gitRun(t, ws, "rev-parse", "HEAD"); !strings.HasPrefix(strings.TrimSpace(out), g.headShortSHA(t)) {
		t.Errorf("git rev-parse HEAD = %q, Go reported %q", out, g.headShortSHA(t))
	}
	// Reflogs are split the way dulwich splits them, which is NOT the way git
	// itself would: the first commit is created while HEAD is unborn, so
	// dulwich's refs.add_if_new logs it under the name it was given ("HEAD" →
	// .git/logs/HEAD); later commits go through refs.set_if_equals, which logs
	// under the *resolved* ref (.git/logs/refs/heads/master). Verified against
	// the reference by dumping both files after two dulwich commits; git reflog
	// therefore only shows the initial commit.
	headLog := gitRun(t, ws, "reflog")
	if !strings.Contains(headLog, "commit: init: nanobot memory store") {
		t.Errorf("git reflog does not show the initial commit:\n%s", headLog)
	}
	if strings.Contains(headLog, "commit: dream: go commit") {
		t.Errorf("git reflog shows a commit that the reference logs under the branch instead:\n%s", headLog)
	}
	branchLog := gitRun(t, ws, "reflog", "refs/heads/master")
	if !strings.Contains(branchLog, "commit: dream: go commit") {
		t.Errorf("branch reflog does not show the second commit:\n%s", branchLog)
	}
}

func (g *GitStore) headShortSHA(t *testing.T) string {
	t.Helper()
	entries, err := g.Log(1, nil)
	if err != nil || len(entries) == 0 {
		t.Fatalf("Log() = %v, %v", entries, err)
	}
	return entries[0].SHA
}

// TestGoReadsGitPackedRepository packs the repository with the git CLI and checks
// that the port still reads it: objects move into a packfile, refs may move into
// packed-refs, and git stores most of those objects as deltas — which is the
// code path that has no counterpart in the loose-object reader.
func TestGoReadsGitPackedRepository(t *testing.T) {
	ws := t.TempDir()
	g := New(ws, tracked)
	if _, err := g.Init(); err != nil {
		t.Fatal(err)
	}
	// Several similar, sizeable revisions so that git packs them as deltas.
	var body strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&body, "line %d %s\n", i, strings.Repeat("x", 8))
	}
	for rev := 0; rev < 6; rev++ {
		body.WriteString(fmt.Sprintf("revision %d\n", rev))
		writeFile(t, filepath.Join(ws, "SOUL.md"), body.String())
		if _, committed, err := g.AutoCommit(fmt.Sprintf("dream: packed %d", rev)); err != nil || !committed {
			t.Fatalf("AutoCommit(%d) = %v, %v", rev, committed, err)
		}
	}
	before, err := g.Log(10, nil)
	if err != nil {
		t.Fatal(err)
	}
	beforeSummary, err := g.SummarizeWorkingTree([]string{"SOUL.md"})
	if err != nil {
		t.Fatal(err)
	}
	beforeDiff, err := g.DiffCommits(before[len(before)-1].SHA, before[0].SHA)
	if err != nil {
		t.Fatal(err)
	}

	gitRun(t, ws, "gc", "--aggressive", "--prune=now")
	gitRun(t, ws, "pack-refs", "--all")

	// The pack must actually contain deltas, otherwise this test would not
	// exercise the delta reader at all.
	verify := gitRun(t, ws, "verify-pack", "-v", packedIndex(t, ws))
	deltas := 0
	for _, line := range strings.Split(verify, "\n") {
		if len(strings.Fields(line)) >= 7 {
			deltas++
		}
	}
	if deltas == 0 {
		t.Fatal("git produced a pack with no deltas; the delta reader is not covered")
	}
	t.Logf("pack contains %d delta objects", deltas)

	after, err := g.Log(10, nil)
	if err != nil {
		t.Fatalf("Log() after git gc: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("Log() after git gc = %d entries, want %d", len(after), len(before))
	}
	for i := range after {
		if after[i] != before[i] {
			t.Errorf("entry %d changed after git gc: %+v != %+v", i, after[i], before[i])
		}
	}
	afterSummary, err := g.SummarizeWorkingTree([]string{"SOUL.md"})
	if err != nil {
		t.Fatalf("SummarizeWorkingTree() after git gc: %v", err)
	}
	if afterSummary != beforeSummary {
		t.Errorf("summary after git gc = %q, want %q", afterSummary, beforeSummary)
	}
	afterDiff, err := g.DiffCommits(after[len(after)-1].SHA, after[0].SHA)
	if err != nil {
		t.Fatalf("DiffCommits() after git gc: %v", err)
	}
	if afterDiff != beforeDiff {
		t.Errorf("diff after git gc differs from the loose-object diff")
	}

	// And a new commit still works on the packed repository.
	body.WriteString("after gc\n")
	writeFile(t, filepath.Join(ws, "SOUL.md"), body.String())
	if _, committed, err := g.AutoCommit("dream: after gc"); err != nil || !committed {
		t.Fatalf("AutoCommit() after git gc = %v, %v", committed, err)
	}
	if out := gitRun(t, ws, "fsck", "--strict"); strings.TrimSpace(out) != "" {
		t.Errorf("git fsck reported problems after the post-gc commit:\n%s", out)
	}
	if out := gitRun(t, ws, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("git status is not clean after the post-gc commit:\n%s", out)
	}
}

// packedIndex returns the repository's pack index path.
func packedIndex(t *testing.T, ws string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(ws, ".git", "objects", "pack", "*.idx"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no pack index found: %v", err)
	}
	return matches[0]
}

// TestIgnoresForeignPaths checks the .gitignore the store writes actually keeps
// the repository limited to the tracked files.
func TestIgnoresForeignPaths(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "scratch.txt"), "not tracked\n")
	g := New(ws, tracked)
	if _, err := g.Init(); err != nil {
		t.Fatal(err)
	}
	if out := gitRun(t, ws, "ls-files"); strings.Contains(out, "scratch.txt") {
		t.Errorf("git ls-files lists an untracked file:\n%s", out)
	}
	writeFile(t, filepath.Join(ws, "scratch.txt"), "still not tracked\n")
	if _, committed, err := g.AutoCommit("dream: nothing"); err != nil || committed {
		t.Fatalf("AutoCommit() = %v, %v; want no commit", committed, err)
	}
}
