// GitStore differential test.
//
// compat/python/dump_reference.py runs a scripted scenario against the frozen
// Python GitStore and records every observable value; this file runs the same
// scenario against the Go port and compares the two. The scenario is also
// reproduced in the same fixed directory on both sides, which lets the git CLI
// act as an independent oracle over the two repositories afterwards.
//
// Two things are deliberately NOT compared literally:
//
//   - commit ids. The reference stamps commits with time.time() (dulwich's
//     porcelain.commit), so the two sides can never produce the same commit id.
//     The dumper records their shape instead, and the tree contents are compared
//     exactly through the git oracle below — a tree id covers every path, mode
//     and blob id in the repository.
//   - the workspace path, which differs between the two sides.
//
// Each side writes its scenario repositories under a per-process directory (the
// Go side tells the dumper where to write through GITSTORE_SCRATCH_DIR), so two
// concurrent test processes cannot delete each other's repositories and this
// process always compares against the repositories its own dumper run produced.
package compat

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/gitstore"
)

// The Go-side scenario directories. They mirror the names the dumper uses under
// .tools/tmp so that both repositories can be inspected side by side.
const (
	goWSName      = "gitstore-go-ws"
	goUnbornName  = "gitstore-go-unborn"
	goOutsideName = "gitstore-go-outside.txt"
	goFileName    = "gitstore-go-file"
	goBlankName   = "gitstore-go-blank"
	goNestedName  = "gitstore-go-nested"
	goDirName     = "gitstore-go-dir"
	goCorruptName = "gitstore-go-corrupt"
	goMissingName = "gitstore-go-missing"
)

var gitStoreTracked = []string{"SOUL.md", "USER.md", "memory/MEMORY.md", "memory/.dream_cursor"}

func TestGitStoreMatchesPythonReference(t *testing.T) {
	ref := loadReference(t)
	if len(ref.GitStore) == 0 {
		t.Fatal("reference produced no gitstore data")
	}
	root := repoRoot(t)
	got := runGoGitStoreScenario(t, root)

	var keys []string
	for k := range ref.GitStore {
		keys = append(keys, k)
	}
	for k := range got {
		if _, ok := ref.GitStore[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	compared := 0
	for _, k := range keys {
		want, inRef := ref.GitStore[k]
		have, inGo := got[k]
		switch {
		case !inRef:
			t.Errorf("gitstore.%s: produced by the Go side but absent from the reference", k)
			continue
		case !inGo:
			t.Errorf("gitstore.%s: recorded by the reference but not produced by the Go side", k)
			continue
		}
		if !reflect.DeepEqual(asJSON(t, have), want) {
			t.Errorf("gitstore.%s mismatch\n got: %s\nwant: %s", k, asJSONText(t, have), asJSONText(t, want))
			continue
		}
		compared++
	}
	t.Logf("compared %d gitstore values against the Python reference", compared)
}

// asJSON round-trips a Go value through encoding/json so that numbers and maps
// compare equal to the values decoded from the reference document.
func asJSON(t *testing.T, v any) any {
	t.Helper()
	return asJSONText(t, v)
}

func asJSONText(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal %T: %v", v, err)
	}
	return out
}

// ---------------------------------------------------------------------------
// The scenario
// ---------------------------------------------------------------------------

// callResult mirrors the dumper's _call helper: either the value or the error.
func callResult(value any, err error) map[string]any {
	if err != nil {
		out := map[string]any{"ok": false, "message": err.Error()}
		var gsErr *gitstore.GitStoreError
		if ok := asGitStoreError(err, &gsErr); ok {
			out["type"] = "GitStoreError"
		} else {
			out["type"] = fmt.Sprintf("%T", err)
		}
		return out
	}
	return map[string]any{"ok": true, "value": value}
}

func asGitStoreError(err error, target **gitstore.GitStoreError) bool {
	if e, ok := err.(*gitstore.GitStoreError); ok {
		*target = e
		return true
	}
	return false
}

// normalizedCall is callResult with the workspace path replaced by a token, the
// way the reference dumper normalises it.
func normalizedCall(t *testing.T, value any, err error, norm func(string) string) map[string]any {
	t.Helper()
	out := callResult(value, err)
	if msg, ok := out["message"].(string); ok {
		out["message"] = norm(msg)
	}
	return out
}

// shaShape describes a commit id without pinning its value.
func shaShape(sha string, committed bool) any {
	if !committed {
		return nil
	}
	hex := true
	for _, r := range sha {
		if !strings.ContainsRune("0123456789abcdef", r) {
			hex = false
		}
	}
	return map[string]any{"len": len(sha), "hex": hex}
}

// appendFileAt adds content to an existing file, creating it when needed.
func appendFileAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeFileAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFileAt(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return "<" + fmt.Sprintf("%T", err) + ">"
	}
	return string(data)
}

// gitEntries lists .git paths the way the dumper does, collapsing loose objects.
func gitEntries(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		parts := strings.Split(rel, "/")
		if parts[0] == "objects" && len(parts) >= 2 && len(parts[1]) == 2 {
			return nil
		}
		if fi.IsDir() {
			rel += "/"
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

func freshDir(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o777); err != nil {
		t.Fatal(err)
	}
}

// gitStoreScratchSuffix keeps this process's scenario directories apart from any
// other test process running the same suite in this workspace: a shared scenario
// directory would let two runs delete each other's repositories mid-scenario.
func gitStoreScratchSuffix() string { return fmt.Sprintf("%d", os.Getpid()) }

// useScratchDir tells the reference dumper where to write its scenario
// repositories and returns that directory. The dumper runs as a child process
// and inherits this environment, so the value identifies the repositories
// produced by *this* process's dumper run — reading them from a fixed path would
// risk comparing against a stale directory left by an earlier run.
func useScratchDir(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, ".tools", "tmp", "gitstore-ref-"+gitStoreScratchSuffix())
	// Remove anything left behind first: the checks below must fail loudly if the
	// dumper does not actually produce the repositories, rather than compare
	// against a directory an earlier run left there.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("GITSTORE_SCRATCH_DIR", dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// goScratchDir is where the port's own scenario repositories live.
func goScratchDir(root string) string {
	dir := filepath.Join(root, ".tools", "tmp", "gitstore-go-"+gitStoreScratchSuffix())
	_ = os.MkdirAll(dir, 0o777)
	return dir
}

// commitTimestampRE matches the "YYYY-MM-DD HH:MM" stamp CommitInfo.Format writes.
var commitTimestampRE = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}`)

// runGoGitStoreScenario performs exactly the operations dump_reference.py
// performs and returns the same structure of observations.
func runGoGitStoreScenario(t *testing.T, root string) map[string]any {
	t.Helper()
	tmp := goScratchDir(root)
	ws := filepath.Join(tmp, goWSName)
	unbornWS := filepath.Join(tmp, goUnbornName)
	outside := filepath.Join(tmp, goOutsideName)
	asFile := filepath.Join(tmp, goFileName)

	freshDir(t, ws)
	freshDir(t, unbornWS)
	writeFileAt(t, outside, "outside line\n")

	norm := func(s string) string {
		for _, r := range [][2]string{
			{ws, "<WS>"}, {unbornWS, "<UNBORN>"}, {outside, "<OUT>"}, {asFile, "<FILE>"},
		} {
			s = strings.ReplaceAll(s, r[0], r[1])
		}
		// Commit timestamps come from the clock and the reference is produced
		// seconds earlier: normalising them keeps a run that crosses a minute
		// boundary from reporting a spurious mismatch. Their shape is pinned by
		// the timestamp_len / timestamp_shape values instead.
		return commitTimestampRE.ReplaceAllString(s, "<TS>")
	}

	// The reference runs the whole scenario with PATH emptied, to prove it never
	// shells out to git. The Go port must not need it either.
	savedPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", ""); err != nil {
		t.Fatal(err)
	}
	defer os.Setenv("PATH", savedPath)

	out := map[string]any{}

	// -- 1. init ------------------------------------------------------------
	gs := gitstore.New(ws, gitStoreTracked)
	out["is_initialized_before"] = gs.IsInitialized()
	created, err := gs.Init()
	out["init_created"] = callResult(created, err)
	again, err := gs.Init()
	out["init_again"] = callResult(again, err)
	out["gitignore"] = readFileAt(t, filepath.Join(ws, ".gitignore"))
	out["head_file"] = readFileAt(t, filepath.Join(ws, ".git", "HEAD"))
	out["config_file"] = readFileAt(t, filepath.Join(ws, ".git", "config"))
	out["description_file"] = readFileAt(t, filepath.Join(ws, ".git", "description"))
	exclude, err := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	out["exclude_file_len"] = len(exclude)
	out["git_entries"] = gitEntries(t, filepath.Join(ws, ".git"))
	out["is_initialized_after"] = gs.IsInitialized()
	out["gitignore_other_tracked"] = gitignoreOf(t, tmp, "gitstore-go-gi1", []string{"SOUL.md"})
	out["gitignore_flat"] = gitignoreOf(t, tmp, "gitstore-go-gi2", []string{"a.md", "b.md"})

	initLog, err := gs.Log(gitstore.DefaultLogEntries, nil)
	if err != nil {
		t.Fatal(err)
	}
	var initLogOut []map[string]any
	for _, c := range initLog {
		initLogOut = append(initLogOut, map[string]any{
			"subject":         c.Subject(),
			"message":         c.Message,
			"sha_len":         len(c.SHA),
			"sha_is_hex":      isHex(c.SHA),
			"timestamp_len":   len(c.Timestamp),
			"timestamp_shape": timestampShape(c.Timestamp),
			"format":          norm(strings.ReplaceAll(c.Format(""), c.SHA, "<SHA>")),
		})
	}
	out["init_log"] = initLogOut
	out["summary_clean"] = mustSummarize(t, gs, []string{"SOUL.md", "USER.md", "memory/MEMORY.md"})

	// -- 2. working-tree summary formats ------------------------------------
	writeFileAt(t, filepath.Join(ws, "SOUL.md"), "hello\nworld\n")
	writeFileAt(t, filepath.Join(ws, "USER.md"), "user line\n")
	summaryPaths := []string{"SOUL.md", "USER.md", "memory/MEMORY.md"}
	out["summary_added"] = mustSummarize(t, gs, summaryPaths)
	out["summary_duplicate"] = mustSummarize(t, gs, []string{"SOUL.md", "SOUL.md"})
	out["summary_dot_prefix"] = mustSummarize(t, gs, []string{"./SOUL.md"})
	out["summary_missing_tracked"] = mustSummarize(t, gs, []string{"memory/MEMORY.md"})
	out["summary_absolute"] = norm(mustSummarize(t, gs, []string{outside}))
	out["summary_empty_list"] = mustSummarize(t, gs, nil)

	// -- 3. commit, log, diff ------------------------------------------------
	second, committed, err := gs.AutoCommit("dream: second")
	if err != nil {
		t.Fatal(err)
	}
	out["commit_second"] = shaShape(second, committed)
	out["summary_after_commit"] = mustSummarize(t, gs, summaryPaths)
	clean, committed, err := gs.AutoCommit("dream: second")
	if err != nil {
		t.Fatal(err)
	}
	out["commit_clean"] = shaShape(clean, committed)

	log, err := gs.Log(gitstore.DefaultLogEntries, nil)
	if err != nil {
		t.Fatal(err)
	}
	out["log_subjects"] = subjects(log)
	zero, err := gs.Log(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	out["log_zero"] = subjects(zero)
	negative, err := gs.Log(-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	out["log_negative"] = subjects(negative)
	one, err := gs.Log(1, nil)
	if err != nil {
		t.Fatal(err)
	}
	out["log_one"] = subjects(one)
	prefix := "dream:"
	withPrefix, err := gs.Log(gitstore.DefaultLogEntries, &prefix)
	if err != nil {
		t.Fatal(err)
	}
	out["log_prefix_match"] = subjects(withPrefix)
	noMatch := "zzz"
	none, err := gs.Log(gitstore.DefaultLogEntries, &noMatch)
	if err != nil {
		t.Fatal(err)
	}
	out["log_prefix_none_match"] = subjects(none)
	empty := ""
	all, err := gs.Log(gitstore.DefaultLogEntries, &empty)
	if err != nil {
		t.Fatal(err)
	}
	out["log_prefix_empty"] = subjects(all)

	initSHA, secondSHA := log[len(log)-1].SHA, log[0].SHA
	out["diff_init_second"] = norm(mustDiff(t, gs, initSHA, secondSHA))
	out["diff_same"] = mustDiff(t, gs, secondSHA, secondSHA)
	out["diff_unknown"] = mustDiff(t, gs, "deadbeef", secondSHA)
	out["diff_reversed"] = norm(mustDiff(t, gs, secondSHA, initSHA))

	info, diff, ok, err := gs.ShowCommitDiff(secondSHA, gitstore.DefaultLogEntries, nil)
	if err != nil {
		out["show_commit_diff"] = callResult(nil, err)
	} else if !ok {
		out["show_commit_diff"] = map[string]any{"ok": true, "value": nil}
	} else {
		out["show_commit_diff"] = map[string]any{"ok": true, "value": map[string]any{
			"subject": info.Subject(), "sha_len": len(info.SHA), "diff": norm(diff),
		}}
	}
	_, _, found, err := gs.ShowCommitDiff("ffffffff", gitstore.DefaultLogEntries, nil)
	out["show_commit_diff_no_match"] = callResult(showValue(found), err)
	_, _, found, err = gs.ShowCommitDiff(secondSHA, gitstore.DefaultLogEntries, &noMatch)
	out["show_commit_diff_prefix_filter"] = callResult(showValue(found), err)

	// -- 4. content shapes ---------------------------------------------------
	writeFileAt(t, filepath.Join(ws, "SOUL.md"), "hello\r\nworld\r\n")
	out["summary_crlf_equal"] = mustSummarize(t, gs, []string{"SOUL.md"})
	writeFileAt(t, filepath.Join(ws, "SOUL.md"), "hello\rworld\r")
	out["summary_cr_only"] = mustSummarize(t, gs, []string{"SOUL.md"})
	writeFileAt(t, filepath.Join(ws, "SOUL.md"), "hello\rworld\r\n")
	out["summary_crlf_partial"] = mustSummarize(t, gs, []string{"SOUL.md"})
	writeFileAt(t, filepath.Join(ws, "SOUL.md"), "hello\nworld")
	out["summary_no_trailing_newline"] = mustSummarize(t, gs, []string{"SOUL.md"})
	writeFileAt(t, filepath.Join(ws, "SOUL.md"), "hello\nworld\n")

	if err := os.WriteFile(filepath.Join(ws, "USER.md"), []byte("\xff\xfe\x00binary\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out["summary_binary"] = mustSummarize(t, gs, summaryPaths)
	writeFileAt(t, filepath.Join(ws, "USER.md"), "user line\n")

	// str.splitlines() breaks on more than \n and \r: \v \f \x1c \x1d \x1e \x85
	// U+2028 U+2029 all end a line.
	writeFileAt(t, filepath.Join(ws, "SOUL.md"), "A\x0bB\x0cC\x1cD\x1dE\x1eF\u0085G\u2028H\u2029I\n")
	out["summary_exotic_separators"] = mustSummarize(t, gs, []string{"SOUL.md"})
	writeFileAt(t, filepath.Join(ws, "SOUL.md"), "hello\nworld\n")

	// A committed blob that is not valid UTF-8: the HEAD side is decoded with
	// errors="replace", which collapses a truncated multi-byte sequence into ONE
	// replacement character (the Unicode "maximal subpart" rule).
	if err := os.WriteFile(filepath.Join(ws, "USER.md"), []byte("\xf0\x9f\x98ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gs.AutoCommit("dream: invalid utf8 blob"); err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, filepath.Join(ws, "USER.md"), "plain\n")
	out["summary_invalid_utf8_head"] = mustSummarize(t, gs, []string{"USER.md"})
	writeFileAt(t, filepath.Join(ws, "USER.md"), "user line\n")

	// Two hunks separated by a long unchanged run: exercises the diff grouping.
	var bodyHead, bodyNew strings.Builder
	for i := 0; i < 60; i++ {
		line := fmt.Sprintf("line %d\n", i)
		bodyHead.WriteString(line)
		switch i {
		case 3:
			bodyNew.WriteString("changed early\n")
		case 56:
			bodyNew.WriteString("changed late\n")
		default:
			bodyNew.WriteString(line)
		}
	}
	writeFileAt(t, filepath.Join(ws, "SOUL.md"), bodyHead.String())
	if _, _, err := gs.AutoCommit("dream: grouping base"); err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, filepath.Join(ws, "SOUL.md"), bodyNew.String())
	out["summary_two_hunks"] = mustSummarize(t, gs, []string{"SOUL.md"})

	// 300 lines with one value repeated far more often than difflib's autojunk
	// heuristic allows (n >= 200, popular = count > n//100 + 1).
	var popular, popularNew strings.Builder
	for i := 0; i < 250; i++ {
		popular.WriteString("x\n")
		popularNew.WriteString("x\n")
	}
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&popular, "u%d\n", i)
		fmt.Fprintf(&popularNew, "v%d\n", i)
	}
	writeFileAt(t, filepath.Join(ws, "SOUL.md"), popular.String())
	if _, _, err := gs.AutoCommit("dream: autojunk base"); err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, filepath.Join(ws, "SOUL.md"), popularNew.String())
	out["summary_autojunk"] = mustSummarize(t, gs, []string{"SOUL.md"})
	writeFileAt(t, filepath.Join(ws, "SOUL.md"), "hello\nworld\n")

	var big strings.Builder
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&big, "line %d\n", i)
	}
	writeFileAt(t, filepath.Join(ws, "SOUL.md"), big.String())
	truncated := mustSummarize(t, gs, []string{"SOUL.md"})
	out["summary_truncated_len"] = utf8.RuneCountInString(truncated)
	out["summary_truncated_tail"] = lastRunes(truncated, 60)
	out["summary_truncated_head"] = firstRunes(truncated, 120)
	writeFileAt(t, filepath.Join(ws, "SOUL.md"), "hello\nworld\n")

	// -- 5. revert -----------------------------------------------------------
	revertSHA, reverted, err := gs.Revert(secondSHA, nil)
	if err != nil {
		t.Fatal(err)
	}
	out["revert_created"] = shaShape(revertSHA, reverted)
	out["revert_soul"] = readFileAt(t, filepath.Join(ws, "SOUL.md"))
	out["revert_user"] = readFileAt(t, filepath.Join(ws, "USER.md"))
	out["revert_summary"] = mustSummarize(t, gs, summaryPaths)
	revertLog, err := gs.Log(gitstore.DefaultLogEntries, nil)
	if err != nil {
		t.Fatal(err)
	}
	out["revert_message"] = norm(strings.ReplaceAll(revertLog[0].Message, secondSHA, "<SHA>"))
	// The reference records the raw return value here (None means "nothing to
	// revert"), not a wrapped call.
	rootSHA, rootReverted, err := gs.Revert(initSHA, nil)
	if err != nil {
		t.Fatal(err)
	}
	out["revert_root"] = shaShape(rootSHA, rootReverted)
	noMatchSHA, noMatchReverted, err := gs.Revert(secondSHA, &noMatch)
	if err != nil {
		t.Fatal(err)
	}
	out["revert_prefix_no_match"] = shaShape(noMatchSHA, noMatchReverted)
	unknownSHA, unknownReverted, err := gs.Revert("ffffffff", nil)
	if err != nil {
		t.Fatal(err)
	}
	out["revert_unknown"] = shaShape(unknownSHA, unknownReverted)

	// -- 5b. patch shapes ----------------------------------------------------
	writeFileAt(t, filepath.Join(ws, "SOUL.md"), "hello\nworld")
	if err := os.Chmod(filepath.Join(ws, "SOUL.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(ws, "USER.md")); err != nil {
		t.Fatal(err)
	}
	shapesSHA, shapesCommitted, err := gs.AutoCommit("dream: shapes")
	if err != nil {
		t.Fatal(err)
	}
	out["commit_shapes"] = shaShape(shapesSHA, shapesCommitted)
	shapesLog, err := gs.Log(gitstore.DefaultLogEntries, nil)
	if err != nil {
		t.Fatal(err)
	}
	out["diff_shapes"] = norm(mustDiff(t, gs, shapesLog[1].SHA, shapesLog[0].SHA))
	out["summary_shapes"] = mustSummarize(t, gs, summaryPaths)

	// -- 6. store that was never initialized --------------------------------
	blank := filepath.Join(tmp, goBlankName)
	freshDir(t, blank)
	blankGS := gitstore.New(blank, gitStoreTracked)
	blankSHA, blankCommitted, blankErr := blankGS.AutoCommit("x")
	blankLog, blankLogErr := blankGS.Log(gitstore.DefaultLogEntries, nil)
	blankDiff, blankDiffErr := blankGS.DiffCommits("a", "b")
	blankSummary, blankSummaryErr := blankGS.SummarizeWorkingTree([]string{"SOUL.md"})
	_, _, blankShow, blankShowErr := blankGS.ShowCommitDiff("a", gitstore.DefaultLogEntries, nil)
	blankRevert, blankReverted, blankRevertErr := blankGS.Revert("a", nil)
	if blankErr != nil || blankLogErr != nil || blankDiffErr != nil || blankSummaryErr != nil ||
		blankShowErr != nil || blankRevertErr != nil {
		t.Fatalf("uninitialized store returned errors: %v %v %v %v %v %v",
			blankErr, blankLogErr, blankDiffErr, blankSummaryErr, blankShowErr, blankRevertErr)
	}
	out["uninitialized"] = map[string]any{
		"is_initialized": blankGS.IsInitialized(),
		"auto_commit":    shaShape(blankSHA, blankCommitted),
		"log":            subjects(blankLog),
		"diff":           blankDiff,
		"summary":        blankSummary,
		"show":           showValue(blankShow),
		"revert":         shaShape(blankRevert, blankReverted),
		"entries":        dirEntries(t, blank),
	}
	blankCreated, blankInitErr := blankGS.Init()
	out["init_after_noop"] = callResult(blankCreated, blankInitErr)
	out["init_after_noop_entries"] = dirEntries(t, blank)

	// -- 7. unborn HEAD in a hand-made repository ---------------------------
	for _, dir := range []string{"objects/info", "objects/pack", "refs/heads"} {
		if err := os.MkdirAll(filepath.Join(unbornWS, ".git", filepath.FromSlash(dir)), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	writeFileAt(t, filepath.Join(unbornWS, ".git", "HEAD"), "ref: refs/heads/master\n")
	unborn := gitstore.New(unbornWS, gitStoreTracked)
	writeFileAt(t, filepath.Join(unbornWS, "SOUL.md"), "x\n")
	unbornLog, err := unborn.Log(gitstore.DefaultLogEntries, nil)
	if err != nil {
		t.Fatal(err)
	}
	unbornDiff, err := unborn.DiffCommits("deadbeef", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	unbornSummary := mustSummarize(t, unborn, []string{"SOUL.md"})
	unbornCommit, unbornCommitted, err := unborn.AutoCommit("dream: unborn")
	if err != nil {
		t.Fatal(err)
	}
	unbornAfter, err := unborn.Log(gitstore.DefaultLogEntries, nil)
	if err != nil {
		t.Fatal(err)
	}
	out["unborn"] = map[string]any{
		"is_initialized": unborn.IsInitialized(),
		"log":            subjects(unbornLog),
		"diff":           unbornDiff,
		"summary":        unbornSummary,
		"commit":         shaShape(unbornCommit, unbornCommitted),
		"summary_after":  mustSummarize(t, unborn, []string{"SOUL.md"}),
		"subjects":       subjects(unbornAfter),
		"entries":        gitEntries(t, filepath.Join(unbornWS, ".git")),
	}

	// -- 8. ignore sources ---------------------------------------------------
	out["ignores"] = goGitStoreIgnores(t, tmp)

	// -- 9. failure paths ----------------------------------------------------
	out["failures"] = goGitStoreFailures(t, root, tmp, ws, norm)
	return out
}

// goGitStoreIgnores mirrors the dumper's ignore-source scenarios: the user's
// global ignore file, .git/info/exclude and nested .gitignore files all decide
// whether a tracked file gets staged.
func goGitStoreIgnores(t *testing.T, tmp string) map[string]any {
	t.Helper()
	cases := []struct {
		name        string
		global      string
		configExtra string
		extraRel    string
		extraBody   string
		target      string
	}{
		{"control", "*.tmp\n", "", "", "", "SOUL.md"},
		{"global_md", "*.md\n", "", "", "", "SOUL.md"},
		{"info_exclude", "*.tmp\n", "", "info/exclude", "SOUL.md\n", "SOUL.md"},
		{"nested_ignored", "*.tmp\n", "", "memory/.gitignore", "MEMORY.md\n", "memory/MEMORY.md"},
		{"nested_negated", "*.tmp\n", "", "memory/.gitignore", "!MEMORY.md\n", "memory/MEMORY.md"},
		{"nested_new_file", "*.tmp\n", "", "memory/.gitignore", "*.md\n", "SOUL.md"},
		// core.ignorecase reaches the .gitignore filters only, and dulwich parses
		// it with get_boolean, which accepts nothing but true/false.
		{"ignorecase_true_gitignore", "*.tmp\n", "ignorecase = true", ".gitignore", "soul.md\n", "SOUL.md"},
		{"ignorecase_false_gitignore", "*.tmp\n", "ignorecase = false", ".gitignore", "soul.md\n", "SOUL.md"},
		{"ignorecase_true_exclude", "*.tmp\n", "ignorecase = true", "info/exclude", "soul.md\n", "SOUL.md"},
		{"ignorecase_bad", "*.tmp\n", "ignorecase = 1", "", "", "SOUL.md"},
	}
	out := map[string]any{}
	for _, tc := range cases {
		ws := filepath.Join(tmp, "gitstore-go-ign-"+tc.name)
		freshDir(t, ws)
		userIgnore := filepath.Join(tmp, "gitstore-go-ign-"+tc.name+".ignore")
		writeFileAt(t, userIgnore, tc.global)

		gs := gitstore.New(ws, gitStoreTracked)
		if _, err := gs.Init(); err != nil {
			t.Fatal(err)
		}
		// Point the repository at this scenario's user ignore file.
		config, err := os.ReadFile(filepath.Join(ws, ".git", "config"))
		if err != nil {
			t.Fatal(err)
		}
		config = append(config, []byte("[core]\n\texcludesFile = "+userIgnore+"\n")...)
		if tc.configExtra != "" {
			config = append(config, []byte("\t"+tc.configExtra+"\n")...)
		}
		if err := os.WriteFile(filepath.Join(ws, ".git", "config"), config, 0o644); err != nil {
			t.Fatal(err)
		}
		if tc.extraRel != "" {
			path := filepath.Join(ws, tc.extraRel)
			if strings.HasPrefix(tc.extraRel, "info/") {
				path = filepath.Join(ws, ".git", filepath.FromSlash(tc.extraRel))
			}
			// Appended, not written: Init() has already created .gitignore.
			appendFileAt(t, path, tc.extraBody)
		}
		writeFileAt(t, filepath.Join(ws, tc.target), "changed\n")

		sha, committed, err := gs.AutoCommit("dream: probe")
		if err != nil {
			var gse *gitstore.GitStoreError
			if !errors.As(err, &gse) {
				t.Fatalf("%s: AutoCommit error = %v, want a *GitStoreError", tc.name, err)
			}
			out[tc.name] = map[string]any{"error": gse.Error(), "error_type": "GitStoreError"}
			continue
		}
		summary, err := gs.SummarizeWorkingTree([]string{tc.target})
		if err != nil {
			t.Fatalf("%s: SummarizeWorkingTree: %v", tc.name, err)
		}
		entries, err := gs.Log(gitstore.DefaultLogEntries, nil)
		if err != nil {
			t.Fatalf("%s: Log: %v", tc.name, err)
		}
		out[tc.name] = map[string]any{
			"committed":     committed && sha != "",
			"summary_after": summary,
			"subjects":      subjects(entries),
		}
	}
	return out
}

func goGitStoreFailures(t *testing.T, root, tmp, ws string, norm func(string) string) map[string]any {
	t.Helper()
	out := map[string]any{}

	// A workspace that is a regular file.
	asFile := filepath.Join(tmp, goFileName)
	writeFileAt(t, asFile, "not a directory\n")
	_, err := gitstore.New(asFile, []string{"SOUL.md"}).Init()
	out["init_on_file"] = normalizedCall(t, nil, err, norm)

	// A workspace inside another repository: init() declines, writes nothing.
	nestedParent := filepath.Join(tmp, goNestedName)
	freshDir(t, nestedParent)
	if err := os.MkdirAll(filepath.Join(nestedParent, ".git"), 0o777); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(nestedParent, "inner")
	freshDir(t, nested)
	nestedGS := gitstore.New(nested, gitStoreTracked)
	nestedCreated, err := nestedGS.Init()
	out["nested_init"] = callResult(nestedCreated, err)
	out["nested_entries"] = dirEntries(t, nested)

	// A tracked path that is a directory.
	dirWS := filepath.Join(tmp, goDirName)
	freshDir(t, dirWS)
	dirGS := gitstore.New(dirWS, gitStoreTracked)
	if _, err := dirGS.Init(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dirWS, "SOUL.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dirWS, "SOUL.md"), 0o777); err != nil {
		t.Fatal(err)
	}
	_, err = dirGS.SummarizeWorkingTree([]string{"SOUL.md"})
	out["summary_directory"] = callResult(nil, err)

	// A corrupt index: auto_commit fails, log and summary still work.
	corrupt := filepath.Join(tmp, goCorruptName)
	freshDir(t, corrupt)
	corruptGS := gitstore.New(corrupt, gitStoreTracked)
	if _, err := corruptGS.Init(); err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, filepath.Join(corrupt, "SOUL.md"), "changed\n")
	indexPath := filepath.Join(corrupt, ".git", "index")
	goodIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, append([]byte("GARBAGE"), goodIndex[7:]...), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = corruptGS.AutoCommit("dream: corrupt")
	failure := callResult(nil, err)
	if msg, ok := failure["message"].(string); ok {
		failure["prefix"] = strings.HasPrefix(msg, "Git auto-commit failed: ")
	}
	out["auto_commit_corrupt_index"] = failure
	out["summary_corrupt_index"] = mustSummarize(t, corruptGS, []string{"SOUL.md"})
	if err := os.WriteFile(indexPath, goodIndex, 0o644); err != nil {
		t.Fatal(err)
	}

	// A garbage HEAD: log fails, auto_commit and show report a failure.
	headPath := filepath.Join(corrupt, ".git", "HEAD")
	goodHead, err := os.ReadFile(headPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(headPath, []byte("garbage-not-a-ref\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = corruptGS.Log(gitstore.DefaultLogEntries, nil)
	out["log_garbage_head"] = callResult(nil, err)
	_, _, err = corruptGS.AutoCommit("dream: bad head")
	out["auto_commit_garbage_head"] = callResult(nil, err)
	_, _, _, err = corruptGS.ShowCommitDiff("aaaa", gitstore.DefaultLogEntries, nil)
	out["show_garbage_head"] = callResult(nil, err)
	if err := os.WriteFile(headPath, goodHead, 0o644); err != nil {
		t.Fatal(err)
	}

	// A missing blob object: the summary fails, log still works.
	missing := filepath.Join(tmp, goMissingName)
	freshDir(t, missing)
	missingGS := gitstore.New(missing, gitStoreTracked)
	if _, err := missingGS.Init(); err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, filepath.Join(missing, "SOUL.md"), "hello\n")
	if _, _, err := missingGS.AutoCommit("dream: present"); err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, filepath.Join(missing, "SOUL.md"), "hello again\n")
	blob := strings.TrimSpace(gitRunCompat(t, missing, "rev-parse", "HEAD:SOUL.md"))
	if err := os.Remove(filepath.Join(missing, ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	_, err = missingGS.SummarizeWorkingTree([]string{"SOUL.md"})
	out["summary_missing_object"] = callResult(nil, err)
	missingLog, logErr := missingGS.Log(gitstore.DefaultLogEntries, nil)
	out["log_missing_object"] = callResult(subjects(missingLog), logErr)

	return out
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// gitBinary locates the git CLI without relying on PATH, because the scenario
// runs with PATH emptied (the reference does the same, and neither side may
// depend on the git binary at runtime).
func gitBinary(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/usr/bin/git", "/bin/git", "/usr/local/bin/git", "/opt/homebrew/bin/git"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("git"); err == nil {
		return p
	}
	t.Skip("git is not installed; skipping the git-oracle comparison")
	return ""
}

// gitRunCompat runs git in dir with a hermetic configuration.
func gitRunCompat(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gitBinary(t), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH=/usr/bin:/bin:/usr/local/bin",
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

// shaPattern normalises commit ids in git output: the two sides commit at
// different times, so their commit ids necessarily differ.
var shaPattern = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)

func normalizeGitOutput(s string) string { return shaPattern.ReplaceAllString(s, "<SHA>") }

// gitignoreOf initializes a throwaway workspace with the given tracked files and
// returns the .gitignore the store wrote, which is the observable form of
// _build_gitignore.
func gitignoreOf(t *testing.T, tmp, name string, tracked []string) string {
	t.Helper()
	dir := filepath.Join(tmp, name)
	freshDir(t, dir)
	gs := gitstore.New(dir, tracked)
	if _, err := gs.Init(); err != nil {
		t.Fatalf("Init(%s): %v", name, err)
	}
	return readFileAt(t, filepath.Join(dir, ".gitignore"))
}

func subjects(entries []gitstore.CommitInfo) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Subject())
	}
	return out
}

func showValue(ok bool) any {
	if !ok {
		return nil
	}
	return true
}

func mustSummarize(t *testing.T, gs *gitstore.GitStore, paths []string) string {
	t.Helper()
	out, err := gs.SummarizeWorkingTree(paths)
	if err != nil {
		t.Fatalf("SummarizeWorkingTree(%v): %v", paths, err)
	}
	return out
}

func mustDiff(t *testing.T, gs *gitstore.GitStore, a, b string) string {
	t.Helper()
	out, err := gs.DiffCommits(a, b)
	if err != nil {
		t.Fatalf("DiffCommits(%s, %s): %v", a, b, err)
	}
	return out
}

func isHex(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return len(s) > 0
}

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

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

// ---------------------------------------------------------------------------
// git as an independent oracle
// ---------------------------------------------------------------------------

// TestGitStoreRepositoriesMatchViaGitOracle compares the repository the Python
// reference produced with the one the Go port produced, using git itself: tree
// ids cover every path, mode and blob id, so an equal tree id means the two
// implementations committed byte-identical content.
func TestGitStoreRepositoriesMatchViaGitOracle(t *testing.T) {
	root := repoRoot(t)
	refWS := useScratchDir(t, root)
	ref := loadReference(t)
	if len(ref.GitStore) == 0 {
		t.Fatal("reference produced no gitstore data")
	}
	runGoGitStoreScenario(t, root)

	// The dumper keeps its workspaces directly under the scratch directory; the
	// port's scenario directories keep their own names one level down.
	refWS = filepath.Join(refWS, "ws")
	goWS := filepath.Join(goScratchDir(root), goWSName)
	for _, dir := range []string{refWS, goWS} {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			t.Fatalf("workspace %s is missing: %v", dir, err)
		}
	}

	for _, args := range [][]string{
		{"rev-parse", "HEAD^{tree}"},
		{"ls-tree", "-r", "HEAD"},
		{"ls-tree", "HEAD"},
		{"ls-tree", "-r", "HEAD~1"},
		{"ls-files", "-s"},
		{"log", "--format=%s"},
		{"rev-list", "--count", "HEAD"},
	} {
		want := normalizeGitOutput(gitRunCompat(t, refWS, args...))
		got := normalizeGitOutput(gitRunCompat(t, goWS, args...))
		if want != got {
			t.Errorf("git %s differs between the reference and the port:\n reference:\n%s\n port:\n%s",
				strings.Join(args, " "), want, got)
		}
	}

	// The port's repository must also satisfy git's own integrity checks.
	if out := gitRunCompat(t, goWS, "fsck", "--strict"); strings.TrimSpace(out) != "" {
		t.Errorf("git fsck reported problems in the port's repository:\n%s", out)
	}
	if out := gitRunCompat(t, goWS, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("git status is not clean in the port's repository:\n%s", out)
	}
}
