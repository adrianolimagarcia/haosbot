package gitstore

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// The unified-diff writer behind GitStore.diff_commits.
//
// diff_commits calls dulwich's porcelain.diff(commit=..., commit2=...), which
// walks the two trees and writes each change with
// dulwich/patch.py:write_tree_diff → write_object_diff → gen_diff_header plus
// unified_diff_with_algorithm. That output is shown to the user and, through
// show_commit_diff, embedded in the Dream prompt, so it is reproduced here
// rather than replaced by `git diff` (which uses a different diff algorithm and
// different header details).

// firstFewBytes is dulwich/patch.py:FIRST_FEW_BYTES, the window git looks at to
// decide a blob is binary.
const firstFewBytes = 8000

// diffFile is one side of a change: (path, mode, id), each absent when the file
// does not exist on that side.
type diffFile struct {
	path *string
	mode *uint32
	id   *objectID
}

// writeTreeDiff mirrors write_tree_diff (dulwich/patch.py:565).
func (r *repository) writeTreeDiff(oldTree, newTree *objectID) ([]byte, error) {
	changes, err := r.treeChanges(oldTree, newTree)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	for _, c := range changes {
		old := diffFile{path: optionalString(c.oldPath), mode: c.oldMode, id: c.oldID}
		newFile := diffFile{path: optionalString(c.newPath), mode: c.newMode, id: c.newID}
		if err := r.writeObjectDiff(&buf, old, newFile); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// writeObjectDiff mirrors write_object_diff (dulwich/patch.py:384).
func (r *repository) writeObjectDiff(buf *bytes.Buffer, old, newFile diffFile) error {
	oldPath := patchFilename(old.path, "a")
	newPath := patchFilename(newFile.path, "b")

	oldContent, err := r.objectContent(old)
	if err != nil {
		return err
	}
	newContent, err := r.objectContent(newFile)
	if err != nil {
		return err
	}

	writeDiffHeader(buf, old, newFile)
	if isBinaryContent(oldContent) || isBinaryContent(newContent) {
		fmt.Fprintf(buf, "Binary files %s and %s differ\n", oldPath, newPath)
		return nil
	}
	oldLines := blobLines(oldContent)
	newLines := blobLines(newContent)
	writeUnifiedDiff(buf, oldLines, newLines, oldPath, newPath)
	return nil
}

// objectContent mirrors the content() closure in write_object_diff: a missing
// side is the empty blob, a gitlink renders as a subproject line, and anything
// else is the stored blob.
func (r *repository) objectContent(f diffFile) ([]byte, error) {
	if f.id == nil {
		return nil, nil
	}
	if f.mode != nil && *f.mode&0o170000 == modeGitlink {
		return []byte("Subproject commit " + f.id.String() + "\n"), nil
	}
	obj, err := r.store.get(*f.id)
	if err != nil {
		return nil, err
	}
	return obj.data, nil
}

// isBinaryContent mirrors dulwich/patch.py:is_binary.
func isBinaryContent(content []byte) bool {
	window := content
	if len(window) > firstFewBytes {
		window = window[:firstFewBytes]
	}
	return bytes.IndexByte(window, 0) >= 0
}

// blobLines mirrors dulwich's Blob.splitlines (dulwich/objects.py:942): lines
// keep their terminators so the writer can detect a missing final newline.
func blobLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	return splitLinesKeepEnds(string(content), false)
}

// writeDiffHeader mirrors gen_diff_header (dulwich/patch.py:473).
func writeDiffHeader(buf *bytes.Buffer, old, newFile diffFile) {
	oldPath, newPath := old.path, newFile.path
	if oldPath == nil && newPath != nil {
		oldPath = newPath
	}
	if newPath == nil && oldPath != nil {
		newPath = oldPath
	}
	buf.WriteString("diff --git " + patchFilename(oldPath, "a") + " " + patchFilename(newPath, "b") + "\n")

	oldMode, newMode := old.mode, newFile.mode
	if !sameMode(oldMode, newMode) {
		if newMode != nil {
			if oldMode != nil {
				fmt.Fprintf(buf, "old file mode %s\n", strconv.FormatUint(uint64(*oldMode), 8))
			}
			fmt.Fprintf(buf, "new file mode %s\n", strconv.FormatUint(uint64(*newMode), 8))
		} else if oldMode != nil {
			fmt.Fprintf(buf, "deleted file mode %s\n", strconv.FormatUint(uint64(*oldMode), 8))
		}
	}
	buf.WriteString("index " + shortObjectID(old.id) + ".." + shortObjectID(newFile.id))
	if newMode != nil && oldMode != nil {
		buf.WriteString(" " + strconv.FormatUint(uint64(*newMode), 8))
	}
	buf.WriteString("\n")
}

func sameMode(a, b *uint32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// patchFilename mirrors patch_filename (dulwich/patch.py:368).
func patchFilename(p *string, root string) string {
	if p == nil {
		return "/dev/null"
	}
	return root + "/" + *p
}

// shortObjectID mirrors shortid (dulwich/patch.py:353): 7 hex characters, or
// seven zeros for a missing side.
func shortObjectID(id *objectID) string {
	if id == nil {
		return "0000000"
	}
	return id.String()[:7]
}

// writeUnifiedDiff mirrors dulwich.patch.unified_diff (dulwich/patch.py:190):
// the same difflib SequenceMatcher, but over byte lines and with git's
// "\ No newline at end of file" markers.
func writeUnifiedDiff(buf *bytes.Buffer, a, b []string, fromFile, toFile string) {
	const lineTerm = "\n"
	started := false
	for _, group := range newSeqMatcher(nil, a, b).getGroupedOpcodes(3) {
		if !started {
			started = true
			buf.WriteString("--- " + fromFile + lineTerm)
			buf.WriteString("+++ " + toFile + lineTerm)
		}
		first, last := group[0], group[len(group)-1]
		file1Range := formatRangeUnified(first.i1, last.i2)
		file2Range := formatRangeUnified(first.j1, last.j2)
		buf.WriteString("@@ -" + file1Range + " +" + file2Range + " @@" + lineTerm)
		for _, op := range group {
			if op.tag == "equal" {
				for _, line := range a[op.i1:op.i2] {
					buf.WriteString(" " + line)
				}
				continue
			}
			if op.tag == "replace" || op.tag == "delete" {
				for _, line := range a[op.i1:op.i2] {
					buf.WriteString("-" + withNoNewlineMarker(line))
				}
			}
			if op.tag == "replace" || op.tag == "insert" {
				for _, line := range b[op.j1:op.j2] {
					buf.WriteString("+" + withNoNewlineMarker(line))
				}
			}
		}
	}
}

// withNoNewlineMarker appends git's marker to a line that has no terminator.
func withNoNewlineMarker(line string) string {
	if strings.HasSuffix(line, "\n") {
		return line
	}
	return line + "\n\\ No newline at end of file\n"
}
