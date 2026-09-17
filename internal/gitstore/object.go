package gitstore

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// This file implements the slice of the git object model the reference uses
// through dulwich: loose object read/write, tree and commit encoding, and the
// tree/commit decoding needed by log, diff and summarize_working_tree.
//
// The encodings are git's, not dulwich's inventions, so objects written here are
// read by git and by dulwich unchanged (verified in compat/interop tests).

// objectID is a 20-byte SHA-1 object name.
type objectID [20]byte

// zeroID is git's null object name, used for unborn refs and reflog lines
// (dulwich/reflog.py:ZERO_SHA).
var zeroID objectID

func (id objectID) String() string { return hex.EncodeToString(id[:]) }

// short returns the 8-character abbreviation the reference exposes
// (gitstore.py:162 — sha_bytes.decode()[:8]).
func (id objectID) short() string { return id.String()[:8] }

func objectIDFromHex(s string) (objectID, error) {
	var id objectID
	if len(s) != 40 {
		return id, fmt.Errorf("gitstore: object id %q is not 40 hex characters", s)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, fmt.Errorf("gitstore: object id %q: %w", s, err)
	}
	copy(id[:], b)
	return id, nil
}

func objectIDFromBytes(b []byte) (objectID, error) {
	var id objectID
	if len(b) != 20 {
		return id, fmt.Errorf("gitstore: object id must be 20 bytes, got %d", len(b))
	}
	copy(id[:], b)
	return id, nil
}

// objKind is a git object type name.
type objKind string

const (
	kindCommit objKind = "commit"
	kindTree   objKind = "tree"
	kindBlob   objKind = "blob"
	kindTag    objKind = "tag"
)

// rawObject is an object as stored: its type and uncompressed body.
type rawObject struct {
	kind objKind
	data []byte
}

// hashObject returns the object name of a body, i.e. sha1("<type> <len>\0<body>")
// (git's object naming, dulwich/objects.py:ShaFile.id).
func hashObject(kind objKind, data []byte) objectID {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d", kind, len(data))
	h.Write([]byte{0})
	h.Write(data)
	var id objectID
	copy(id[:], h.Sum(nil))
	return id
}

// ---------------------------------------------------------------------------
// Loose object store
// ---------------------------------------------------------------------------

// objectStore reads and writes objects in a single repository's object
// database: loose files under .git/objects/xx/yyyy plus, for read-only access,
// pack files (see pack.go).
type objectStore struct {
	dir     string // <workspace>/.git/objects
	packs   []*packFile
	packsOK bool
}

func newObjectStore(gitDir string) *objectStore {
	return &objectStore{dir: filepath.Join(gitDir, "objects")}
}

// hasLoose reports whether the loose object file for id exists.
func (s *objectStore) hasLoose(id objectID) bool {
	_, err := os.Stat(s.loosePath(id))
	return err == nil
}

func (s *objectStore) loosePath(id objectID) string {
	hexID := id.String()
	return filepath.Join(s.dir, hexID[:2], hexID[2:])
}

// get returns the object with the given name, looking in loose files first and
// then in pack files — the same lookup order dulwich's DiskObjectStore uses.
func (s *objectStore) get(id objectID) (rawObject, error) {
	if data, err := os.ReadFile(s.loosePath(id)); err == nil {
		return decodeLoose(id, data)
	} else if !errors.Is(err, os.ErrNotExist) {
		return rawObject{}, err
	}
	return s.getPacked(id)
}

// getLooseOnly reads a loose object without consulting packs.
func (s *objectStore) getLooseOnly(id objectID) (rawObject, error) {
	data, err := os.ReadFile(s.loosePath(id))
	if err != nil {
		return rawObject{}, err
	}
	return decodeLoose(id, data)
}

// decodeLoose parses "<type> <size>\0<body>" from a zlib stream.
func decodeLoose(id objectID, compressed []byte) (rawObject, error) {
	zr, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return rawObject{}, fmt.Errorf("gitstore: object %s: %w", id, err)
	}
	defer zr.Close()
	body, err := io.ReadAll(zr)
	if err != nil {
		return rawObject{}, fmt.Errorf("gitstore: object %s: %w", id, err)
	}
	nul := bytes.IndexByte(body, 0)
	if nul < 0 {
		return rawObject{}, fmt.Errorf("gitstore: object %s has no header terminator", id)
	}
	header := string(body[:nul])
	sp := strings.IndexByte(header, ' ')
	if sp < 0 {
		return rawObject{}, fmt.Errorf("gitstore: object %s has a malformed header", id)
	}
	kind := objKind(header[:sp])
	size, err := strconv.Atoi(header[sp+1:])
	if err != nil {
		return rawObject{}, fmt.Errorf("gitstore: object %s has a malformed size", id)
	}
	data := body[nul+1:]
	if len(data) != size {
		return rawObject{}, fmt.Errorf("gitstore: object %s declares %d bytes, has %d", id, size, len(data))
	}
	return rawObject{kind: kind, data: data}, nil
}

// put writes an object if it is not already present and returns its name.
func (s *objectStore) put(kind objKind, data []byte) (objectID, error) {
	id := hashObject(kind, data)
	if s.hasLoose(id) {
		return id, nil
	}
	hexID := id.String()
	dir := filepath.Join(s.dir, hexID[:2])
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return id, err
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := fmt.Fprintf(zw, "%s %d", kind, len(data)); err != nil {
		return id, err
	}
	if _, err := zw.Write([]byte{0}); err != nil {
		return id, err
	}
	if _, err := zw.Write(data); err != nil {
		return id, err
	}
	if err := zw.Close(); err != nil {
		return id, err
	}
	// Write to a unique temp file in the destination directory and rename, so a
	// reader never observes a partially written object (dulwich's GitFile does
	// the same).
	tmp, err := os.CreateTemp(dir, "tmp_obj_*")
	if err != nil {
		return id, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return id, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return id, err
	}
	if err := os.Chmod(tmpName, 0o444); err != nil {
		os.Remove(tmpName)
		return id, err
	}
	if err := os.Rename(tmpName, s.loosePath(id)); err != nil {
		os.Remove(tmpName)
		return id, err
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// Trees
// ---------------------------------------------------------------------------

// treeEntry is one entry of a tree object. mode is stored the way git prints it
// ("100644", "40000", "120000", "160000"); git omits the leading zero of the
// directory mode.
type treeEntry struct {
	name string
	mode string
	id   objectID
}

// parseTree decodes a tree object body.
//
// Entries are stored in git's tree order, which is byte order over the name with
// a trailing "/" appended for subtrees (dulwich/objects.py:Tree._deserialize).
func parseTree(data []byte) ([]treeEntry, error) {
	var entries []treeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			return nil, errors.New("gitstore: malformed tree entry")
		}
		mode := string(data[:sp])
		rest := data[sp+1:]
		nul := bytes.IndexByte(rest, 0)
		if nul < 0 {
			return nil, errors.New("gitstore: malformed tree entry name")
		}
		name := string(rest[:nul])
		rest = rest[nul+1:]
		if len(rest) < 20 {
			return nil, errors.New("gitstore: truncated tree entry")
		}
		id, _ := objectIDFromBytes(rest[:20])
		entries = append(entries, treeEntry{name: name, mode: mode, id: id})
		data = rest[20:]
	}
	return entries, nil
}

// treeEntryKey returns the sort key git uses inside a tree: the name, plus "/"
// for subtrees.
func treeEntryKey(e treeEntry) string {
	if e.mode == "40000" || e.mode == "040000" {
		return e.name + "/"
	}
	return e.name
}

func encodeTree(entries []treeEntry) []byte {
	sorted := make([]treeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return treeEntryKey(sorted[i]) < treeEntryKey(sorted[j])
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.mode)
		buf.WriteByte(' ')
		buf.WriteString(e.name)
		buf.WriteByte(0)
		buf.Write(e.id[:])
	}
	return buf.Bytes()
}

// commitObj is the subset of a commit the reference reads: its tree, its
// parents, the committer timestamp (used for the log display time,
// gitstore.py:259-262) and its message.
type commitObj struct {
	tree       objectID
	parents    []objectID
	commitTime int64
	message    []byte
	raw        []byte
}

// commitIdent is the "Name <email>" part of an author/committer line.
type commitIdent struct {
	name  string
	email string
}

// parseCommit decodes a commit object body.
//
// Header parsing follows dulwich's _parse_message: "key value" lines, with
// continuation lines beginning with a space, terminated by a blank line; the
// remainder is the message.
func parseCommit(data []byte) (commitObj, error) {
	c := commitObj{raw: data}
	rest := data
	for len(rest) > 0 {
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			return c, errors.New("gitstore: commit header is not terminated")
		}
		line := rest[:nl]
		rest = rest[nl+1:]
		if len(line) == 0 {
			break
		}
		if line[0] == ' ' {
			// Continuation of the previous header (gpgsig and friends); the
			// reference never reads those, so skipping is enough.
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		key := string(line[:sp])
		value := string(line[sp+1:])
		switch key {
		case "tree":
			id, err := objectIDFromHex(value)
			if err != nil {
				return c, err
			}
			c.tree = id
		case "parent":
			id, err := objectIDFromHex(value)
			if err != nil {
				return c, err
			}
			c.parents = append(c.parents, id)
		case "committer":
			if ts, ok := parseIdentTime(value); ok {
				c.commitTime = ts
			}
		}
	}
	c.message = rest
	return c, nil
}

// parseIdentTime extracts the unix timestamp from "Name <email> 1700000000 +0000".
func parseIdentTime(value string) (int64, bool) {
	fields := strings.Fields(value)
	if len(fields) < 2 {
		return 0, false
	}
	ts, err := strconv.ParseInt(fields[len(fields)-2], 10, 64)
	if err != nil {
		return 0, false
	}
	return ts, true
}

// encodeCommit builds a commit body in git's field order:
// tree, parents, author, committer, [encoding], blank line, message
// (dulwich/objects.py:Commit._serialize).
func encodeCommit(
	tree objectID,
	parents []objectID,
	author, committer commitIdent,
	authorTime, commitTime int64,
	authorTZ, commitTZ int,
	message []byte,
) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&buf, "parent %s\n", p)
	}
	fmt.Fprintf(&buf, "author %s <%s> %d %s\n", author.name, author.email, authorTime, formatTimezone(authorTZ, false))
	fmt.Fprintf(&buf, "committer %s <%s> %d %s\n", committer.name, committer.email, commitTime, formatTimezone(commitTZ, false))
	buf.WriteByte('\n')
	buf.Write(message)
	return buf.Bytes()
}

// formatTimezone renders a UTC offset in seconds as git's ±HHMM
// (dulwich/objects.py:format_timezone).
func formatTimezone(offset int, negUTC bool) string {
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	} else if offset == 0 && negUTC {
		sign = "-"
	}
	return fmt.Sprintf("%s%02d%02d", sign, offset/3600, (offset%3600)/60)
}
