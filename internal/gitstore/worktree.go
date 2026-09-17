package gitstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Working-tree operations: staging paths into the index, deciding whether the
// index differs from HEAD, and creating a commit.
//
// These are the dulwich porcelain calls GitStore makes, transcribed:
// porcelain.add + porcelain.status (gitstore.py:139-142) and porcelain.commit
// (gitstore.py:151-156). They are deliberately spelled out rather than routed
// through a generic "git add"/"git commit" because the reference's staging
// behaviour has observable details: a tracked file that no longer exists stages
// a *deletion* (dulwich/worktree.py:WorkTree.stage), and a path matched by an
// ignore rule is skipped entirely.

// stagePaths mirrors porcelain.add(paths=...): resolve each path relative to the
// repository, drop ignored paths, then stage what remains.
//
// Returns the relative paths that were staged and those that were ignored, which
// is what porcelain.add returns (dulwich/porcelain/__init__.py:1501).
func (r *repository) stagePaths(absPaths []string, ignores *ignoreManager) (staged []string, ignored []string, err error) {
	repoPath := resolveNonStrict(r.worktree)
	for _, p := range absPaths {
		path := p
		if !filepath.IsAbs(path) {
			path = filepath.Join(repoPath, path)
		}
		// Symlinked paths resolve only their parent directory, so a tracked
		// symlink pointing outside the repository still stages as itself
		// (dulwich/porcelain/__init__.py:1570-1577).
		var resolved string
		if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			resolved = filepath.Join(resolveNonStrict(filepath.Dir(path)), filepath.Base(path))
		} else {
			resolved = resolveNonStrict(path)
		}
		rel, rerr := filepath.Rel(repoPath, resolved)
		if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return nil, nil, fmt.Errorf("gitstore: path %s is not within repository %s", path, repoPath)
		}
		rel = filepath.ToSlash(rel)

		if fi, lerr := os.Stat(resolved); lerr == nil && fi.IsDir() {
			// A directory stages the untracked and modified files below it.
			files, ferr := r.filesUnder(resolved, repoPath, rel)
			if ferr != nil {
				return nil, nil, ferr
			}
			for _, f := range files {
				if ignores != nil && truthy(ignores.isIgnored(f)) {
					ignored = append(ignored, f)
					continue
				}
				staged = append(staged, f)
			}
			continue
		}
		if ignores != nil && truthy(ignores.isIgnored(rel)) {
			ignored = append(ignored, rel)
			continue
		}
		staged = append(staged, rel)
	}
	if len(staged) == 0 {
		return staged, ignored, nil
	}
	if err := r.stage(staged); err != nil {
		return nil, nil, err
	}
	return staged, ignored, nil
}

// filesUnder lists the files below a directory that are not already tracked, plus
// the tracked files below it that differ from the index. It stands in for
// get_untracked_paths + get_unstaged_changes, which porcelain.add uses when it is
// handed a directory.
func (r *repository) filesUnder(dir, repoPath, rel string) ([]string, error) {
	idx, err := readIndex(filepath.Join(r.gitDir, "index"))
	if err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	err = filepath.Walk(dir, func(p string, fi os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if fi.IsDir() {
			return nil
		}
		rp, rerr := filepath.Rel(repoPath, p)
		if rerr != nil {
			return nil
		}
		rp = filepath.ToSlash(rp)
		if !seen[rp] {
			seen[rp] = true
			out = append(out, rp)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for name := range idx.entries {
		if strings.HasPrefix(name, rel+"/") && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// stage mirrors WorkTree.stage (dulwich/worktree.py:299): update the index for
// each path, using the working tree's current state.
func (r *repository) stage(relPaths []string) error {
	indexPath := filepath.Join(r.gitDir, "index")
	idx, err := readIndex(indexPath)
	if err != nil {
		return err
	}
	for _, rel := range relPaths {
		full := filepath.Join(r.worktree, filepath.FromSlash(rel))
		fi, lerr := os.Lstat(full)
		if lerr != nil {
			if errors.Is(lerr, os.ErrNotExist) || isNotDir(lerr) {
				// The file is gone: staging it records the deletion.
				delete(idx.entries, rel)
				continue
			}
			return lerr
		}
		switch {
		case fi.IsDir():
			// A directory is only an index entry when it is a submodule
			// (gitlink). Submodule entries are not reproduced by this port;
			// the entry is dropped, which is what dulwich does for every
			// directory that is not a repository.
			delete(idx.entries, rel)
		case !fi.Mode().IsRegular() && fi.Mode()&os.ModeSymlink == 0:
			delete(idx.entries, rel)
		default:
			content, mode, cerr := readWorktreeFile(full, fi)
			if cerr != nil {
				return cerr
			}
			id, perr := r.store.put(kindBlob, content)
			if perr != nil {
				return perr
			}
			idx.entries[rel] = indexEntryFromStat(fi, mode, id, rel)
		}
	}
	return idx.write(indexPath)
}

// readWorktreeFile returns the bytes git would store for a path: the file's
// content, or a symlink's target, with git's canonical mode.
func readWorktreeFile(path string, fi os.FileInfo) ([]byte, uint32, error) {
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return nil, 0, err
		}
		return []byte(target), modeSymlink, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	mode := uint32(modeRegular)
	if fi.Mode().Perm()&0o100 != 0 {
		mode = modeExecutable
	}
	return data, mode, nil
}

// hasStagedChanges mirrors porcelain.status(...).staged being non-empty
// (gitstore.py:140-143): the index is compared against the tree at HEAD, so a
// staged change is anything that would alter the next commit's tree.
func (r *repository) hasStagedChanges(idx *gitIndex) (bool, error) {
	treeID, err := r.store.writeTreeFromIndex(idx)
	if err != nil {
		return false, err
	}
	headTree, ok, err := r.headTree()
	if err != nil {
		return false, err
	}
	if !ok {
		// Unborn HEAD: everything in the index is an addition.
		return len(idx.entries) > 0, nil
	}
	return treeID != headTree, nil
}

// commit mirrors porcelain.commit (gitstore.py:151) with the author and
// committer the reference always uses.
func (r *repository) commit(message []byte, ident commitIdent, ts int64, tz int) (objectID, error) {
	indexPath := filepath.Join(r.gitDir, "index")
	idx, err := readIndex(indexPath)
	if err != nil {
		return objectID{}, err
	}
	treeID, err := r.store.writeTreeFromIndex(idx)
	if err != nil {
		return objectID{}, err
	}
	var parents []objectID
	if head, ok, err := r.headID(); err != nil {
		return objectID{}, err
	} else if ok {
		parents = append(parents, head)
	}
	body := encodeCommit(treeID, parents, ident, ident, ts, ts, tz, tz, message)
	id, err := r.store.put(kindCommit, body)
	if err != nil {
		return objectID{}, err
	}
	if err := r.updateHead(id, "commit: "+string(message), ident.String(), ts, tz); err != nil {
		return objectID{}, err
	}
	return id, nil
}

// String renders an identity the way git writes it in a reflog line.
func (i commitIdent) String() string { return i.name + " <" + i.email + ">" }

// treeChange is one entry of a tree diff: dulwich's TreeChange.
type treeChange struct {
	oldPath, newPath string
	oldMode, newMode *uint32
	oldID, newID     *objectID
}

// treeChanges mirrors ObjectStore.tree_changes (dulwich/object_store.py:428) over
// diff_tree.tree_changes + walk_trees, which is what write_tree_diff iterates.
func (r *repository) treeChanges(oldTree, newTree *objectID) ([]treeChange, error) {
	var out []treeChange
	if err := r.walkChanges(oldTree, newTree, "", &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *repository) walkChanges(t1, t2 *objectID, prefix string, out *[]treeChange) error {
	var entries1, entries2 []treeEntry
	var err error
	if t1 != nil {
		if entries1, err = r.treeEntries(*t1); err != nil {
			return err
		}
	}
	if t2 != nil {
		if entries2, err = r.treeEntries(*t2); err != nil {
			return err
		}
	}
	// Two-pointer merge of the two entry lists, exactly like dulwich's
	// _merge_entries (dulwich/diff_tree.py): each list is already in git's tree
	// order, and the merge itself compares the full paths as plain byte
	// strings. Both halves of that matter — the comparison is not the tree sort
	// key, so a tree named "a" and a file named "a-b" merge in dulwich's order,
	// not in git's.
	i1, i2 := 0, 0
	type pair struct{ e1, e2 *treeEntry }
	var pairs []pair
	for i1 < len(entries1) && i2 < len(entries2) {
		p1 := prefix + entries1[i1].name
		p2 := prefix + entries2[i2].name
		switch {
		case p1 < p2:
			e := entries1[i1]
			pairs = append(pairs, pair{e1: &e})
			i1++
		case p1 > p2:
			e := entries2[i2]
			pairs = append(pairs, pair{e2: &e})
			i2++
		default:
			e1, e2 := entries1[i1], entries2[i2]
			pairs = append(pairs, pair{e1: &e1, e2: &e2})
			i1++
			i2++
		}
	}
	for ; i1 < len(entries1); i1++ {
		e := entries1[i1]
		pairs = append(pairs, pair{e1: &e})
	}
	for ; i2 < len(entries2); i2++ {
		e := entries2[i2]
		pairs = append(pairs, pair{e2: &e})
	}

	for _, pr := range pairs {
		e1, e2 := pr.e1, pr.e2
		isTree1 := e1 != nil && isTreeMode(e1.mode)
		isTree2 := e2 != nil && isTreeMode(e2.mode)
		if isTree1 && isTree2 && e1.id == e2.id {
			continue // prune_identical
		}
		// Trees are not reported themselves (include_trees=False).
		var f1, f2 *treeEntry
		if e1 != nil && !isTree1 {
			f1 = e1
		}
		if e2 != nil && !isTree2 {
			f2 = e2
		}
		if f1 != nil || f2 != nil {
			name := ""
			if f1 != nil {
				name = f1.name
			} else {
				name = f2.name
			}
			*out = append(*out, r.changeFor(f1, f2, prefix+name)...)
		}
		if isTree1 || isTree2 {
			var c1, c2 *objectID
			if isTree1 {
				id := e1.id
				c1 = &id
			}
			if isTree2 {
				id := e2.id
				c2 = &id
			}
			name := ""
			if e1 != nil {
				name = e1.name
			} else {
				name = e2.name
			}
			if err := r.walkChanges(c1, c2, prefix+name+"/", out); err != nil {
				return err
			}
		}
	}
	return nil
}

// changeFor turns a pair of entries into the TreeChange list tree_changes emits.
func (r *repository) changeFor(e1, e2 *treeEntry, path string) []treeChange {
	modeOf := func(e *treeEntry) *uint32 {
		if e == nil {
			return nil
		}
		m := parseOctalMode(e.mode)
		return &m
	}
	idOf := func(e *treeEntry) *objectID {
		if e == nil {
			return nil
		}
		id := e.id
		return &id
	}
	if e1 != nil && e2 != nil {
		if fileType(e1.mode) != fileType(e2.mode) {
			// A change of file type is reported as a delete followed by an add.
			return []treeChange{
				{oldPath: path, oldMode: modeOf(e1), oldID: idOf(e1)},
				{newPath: path, newMode: modeOf(e2), newID: idOf(e2)},
			}
		}
		if e1.id == e2.id && e1.mode == e2.mode {
			return nil
		}
		return []treeChange{{
			oldPath: path, newPath: path,
			oldMode: modeOf(e1), newMode: modeOf(e2),
			oldID: idOf(e1), newID: idOf(e2),
		}}
	}
	if e1 != nil {
		return []treeChange{{oldPath: path, oldMode: modeOf(e1), oldID: idOf(e1)}}
	}
	return []treeChange{{newPath: path, newMode: modeOf(e2), newID: idOf(e2)}}
}

func (r *repository) treeEntries(id objectID) ([]treeEntry, error) {
	obj, err := r.store.get(id)
	if err != nil {
		return nil, err
	}
	if obj.kind != kindTree {
		return nil, fmt.Errorf("gitstore: object %s is a %s, not a tree", id, obj.kind)
	}
	return parseTree(obj.data)
}

func isTreeMode(mode string) bool {
	m := parseOctalMode(mode)
	return m == modeTree || m == 0o40000
}

// fileType returns the S_IFMT part of a tree mode, which is what decides whether
// a change is a modification or a delete+add pair.
func fileType(mode string) uint32 { return parseOctalMode(mode) & 0o170000 }

func parseOctalMode(mode string) uint32 {
	var v uint32
	for i := 0; i < len(mode); i++ {
		if mode[i] < '0' || mode[i] > '7' {
			return v
		}
		v = v*8 + uint32(mode[i]-'0')
	}
	return v
}

func truthy(b *bool) bool { return b != nil && *b }

func isNotDir(err error) bool {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return strings.Contains(pe.Err.Error(), "not a directory")
	}
	return false
}
