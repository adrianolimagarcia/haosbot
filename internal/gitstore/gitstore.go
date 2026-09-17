package gitstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// GitStore is the port of nanobot/utils/gitstore.py:GitStore — git-backed
// version control for the memory files.
//
// Public API (Python signatures on the left):
//
//	GitStore(workspace, tracked_files)          New(workspace, trackedFiles)
//	is_initialized()                            IsInitialized()
//	init() -> bool                              Init() (bool, error)
//	auto_commit(message) -> str | None          AutoCommit(message) (string, bool, error)
//	log(max_entries, message_prefix)            Log(maxEntries, messagePrefix) ([]CommitInfo, error)
//	diff_commits(sha1, sha2) -> str             DiffCommits(sha1, sha2) (string, error)
//	summarize_working_tree(paths) -> str        SummarizeWorkingTree(paths) (string, error)
//	show_commit_diff(sha, max, prefix)          ShowCommitDiff(sha, maxEntries, messagePrefix)
//	revert(commit, *, message_prefix=None)     Revert(commit, messagePrefix) (string, bool, error)
//
// The bool in AutoCommit/Revert reports "a commit was created"; Python spells
// that as returning None. ShowCommitDiff returns ok=false for Python's None.
//
// Error model: every failure the reference wraps in GitStoreError comes back as
// *GitStoreError with the same message text (gitstore.py:121, :166, :195, :274,
// :298, :385, :440, :507). The reference logs warnings for a few non-fatal cases
// (gitstore.py:70, :463, :477, :485); this port has no logging subsystem, so
// those branches simply return their value.
type GitStore struct {
	workspace    string
	trackedFiles []string
}

// DefaultLogEntries is the reference's default for log(max_entries=20)
// (gitstore.py:232).
const DefaultLogEntries = 20

// initMessage and the identity below are hard-coded by the reference for every
// commit it makes (gitstore.py:114-116, :154-155).
const initMessage = "init: nanobot memory store"

var nanobotIdentity = commitIdent{name: "nanobot", email: "nanobot@dream"}

// workingTreeDiffMaxChars caps the unified-diff block in
// summarize_working_tree (gitstore.py:21).
const workingTreeDiffMaxChars = 6000

// GitStoreError reports that the memory git repository could not complete an
// operation. It mirrors gitstore.py:GitStoreError, including its message text.
type GitStoreError struct {
	msg string
	err error
}

func (e *GitStoreError) Error() string { return e.msg }

// Unwrap exposes the underlying cause.
func (e *GitStoreError) Unwrap() error { return e.err }

func newGitStoreError(msg string, cause error) *GitStoreError {
	return &GitStoreError{msg: msg, err: cause}
}

// New returns a GitStore for workspace tracking trackedFiles. The workspace is
// not touched until Init is called.
func New(workspace string, trackedFiles []string) *GitStore {
	return &GitStore{workspace: workspace, trackedFiles: trackedFiles}
}

// Workspace returns the workspace directory the store operates on.
func (g *GitStore) Workspace() string { return g.workspace }

// TrackedFiles returns the tracked file list, relative to the workspace.
func (g *GitStore) TrackedFiles() []string {
	out := make([]string, len(g.trackedFiles))
	copy(out, g.trackedFiles)
	return out
}

// CommitInfo mirrors gitstore.py:CommitInfo.
type CommitInfo struct {
	SHA       string // short SHA (8 characters)
	Message   string
	Timestamp string // "%Y-%m-%d %H:%M", local time
}

// Subject returns the first line of the commit message, or a placeholder when
// the message is empty (gitstore.py:34-37).
func (c CommitInfo) Subject() string {
	lines := splitLinesStr(c.Message)
	if len(lines) == 0 {
		return "(no message)"
	}
	return lines[0]
}

// Format renders the commit for display, optionally with a diff
// (gitstore.py:39-44).
func (c CommitInfo) Format(diff string) string {
	header := "## " + c.Subject() + "\n`" + c.SHA + "` — " + c.Timestamp + "\n"
	if diff != "" {
		return header + "\n```diff\n" + diff + "\n```"
	}
	return header + "\n(no file changes)"
}

// ---------------------------------------------------------------------------
// init
// ---------------------------------------------------------------------------

// IsInitialized reports whether workspace/.git is a directory
// (gitstore.py:54-56).
func (g *GitStore) IsInitialized() bool {
	return openRepository(g.workspace).isInitialized()
}

// Init initializes the repository if needed and returns true when a new repo was
// created. It mirrors GitStore.init (gitstore.py:60-121) including the two
// early-outs: an existing repo, and a workspace that already sits inside
// somebody else's repository (no nested repo, and no files written).
func (g *GitStore) Init() (bool, error) {
	if g.IsInitialized() {
		return false, nil
	}
	if g.isInsideGitRepo() {
		return false, nil
	}
	ok, err := g.initUnchecked()
	if err != nil {
		return false, newGitStoreError(fmt.Sprintf("Git store init failed for %s", g.workspace), err)
	}
	return ok, nil
}

func (g *GitStore) initUnchecked() (bool, error) {
	// porcelain.init creates the directory when it is missing
	// (dulwich/porcelain/__init__.py:1080-1085).
	if _, err := os.Stat(g.workspace); errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(g.workspace, 0o777); err != nil {
			return false, err
		}
	}
	repo, err := initRepository(g.workspace)
	if err != nil {
		return false, err
	}

	// Write .gitignore, merging with an existing one (gitstore.py:82-97).
	gitignorePath := pythonPathJoin(g.workspace, ".gitignore")
	entries := g.buildGitignore()
	if existing, err := os.ReadFile(gitignorePath); err == nil {
		existingText := string(existing)
		existingLines := map[string]bool{}
		for _, line := range splitLinesStr(existingText) {
			existingLines[line] = true
		}
		var newLines []string
		for _, line := range splitLinesStr(entries) {
			if !existingLines[line] {
				newLines = append(newLines, line)
			}
		}
		if len(newLines) > 0 {
			merged := strings.TrimRight(existingText, "\n") + "\n" + strings.Join(newLines, "\n") + "\n"
			if err := os.WriteFile(gitignorePath, []byte(merged), 0o666); err != nil {
				return false, err
			}
		}
	} else {
		if err := os.WriteFile(gitignorePath, []byte(entries), 0o666); err != nil {
			return false, err
		}
	}

	// Touch every tracked file so the initial commit has something to track
	// (gitstore.py:99-105).
	for _, rel := range g.trackedFiles {
		p := pythonPathJoin(g.workspace, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
			return false, err
		}
		if _, err := os.Stat(p); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
			if err := os.WriteFile(p, nil, 0o666); err != nil {
				return false, err
			}
		}
	}

	staging := g.stagingPaths(append([]string{".gitignore"}, g.trackedFiles...)...)
	ignores, err := newIgnoreManager(repo.gitDir, g.workspace)
	if err != nil {
		return false, err
	}
	if _, _, err := repo.stagePaths(staging, ignores); err != nil {
		return false, err
	}
	if _, err := repo.commit([]byte(initMessage), nanobotIdentity, time.Now().Unix(), localTimezoneOffset()); err != nil {
		return false, err
	}
	return true, nil
}

// isInsideGitRepo mirrors _is_inside_git_repo (gitstore.py:197-211): walk up from
// the resolved workspace looking for a ".git" entry. A file counts as well as a
// directory, because worktrees and submodules use a .git file.
func (g *GitStore) isInsideGitRepo() bool {
	current := resolveNonStrict(g.workspace)
	for {
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}

// buildGitignore mirrors _build_gitignore (gitstore.py:213-226).
func (g *GitStore) buildGitignore() string {
	dirs := map[string]bool{}
	for _, f := range g.trackedFiles {
		parent := filepath.ToSlash(filepath.Dir(f))
		if parent != "." {
			dirs[parent] = true
		}
	}
	sortedDirs := make([]string, 0, len(dirs))
	for d := range dirs {
		sortedDirs = append(sortedDirs, d)
	}
	sort.Strings(sortedDirs)

	lines := []string{"/*"}
	for _, d := range sortedDirs {
		lines = append(lines, "!"+d+"/")
	}
	for _, f := range g.trackedFiles {
		lines = append(lines, "!"+f)
	}
	lines = append(lines, "!.gitignore")
	return strings.Join(lines, "\n") + "\n"
}

// stagingPaths mirrors _staging_paths (gitstore.py:170-172): absolute paths,
// without resolving symlinks in the final component.
func (g *GitStore) stagingPaths(paths ...string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, pythonAbsolute(pythonPathJoin(g.workspace, p)))
	}
	return out
}

// ---------------------------------------------------------------------------
// daily operations
// ---------------------------------------------------------------------------

// AutoCommit stages the tracked files and commits when the working tree differs
// from HEAD. It returns the 8-character commit SHA and true when a commit was
// created; ("", false, nil) is Python's None (gitstore.py:125-166).
func (g *GitStore) AutoCommit(message string) (string, bool, error) {
	repo := openRepository(g.workspace)
	if !repo.isInitialized() {
		return "", false, nil
	}
	sha, err := g.autoCommit(repo, message)
	if err != nil {
		return "", false, newGitStoreError("Git auto-commit failed: "+message, err)
	}
	return sha, sha != "", nil
}

func (g *GitStore) autoCommit(repo *repository, message string) (string, error) {
	staging := g.stagingPaths(g.trackedFiles...)
	ignores, err := newIgnoreManager(repo.gitDir, g.workspace)
	if err != nil {
		return "", err
	}
	if _, _, err := repo.stagePaths(staging, ignores); err != nil {
		return "", err
	}
	// Stage first, then ask the index — the reference does it in this order so a
	// same-size rewrite with a preserved mtime is still noticed
	// (gitstore.py:135-143).
	idx, err := readIndex(filepath.Join(repo.gitDir, "index"))
	if err != nil {
		return "", err
	}
	changed, err := repo.hasStagedChanges(idx)
	if err != nil {
		return "", err
	}
	if !changed {
		return "", nil
	}
	id, err := repo.commit([]byte(message), nanobotIdentity, time.Now().Unix(), localTimezoneOffset())
	if err != nil {
		return "", err
	}
	return id.short(), nil
}

// localTimezoneOffset returns the current local UTC offset in seconds, which is
// what dulwich stamps into commits and reflogs (porcelain.get_user_timezones
// uses time.localtime().tm_gmtoff).
func localTimezoneOffset() int {
	_, offset := time.Now().Zone()
	return offset
}

// ---------------------------------------------------------------------------
// query
// ---------------------------------------------------------------------------

// Log returns the commit log, newest first, optionally filtered by message
// prefix. When filtering, maxEntries counts matching commits (gitstore.py:230-274).
func (g *GitStore) Log(maxEntries int, messagePrefix *string) ([]CommitInfo, error) {
	repo := openRepository(g.workspace)
	if !repo.isInitialized() {
		return nil, nil
	}
	entries, err := g.log(repo, maxEntries, messagePrefix)
	if err != nil {
		return nil, newGitStoreError("Git log failed", err)
	}
	return entries, nil
}

func (g *GitStore) log(repo *repository, maxEntries int, messagePrefix *string) ([]CommitInfo, error) {
	var entries []CommitInfo
	sha, ok, err := repo.headID()
	if err != nil || !ok {
		return nil, err
	}
	for len(entries) < maxEntries {
		obj, err := repo.store.get(sha)
		if err != nil {
			return nil, err
		}
		if obj.kind != kindCommit {
			break
		}
		commit, err := parseCommit(obj.data)
		if err != nil {
			return nil, err
		}
		ts := time.Unix(commit.commitTime, 0).Local().Format("2006-01-02 15:04")
		msg := pythonStrip(decodeUTF8Replace(commit.message))
		if messagePrefix == nil || pythonStartsWith(msg, *messagePrefix) {
			entries = append(entries, CommitInfo{SHA: sha.short(), Message: msg, Timestamp: ts})
		}
		if len(commit.parents) == 0 {
			break
		}
		sha = commit.parents[0]
	}
	return entries, nil
}

// DiffCommits returns the unified diff between two commits, or "" when the
// repository is uninitialized or either SHA cannot be resolved
// (gitstore.py:276-298).
func (g *GitStore) DiffCommits(sha1, sha2 string) (string, error) {
	repo := openRepository(g.workspace)
	if !repo.isInitialized() {
		return "", nil
	}
	out, err := g.diffCommits(repo, sha1, sha2)
	if err != nil {
		return "", newGitStoreError(fmt.Sprintf("Git diff failed for %s..%s", sha1, sha2), err)
	}
	return out, nil
}

func (g *GitStore) diffCommits(repo *repository, sha1, sha2 string) (string, error) {
	full1, ok1 := g.resolveSHA(repo, sha1)
	full2, ok2 := g.resolveSHA(repo, sha2)
	if !ok1 || !ok2 {
		return "", nil
	}
	tree1, err := commitTree(repo, full1)
	if err != nil {
		return "", err
	}
	tree2, err := commitTree(repo, full2)
	if err != nil {
		return "", err
	}
	patch, err := repo.writeTreeDiff(tree1, tree2)
	if err != nil {
		return "", err
	}
	return decodeUTF8Replace(patch), nil
}

// commitTree returns the tree of a commit object.
func commitTree(repo *repository, id objectID) (*objectID, error) {
	obj, err := repo.store.get(id)
	if err != nil {
		return nil, err
	}
	if obj.kind != kindCommit {
		return nil, fmt.Errorf("gitstore: object %s is a %s, not a commit", id, obj.kind)
	}
	commit, err := parseCommit(obj.data)
	if err != nil {
		return nil, err
	}
	tree := commit.tree
	return &tree, nil
}

// resolveSHA mirrors _resolve_sha (gitstore.py:174-195): walk the first-parent
// chain from HEAD and return the first commit whose full SHA starts with the
// given prefix. An unborn HEAD, a missing object or a malformed HEAD all resolve
// to "not found".
func (g *GitStore) resolveSHA(repo *repository, shortSHA string) (objectID, bool) {
	sha, ok, err := repo.headID()
	if err != nil || !ok {
		return objectID{}, false
	}
	for {
		if strings.HasPrefix(sha.String(), shortSHA) {
			return sha, true
		}
		obj, err := repo.store.get(sha)
		if err != nil {
			return objectID{}, false
		}
		if obj.kind != kindCommit {
			return objectID{}, false
		}
		commit, err := parseCommit(obj.data)
		if err != nil {
			return objectID{}, false
		}
		if len(commit.parents) == 0 {
			return objectID{}, false
		}
		sha = commit.parents[0]
	}
}

// SummarizeWorkingTree returns the structured summary of working-tree changes
// against HEAD for the given paths (gitstore.py:300-402).
//
// The exact string is part of the contract: memory.dream_content_diff compares
// it, so the format — including the singular/plural wording, the blank line
// before the diff fence and the trailing "```" with no newline — is reproduced
// verbatim.
func (g *GitStore) SummarizeWorkingTree(paths []string) (string, error) {
	repo := openRepository(g.workspace)
	if !repo.isInitialized() {
		return "", nil
	}
	out, err := g.summarize(repo, paths)
	if err != nil {
		return "", newGitStoreError("Git working-tree summary failed", err)
	}
	return out, nil
}

func (g *GitStore) summarize(repo *repository, paths []string) (string, error) {
	headTree, hasHead, err := repo.headTree()
	if err != nil {
		return "", err
	}

	var summaryLines, diffLines []string
	totalAdded, totalRemoved, changed := 0, 0, 0

	for _, path := range paths {
		headText := ""
		if hasHead {
			text, ok, err := readBlobFromTree(repo, headTree, path)
			if err != nil {
				return "", err
			}
			if ok {
				headText = text
			}
		}
		wtPath := pythonPathJoin(g.workspace, path)
		wtText := ""
		if _, err := os.Stat(wtPath); err == nil {
			data, err := os.ReadFile(wtPath)
			if err != nil {
				// A directory, an unreadable file: the reference lets the
				// exception escape and reports the whole summary as failed
				// (gitstore.py:384-385).
				return "", err
			}
			if !isValidUTF8(data) {
				// Non-UTF-8 working-tree file: record the change without a
				// unified diff (gitstore.py:356-363).
				changed++
				summaryLines = append(summaryLines, path+": binary or non-UTF-8 file changed")
				continue
			}
			wtText = string(data)
		}

		// CRLF and LF are equivalent; other newline differences are not
		// (gitstore.py:364-366).
		if strings.ReplaceAll(headText, "\r\n", "\n") == strings.ReplaceAll(wtText, "\r\n", "\n") {
			continue
		}
		headLines := splitLinesStr(headText)
		wtLines := splitLinesStr(wtText)
		changed++
		hunks := unifiedDiff(headLines, wtLines, path, path, 3, "")
		added, removed := 0, 0
		for _, line := range hunks {
			if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				added++
			}
			if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
				removed++
			}
		}
		totalAdded += added
		totalRemoved += removed
		summaryLines = append(summaryLines, fmt.Sprintf("%s: +%d -%d", path, added, removed))
		diffLines = append(diffLines, hunks...)
	}

	if changed == 0 {
		return "", nil
	}

	diffText := strings.Join(diffLines, "\n")
	if len(diffText) > workingTreeDiffMaxChars {
		diffText = diffText[:workingTreeDiffMaxChars] + "\n...[diff truncated]"
	}

	body := strings.Join(summaryLines, "\n")
	body += fmt.Sprintf("\n%d file%s changed, %d insertion%s(+), %d deletion%s(-)",
		changed, plural(changed), totalAdded, plural(totalAdded), totalRemoved, plural(totalRemoved))
	if len(diffLines) > 0 {
		body += "\n\n```diff\n" + diffText + "\n```"
	}
	return body, nil
}

// isValidUTF8 reports whether data is valid UTF-8, which is what Python's strict
// bytes.decode("utf-8") demands before it raises UnicodeDecodeError.
func isValidUTF8(data []byte) bool { return utf8.Valid(data) }

func plural(n int) string {
	if n != 1 {
		return "s"
	}
	return ""
}

// readBlobFromTree mirrors _read_blob_from_tree (gitstore.py:509-531): walk the
// path components through the tree and decode the blob with errors="replace".
//
// The reference returns a blob's data as soon as it meets one, even when path
// components remain, so "SOUL.md/extra" yields SOUL.md's content rather than
// nothing. That is reproduced here.
func readBlobFromTree(repo *repository, treeID objectID, treePath string) (string, bool, error) {
	parts := pythonPathParts(treePath)
	current, err := repo.store.get(treeID)
	if err != nil {
		return "", false, err
	}
	for _, part := range parts {
		if current.kind != kindTree {
			// Python raises TypeError indexing a non-tree here; the reference
			// reports the whole summary as failed.
			return "", false, fmt.Errorf("gitstore: object is a %s, not a tree", current.kind)
		}
		entries, err := parseTree(current.data)
		if err != nil {
			return "", false, err
		}
		var found *treeEntry
		for i := range entries {
			if entries[i].name == part {
				found = &entries[i]
				break
			}
		}
		if found == nil {
			return "", false, nil
		}
		child, err := repo.store.get(found.id)
		if err != nil {
			return "", false, err
		}
		switch child.kind {
		case kindBlob:
			return decodeUTF8Replace(child.data), true, nil
		case kindTree:
			current = child
		default:
			return "", false, nil
		}
	}
	return "", false, nil
}

// ShowCommitDiff finds a commit and returns it with its diff against its parent
// (gitstore.py:417-440). ok is false when no commit matches.
func (g *GitStore) ShowCommitDiff(shortSHA string, maxEntries int, messagePrefix *string) (CommitInfo, string, bool, error) {
	repo := openRepository(g.workspace)
	entry, diff, ok, err := g.showCommitDiff(repo, shortSHA, maxEntries, messagePrefix)
	if err != nil {
		return CommitInfo{}, "", false, newGitStoreError("Git commit display failed for "+shortSHA, err)
	}
	return entry, diff, ok, nil
}

func (g *GitStore) showCommitDiff(repo *repository, shortSHA string, maxEntries int, messagePrefix *string) (CommitInfo, string, bool, error) {
	commits, err := g.log(repo, maxEntries, messagePrefix)
	if err != nil {
		return CommitInfo{}, "", false, err
	}
	for _, c := range commits {
		if !strings.HasPrefix(c.SHA, shortSHA) {
			continue
		}
		full, ok := g.resolveSHA(repo, c.SHA)
		if !ok {
			return CommitInfo{}, "", false, nil
		}
		obj, err := repo.store.get(full)
		if err != nil {
			return CommitInfo{}, "", false, err
		}
		commit, err := parseCommit(obj.data)
		if err != nil {
			return CommitInfo{}, "", false, err
		}
		diff := ""
		if len(commit.parents) > 0 {
			diff, err = g.diffCommits(repo, commit.parents[0].short(), c.SHA)
			if err != nil {
				return CommitInfo{}, "", false, err
			}
		}
		return c, diff, true, nil
	}
	return CommitInfo{}, "", false, nil
}

// ---------------------------------------------------------------------------
// restore
// ---------------------------------------------------------------------------

// Revert undoes the changes a commit introduced: it restores every tracked file
// to its state in the commit's parent tree and commits that state
// (gitstore.py:444-507). ok is false when the commit cannot be reverted.
func (g *GitStore) Revert(commit string, messagePrefix *string) (string, bool, error) {
	repo := openRepository(g.workspace)
	if !repo.isInitialized() {
		return "", false, nil
	}
	sha, err := g.revert(repo, commit, messagePrefix)
	if err != nil {
		return "", false, newGitStoreError("Git revert failed for "+commit, err)
	}
	return sha, sha != "", nil
}

func (g *GitStore) revert(repo *repository, commit string, messagePrefix *string) (string, error) {
	full, ok := g.resolveSHA(repo, commit)
	if !ok {
		return "", nil
	}
	obj, err := repo.store.get(full)
	if err != nil {
		return "", err
	}
	if obj.kind != kindCommit {
		return "", nil
	}
	typed, err := parseCommit(obj.data)
	if err != nil {
		return "", err
	}
	message := pythonStrip(decodeUTF8Replace(typed.message))
	if messagePrefix != nil && !pythonStartsWith(message, *messagePrefix) {
		return "", nil
	}
	if len(typed.parents) == 0 {
		return "", nil
	}
	parentObj, err := repo.store.get(typed.parents[0])
	if err != nil {
		return "", err
	}
	parent, err := parseCommit(parentObj.data)
	if err != nil {
		return "", err
	}

	var restored []string
	for _, filepathArg := range g.trackedFiles {
		content, ok, err := readBlobFromTree(repo, parent.tree, filepathArg)
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}
		dest := pythonPathJoin(g.workspace, filepathArg)
		// write_text does not create parent directories, and the reference
		// writes the *replaced* decoding, so a non-UTF-8 blob is rewritten
		// lossily — both behaviours are reproduced (gitstore.py:497).
		if err := os.WriteFile(dest, []byte(content), 0o666); err != nil {
			return "", err
		}
		restored = append(restored, filepathArg)
	}
	if len(restored) == 0 {
		return "", nil
	}
	return g.autoCommit(repo, fmt.Sprintf("revert: undo %s", commit))
}
