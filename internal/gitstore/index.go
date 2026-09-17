package gitstore

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// This file implements git's index file (version 2) as dulwich reads and writes
// it: dulwich/index.py:DEFAULT_VERSION = 2, write_index / write_cache_entry.
//
// The index is not an implementation detail here. It is the state the reference
// reads back with porcelain.status (gitstore.py:140) to decide whether
// auto_commit has anything to commit, and it is shared with the real git CLI
// and with dulwich, so a Go-written index must be readable by both (compat/
// interop tests cover this).

const (
	indexSignature = "DIRC"
	indexVersion   = 2

	// Git's mode values as they appear in trees and index entries.
	modeRegular    = 0o100644
	modeExecutable = 0o100755
	modeSymlink    = 0o120000
	modeTree       = 0o040000
	modeGitlink    = 0o160000
)

// indexEntry is one cache entry (dulwich/index.py:IndexEntry).
type indexEntry struct {
	ctimeSec, ctimeNsec uint32
	mtimeSec, mtimeNsec uint32
	dev, ino            uint32
	mode                uint32
	uid, gid, size      uint32
	id                  objectID
	flags               uint16
	name                string // tree path, "/"-separated
}

// gitIndex is the in-memory form of .git/index, keyed by tree path.
type gitIndex struct {
	entries map[string]*indexEntry
}

func newGitIndex() *gitIndex {
	return &gitIndex{entries: map[string]*indexEntry{}}
}

// readIndex loads .git/index. A missing file yields an empty index, exactly as
// dulwich's Index.read does (it returns early when the file does not exist).
func readIndex(path string) (*gitIndex, error) {
	idx := newGitIndex()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return idx, nil
		}
		return nil, err
	}
	if len(data) < 12 {
		return nil, errors.New("gitstore: index file is truncated")
	}
	if string(data[:4]) != indexSignature {
		return nil, errors.New("gitstore: index file has a bad signature")
	}
	version := binary.BigEndian.Uint32(data[4:8])
	if version < 2 || version > 4 {
		return nil, fmt.Errorf("gitstore: unsupported index version %d", version)
	}
	count := int(binary.BigEndian.Uint32(data[8:12]))
	pos := 12
	for i := 0; i < count; i++ {
		if pos+62 > len(data) {
			return nil, errors.New("gitstore: index file is truncated")
		}
		e := &indexEntry{}
		e.ctimeSec = binary.BigEndian.Uint32(data[pos:])
		e.ctimeNsec = binary.BigEndian.Uint32(data[pos+4:])
		e.mtimeSec = binary.BigEndian.Uint32(data[pos+8:])
		e.mtimeNsec = binary.BigEndian.Uint32(data[pos+12:])
		e.dev = binary.BigEndian.Uint32(data[pos+16:])
		e.ino = binary.BigEndian.Uint32(data[pos+20:])
		e.mode = binary.BigEndian.Uint32(data[pos+24:])
		e.uid = binary.BigEndian.Uint32(data[pos+28:])
		e.gid = binary.BigEndian.Uint32(data[pos+32:])
		e.size = binary.BigEndian.Uint32(data[pos+36:])
		copy(e.id[:], data[pos+40:pos+60])
		e.flags = binary.BigEndian.Uint16(data[pos+60:])
		pos += 62
		nameLen := int(e.flags & 0x0fff)
		entryStart := pos - 62
		if version == 4 {
			// Version 4 prefix-compresses entry names with a varint-encoded
			// suffix length. Git only writes it when index.version=4 (or
			// feature.manyFiles); the reference never does. Rather than
			// mis-parse it, refuse it loudly — callers surface this as a
			// GitStoreError, so a repository in that state fails visibly
			// instead of silently committing the wrong tree.
			return nil, errors.New("gitstore: index version 4 is not supported by this port")
		}
		if e.flags&0x4000 != 0 { // extended flags: one extra 16-bit field
			pos += 2
		}
		if nameLen == 0x0fff {
			nul := bytes.IndexByte(data[pos:], 0)
			if nul < 0 {
				return nil, errors.New("gitstore: index entry name is truncated")
			}
			nameLen = nul
		}
		if pos+nameLen > len(data) {
			return nil, errors.New("gitstore: index entry name is truncated")
		}
		e.name = string(data[pos : pos+nameLen])
		// Entries are NUL-padded so that the whole entry is a multiple of 8
		// bytes (dulwich/index.py:write_cache_entry).
		total := (pos - entryStart + nameLen + 8) &^ 7
		pos = entryStart + total
		idx.entries[e.name] = e
	}
	return idx, nil
}

// write serializes the index in version 2, including the trailing SHA-1 over
// everything before it.
func (idx *gitIndex) write(path string) error {
	names := make([]string, 0, len(idx.entries))
	for name := range idx.entries {
		names = append(names, name)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	buf.WriteString(indexSignature)
	var header [8]byte
	binary.BigEndian.PutUint32(header[0:], indexVersion)
	binary.BigEndian.PutUint32(header[4:], uint32(len(names)))
	buf.Write(header[:])
	for _, name := range names {
		writeIndexEntry(&buf, idx.entries[name])
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])

	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func writeIndexEntry(buf *bytes.Buffer, e *indexEntry) {
	start := buf.Len()
	var fixed [62]byte
	binary.BigEndian.PutUint32(fixed[0:], e.ctimeSec)
	binary.BigEndian.PutUint32(fixed[4:], e.ctimeNsec)
	binary.BigEndian.PutUint32(fixed[8:], e.mtimeSec)
	binary.BigEndian.PutUint32(fixed[12:], e.mtimeNsec)
	binary.BigEndian.PutUint32(fixed[16:], e.dev)
	binary.BigEndian.PutUint32(fixed[20:], e.ino)
	binary.BigEndian.PutUint32(fixed[24:], e.mode)
	binary.BigEndian.PutUint32(fixed[28:], e.uid)
	binary.BigEndian.PutUint32(fixed[32:], e.gid)
	binary.BigEndian.PutUint32(fixed[36:], e.size)
	copy(fixed[40:60], e.id[:])
	nameLen := len(e.name)
	if nameLen > 0x0fff {
		nameLen = 0x0fff
	}
	flags := uint16(nameLen) | (e.flags &^ 0x0fff)
	binary.BigEndian.PutUint16(fixed[60:], flags)
	buf.Write(fixed[:])
	buf.WriteString(e.name)
	// Pad with NULs so the entry occupies a multiple of 8 bytes, including at
	// least one NUL terminator.
	realSize := (buf.Len() - start + 8) &^ 7
	for buf.Len()-start < realSize {
		buf.WriteByte(0)
	}
}

// indexEntryFromStat builds an index entry from an lstat result, mirroring
// dulwich/index.py:index_entry_from_stat.
func indexEntryFromStat(fi os.FileInfo, mode uint32, id objectID, name string) *indexEntry {
	st := statOf(fi)
	return &indexEntry{
		ctimeSec:  st.ctimeSec,
		ctimeNsec: st.ctimeNsec,
		mtimeSec:  st.mtimeSec,
		mtimeNsec: st.mtimeNsec,
		dev:       st.dev,
		ino:       st.ino,
		mode:      mode,
		uid:       st.uid,
		gid:       st.gid,
		size:      st.size,
		id:        id,
		name:      name,
	}
}

// writeTreeFromIndex writes the tree objects for the whole index and returns the
// root tree id. It is the equivalent of dulwich's Index.commit(object_store)
// (dulwich/index.py:Index.commit), which both porcelain.status and the commit
// path use to turn the index into a tree.
func (s *objectStore) writeTreeFromIndex(idx *gitIndex) (objectID, error) {
	type node struct {
		entries map[string]*indexEntry // file entries in this directory
		dirs    map[string]*node
	}
	root := &node{entries: map[string]*indexEntry{}, dirs: map[string]*node{}}
	for name, e := range idx.entries {
		parts := strings.Split(name, "/")
		cur := root
		for _, part := range parts[:len(parts)-1] {
			next, ok := cur.dirs[part]
			if !ok {
				next = &node{entries: map[string]*indexEntry{}, dirs: map[string]*node{}}
				cur.dirs[part] = next
			}
			cur = next
		}
		cur.entries[parts[len(parts)-1]] = e
	}

	var build func(n *node) (objectID, error)
	build = func(n *node) (objectID, error) {
		var tree []treeEntry
		for name, e := range n.entries {
			tree = append(tree, treeEntry{name: name, mode: gitModeString(e.mode), id: e.id})
		}
		for name, child := range n.dirs {
			id, err := build(child)
			if err != nil {
				return objectID{}, err
			}
			tree = append(tree, treeEntry{name: name, mode: "40000", id: id})
		}
		return s.put(kindTree, encodeTree(tree))
	}
	return build(root)
}

// gitModeString renders a mode the way git stores it in a tree: the directory
// mode loses its leading zero.
func gitModeString(mode uint32) string {
	if mode == modeTree || mode == 0o40000 {
		return "40000"
	}
	return fmt.Sprintf("%o", mode)
}
