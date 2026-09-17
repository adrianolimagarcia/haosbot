package gitstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Repository plumbing: the .git layout dulwich creates, ref and HEAD handling,
// and the reflog. These are the pieces of dulwich that GitStore relies on
// implicitly (porcelain.init, refs.set_if_equals/add_if_new, Repo._write_reflog).

// repository is an opened git repository rooted at a workspace directory.
type repository struct {
	worktree string
	gitDir   string
	store    *objectStore
}

// openRepository points a repository at worktree/.git.
func openRepository(worktree string) *repository {
	gitDir := filepath.Join(worktree, ".git")
	return &repository{worktree: worktree, gitDir: gitDir, store: newObjectStore(gitDir)}
}

// isInitialized mirrors GitStore.is_initialized (gitstore.py:54-56): the check is
// a directory test, so a .git *file* (a linked worktree or a submodule) reads as
// "not initialized".
func (r *repository) isInitialized() bool {
	fi, err := os.Stat(r.gitDir)
	return err == nil && fi.IsDir()
}

// initRepository creates the repository layout dulwich's porcelain.init creates
// (dulwich/repo.py:Repo.init → _init_maybe_bare → _init_files).
//
// The files written here are compared byte for byte by the interop tests: HEAD,
// config, description and info/exclude all have fixed content in the reference.
func initRepository(worktree string) (*repository, error) {
	r := openRepository(worktree)
	if err := os.Mkdir(r.gitDir, 0o777); err != nil {
		return nil, err
	}
	// BASE_DIRECTORIES (dulwich/repo.py:166).
	for _, dir := range []string{"branches", "refs", "refs/tags", "refs/heads", "hooks", "info"} {
		if err := os.MkdirAll(filepath.Join(r.gitDir, filepath.FromSlash(dir)), 0o777); err != nil {
			return nil, err
		}
	}
	// DiskObjectStore.init creates objects/, objects/info and objects/pack.
	for _, dir := range []string{"objects", "objects/info", "objects/pack"} {
		if err := os.MkdirAll(filepath.Join(r.gitDir, filepath.FromSlash(dir)), 0o777); err != nil {
			return nil, err
		}
	}

	branch := defaultBranchName()
	if err := r.setSymbolicRef("HEAD", "refs/heads/"+branch); err != nil {
		return nil, err
	}
	config := "[core]\n" +
		"\trepositoryformatversion = 0\n" +
		"\tfilemode = true\n" +
		"\tbare = false\n" +
		"\tlogallrefupdates = true\n"
	if err := os.WriteFile(filepath.Join(r.gitDir, "config"), []byte(config), 0o666); err != nil {
		return nil, err
	}
	// _init_files writes these two as well (dulwich/repo.py:553, :606).
	if err := os.WriteFile(filepath.Join(r.gitDir, "description"), []byte("Unnamed repository"), 0o666); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(r.gitDir, "info", "exclude"), nil, 0o666); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *repository) setSymbolicRef(name, target string) error {
	path := filepath.Join(r.gitDir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("ref: "+target+"\n"), 0o666)
}

// readRef returns the contents of a loose ref file, or nil when absent.
func (r *repository) readRef(name string) []byte {
	data, err := os.ReadFile(filepath.Join(r.gitDir, filepath.FromSlash(name)))
	if err != nil {
		return nil
	}
	return []byte(strings.TrimRight(string(data), "\n"))
}

// packedRefs reads .git/packed-refs. The reference consults packed refs after
// loose ones (dulwich/refs.py:DiskRefsContainer.read_ref), which matters for a
// repository that has been `git pack-refs`'d or `git gc`'d.
func (r *repository) packedRefs() map[string]string {
	data, err := os.ReadFile(filepath.Join(r.gitDir, "packed-refs"))
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp != 40 {
			continue
		}
		out[line[41:]] = line[:40]
	}
	return out
}

// readRefResolved returns a ref's value, looking at loose refs first and then
// packed refs.
func (r *repository) readRefResolved(name string) []byte {
	if v := r.readRef(name); v != nil {
		return v
	}
	if v, ok := r.packedRefs()[name]; ok {
		return []byte(v)
	}
	return nil
}

// follow mirrors DiskRefsContainer.follow (dulwich/refs.py:413): it walks the
// symbolic-ref chain and returns the names it visited plus the resolved id, or
// nil for an unborn ref. HEAD itself is the first name in the chain.
func (r *repository) follow(name string) ([]string, *objectID) {
	contents := "ref: " + name
	var refnames []string
	for depth := 0; strings.HasPrefix(contents, "ref: "); depth++ {
		if depth > 5 {
			// dulwich raises SymrefLoop here; treat it as unresolvable.
			return refnames, nil
		}
		refname := contents[len("ref: "):]
		refnames = append(refnames, refname)
		value := r.readRefResolved(refname)
		if len(value) == 0 {
			return refnames, nil
		}
		contents = string(value)
	}
	id, err := objectIDFromHex(contents)
	if err != nil {
		return refnames, nil
	}
	return refnames, &id
}

// headID resolves HEAD to a commit id. ok is false for an unborn HEAD, which is
// the state dulwich reports by raising KeyError (gitstore.py:181-183, :249-251).
func (r *repository) headID() (objectID, bool, error) {
	value := r.readRef("HEAD")
	if len(value) == 0 {
		return objectID{}, false, nil
	}
	if !strings.HasPrefix(string(value), "ref: ") {
		id, err := objectIDFromHex(string(value))
		if err != nil {
			return objectID{}, false, fmt.Errorf("gitstore: HEAD does not name a commit: %w", err)
		}
		return id, true, nil
	}
	names, id := r.follow("HEAD")
	if id == nil {
		return objectID{}, false, nil
	}
	_ = names
	return *id, true, nil
}

// headTree returns the tree of the HEAD commit, or ok=false when there are no
// commits (gitstore.py:404-415).
func (r *repository) headTree() (objectID, bool, error) {
	id, ok, err := r.headID()
	if err != nil || !ok {
		return objectID{}, false, err
	}
	obj, err := r.store.get(id)
	if err != nil {
		return objectID{}, false, err
	}
	if obj.kind != kindCommit {
		return objectID{}, false, nil
	}
	commit, err := parseCommit(obj.data)
	if err != nil {
		return objectID{}, false, err
	}
	return commit.tree, true, nil
}

// updateHead writes a new commit to HEAD, reproducing what
// WorkTree.commit does with dulwich's refs API (dulwich/worktree.py:648-676):
//
//   - when HEAD already resolves to a commit, set_if_equals is used and the
//     reflog entry is written for the *resolved* ref (logs/refs/heads/<branch>);
//   - when HEAD is unborn, add_if_new is used and the reflog entry is written
//     for the name that was passed in (logs/HEAD).
//
// That asymmetry is visible in the repository — `git reflog` reads both files —
// and it is what the frozen reference produces, so it is reproduced here.
func (r *repository) updateHead(newID objectID, message string, committer string, ts int64, tz int) error {
	oldID, hasOld, err := r.headID()
	if err != nil {
		return err
	}
	names, resolved := r.follow("HEAD")
	realname := "HEAD"
	if len(names) > 0 {
		realname = names[len(names)-1]
	}

	refPath := filepath.Join(r.gitDir, filepath.FromSlash(realname))
	if !hasOld {
		// add_if_new: refuse when the ref already exists on disk.
		if _, err := os.Stat(refPath); err == nil {
			return errors.New("gitstore: HEAD changed during commit")
		}
		if _, ok := r.packedRefs()[realname]; ok {
			return errors.New("gitstore: HEAD changed during commit")
		}
		if err := writeRefFile(refPath, newID); err != nil {
			return err
		}
		return r.appendReflog("HEAD", zeroID, newID, committer, ts, tz, message)
	}

	// set_if_equals: the compare-and-swap the reference performs.
	current := r.readRef(realname)
	if current == nil {
		if v, ok := r.packedRefs()[realname]; ok {
			current = []byte(v)
		} else {
			current = []byte(zeroID.String())
		}
	}
	if string(current) != oldID.String() {
		return errors.New("gitstore: HEAD changed during commit")
	}
	if string(current) == newID.String() {
		return nil
	}
	if err := writeRefFile(refPath, newID); err != nil {
		return err
	}
	_ = resolved
	return r.appendReflog(realname, oldID, newID, committer, ts, tz, message)
}

func writeRefFile(path string, id objectID) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	// Write through a lock file and rename, the way dulwich's GitFile does, so a
	// reader never sees a half-written ref.
	tmp, err := os.CreateTemp(filepath.Dir(path), "tmp_ref_*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(id.String() + "\n"); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// appendReflog appends one reflog line to .git/logs/<ref>
// (dulwich/repo.py:_write_reflog, dulwich/reflog.py:format_reflog_line).
func (r *repository) appendReflog(ref string, oldID, newID objectID, committer string, ts int64, tz int, message string) error {
	path := filepath.Join(r.gitDir, "logs", filepath.FromSlash(ref))
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	line := fmt.Sprintf("%s %s %s %d %s\t%s\n", oldID, newID, committer, ts, formatTimezone(tz, false), message)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}
