package gitstore

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Pack file support.
//
// The reference reads through dulwich, which resolves an object name in loose
// files or in any pack. Nothing in GitStore ever runs `git gc`, so packs only
// appear when a user packs the workspace repository themselves — but a port
// that ignored them would report an empty log and an empty working-tree summary
// for such a repository while the reference kept working. Reading packs is
// therefore part of matching the reference, not an optimisation.
//
// Only reads are implemented: the reference never writes a pack (dulwich writes
// loose objects and an index file, exactly like this package).

const (
	packTypeCommit   = 1
	packTypeTree     = 2
	packTypeBlob     = 3
	packTypeTag      = 4
	packTypeOfsDelta = 6
	packTypeRefDelta = 7
)

// packFile is one .pack plus its .idx.
type packFile struct {
	packPath string
	idx      *packIndex
	store    *objectStore // for ref-delta bases that live outside this pack
}

// packIndex holds the object-name → offset table of a version 2 .idx.
type packIndex struct {
	names   []objectID
	offsets []uint64
}

// packLookup finds the offset of id, or false when the pack does not contain it.
func (p *packIndex) lookup(id objectID) (uint64, bool) {
	i := sort.Search(len(p.names), func(i int) bool {
		return bytes.Compare(p.names[i][:], id[:]) >= 0
	})
	if i < len(p.names) && p.names[i] == id {
		return p.offsets[i], true
	}
	return 0, false
}

// loadPacks enumerates .git/objects/pack/*.idx. Failures are reported to the
// caller only when an object is actually missing, so an unreadable pack never
// breaks an operation that loose objects could satisfy.
func (s *objectStore) loadPacks() []*packFile {
	if s.packsOK {
		return s.packs
	}
	s.packsOK = true
	dir := filepath.Join(s.dir, "pack")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".idx" {
			continue
		}
		idxPath := filepath.Join(dir, ent.Name())
		idx, err := readPackIndex(idxPath)
		if err != nil {
			continue
		}
		packPath := idxPath[:len(idxPath)-len(".idx")] + ".pack"
		if _, err := os.Stat(packPath); err != nil {
			continue
		}
		s.packs = append(s.packs, &packFile{packPath: packPath, idx: idx, store: s})
	}
	return s.packs
}

// readPackIndex parses a version 2 .idx file.
func readPackIndex(path string) (*packIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 8+256*4+40 {
		return nil, fmt.Errorf("gitstore: pack index %s is truncated", path)
	}
	if !bytes.Equal(data[:4], []byte{0xff, 't', 'O', 'c'}) {
		return nil, fmt.Errorf("gitstore: pack index %s is not version 2", path)
	}
	if version := binary.BigEndian.Uint32(data[4:8]); version != 2 {
		return nil, fmt.Errorf("gitstore: pack index %s has version %d", path, version)
	}
	fanout := data[8 : 8+256*4]
	count := int(binary.BigEndian.Uint32(fanout[255*4:]))
	namesStart := 8 + 256*4
	crcStart := namesStart + count*20
	offStart := crcStart + count*4
	bigStart := offStart + count*4
	if len(data) < bigStart {
		return nil, fmt.Errorf("gitstore: pack index %s is truncated", path)
	}
	idx := &packIndex{
		names:   make([]objectID, count),
		offsets: make([]uint64, count),
	}
	for i := 0; i < count; i++ {
		id, err := objectIDFromBytes(data[namesStart+i*20 : namesStart+i*20+20])
		if err != nil {
			return nil, err
		}
		idx.names[i] = id
	}
	nBig := 0
	for i := 0; i < count; i++ {
		v := binary.BigEndian.Uint32(data[offStart+i*4:])
		if v&0x80000000 != 0 {
			nBig++
		}
	}
	if len(data) < bigStart+nBig*8 {
		return nil, fmt.Errorf("gitstore: pack index %s is truncated", path)
	}
	for i := 0; i < count; i++ {
		v := binary.BigEndian.Uint32(data[offStart+i*4:])
		if v&0x80000000 != 0 {
			n := int(v & 0x7fffffff)
			if n >= nBig {
				return nil, fmt.Errorf("gitstore: pack index %s has a bad offset", path)
			}
			idx.offsets[i] = binary.BigEndian.Uint64(data[bigStart+n*8:])
		} else {
			idx.offsets[i] = uint64(v)
		}
	}
	return idx, nil
}

// getPacked resolves id from the pack files, or reports os.ErrNotExist.
func (s *objectStore) getPacked(id objectID) (rawObject, error) {
	for _, p := range s.loadPacks() {
		off, ok := p.idx.lookup(id)
		if !ok {
			continue
		}
		return p.readObjectAt(off, 0)
	}
	return rawObject{}, fmt.Errorf("gitstore: object %s: %w", id, os.ErrNotExist)
}

// readObjectAt decodes the object at a pack offset, following deltas.
//
// depth bounds the delta chain so a corrupt pack cannot recurse forever.
func (p *packFile) readObjectAt(offset uint64, depth int) (rawObject, error) {
	f, err := os.Open(p.packPath)
	if err != nil {
		return rawObject{}, err
	}
	defer f.Close()
	return p.readObjectFrom(f, offset, depth)
}

func (p *packFile) readObjectFrom(f *os.File, offset uint64, depth int) (rawObject, error) {
	if depth > 64 {
		return rawObject{}, errors.New("gitstore: pack delta chain is too deep")
	}
	if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
		return rawObject{}, err
	}
	br := newByteReader(f)
	header, err := br.readByte()
	if err != nil {
		return rawObject{}, err
	}
	kind := int((header >> 4) & 7)
	size := uint64(header & 0x0f)
	shift := uint(4)
	for header&0x80 != 0 {
		header, err = br.readByte()
		if err != nil {
			return rawObject{}, err
		}
		size |= uint64(header&0x7f) << shift
		shift += 7
	}

	switch kind {
	case packTypeCommit, packTypeTree, packTypeBlob, packTypeTag:
		data, err := br.inflate(size)
		if err != nil {
			return rawObject{}, err
		}
		return rawObject{kind: packKindName(kind), data: data}, nil
	case packTypeOfsDelta:
		// The base offset is a varint-encoded distance backwards from this
		// object's own offset (git's "offset encoding").
		b, err := br.readByte()
		if err != nil {
			return rawObject{}, err
		}
		baseRel := uint64(b & 0x7f)
		for b&0x80 != 0 {
			b, err = br.readByte()
			if err != nil {
				return rawObject{}, err
			}
			baseRel = ((baseRel + 1) << 7) | uint64(b&0x7f)
		}
		if baseRel > offset {
			return rawObject{}, errors.New("gitstore: pack ofs-delta base is out of range")
		}
		// The delta instructions are read BEFORE the base is resolved: reading
		// the base re-seeks the shared file handle, which would otherwise move
		// the stream out from under this read.
		delta, err := br.inflate(size)
		if err != nil {
			return rawObject{}, err
		}
		base, err := p.readObjectFrom(f, offset-baseRel, depth+1)
		if err != nil {
			return rawObject{}, err
		}
		data, err := applyDelta(base.data, delta)
		if err != nil {
			return rawObject{}, err
		}
		return rawObject{kind: base.kind, data: data}, nil
	case packTypeRefDelta:
		var id objectID
		if _, err := io.ReadFull(br, id[:]); err != nil {
			return rawObject{}, err
		}
		// As above: inflate first so resolving the base cannot disturb the
		// stream this read is positioned on.
		delta, err := br.inflate(size)
		if err != nil {
			return rawObject{}, err
		}
		baseObj, err := p.store.get(id)
		if err != nil {
			return rawObject{}, err
		}
		data, err := applyDelta(baseObj.data, delta)
		if err != nil {
			return rawObject{}, err
		}
		return rawObject{kind: baseObj.kind, data: data}, nil
	default:
		return rawObject{}, fmt.Errorf("gitstore: unsupported pack object type %d", kind)
	}
}

func packKindName(t int) objKind {
	switch t {
	case packTypeCommit:
		return kindCommit
	case packTypeTree:
		return kindTree
	case packTypeBlob:
		return kindBlob
	default:
		return kindTag
	}
}

// applyDelta applies a git delta (copy/insert instructions) to base.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (uint64, error) {
		var v uint64
		var shift uint
		for {
			if pos >= len(delta) {
				return 0, errors.New("gitstore: truncated delta")
			}
			b := delta[pos]
			pos++
			v |= uint64(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return v, nil
			}
		}
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != uint64(len(base)) {
		return nil, fmt.Errorf("gitstore: delta expects a %d-byte base, have %d", srcSize, len(base))
	}
	dstSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, dstSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			var copyOffset, copySize uint64
			for i := uint(0); i < 4; i++ {
				if op&(1<<i) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("gitstore: truncated delta copy")
					}
					copyOffset |= uint64(delta[pos]) << (8 * i)
					pos++
				}
			}
			for i := uint(0); i < 3; i++ {
				if op&(1<<(4+i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("gitstore: truncated delta copy")
					}
					copySize |= uint64(delta[pos]) << (8 * i)
					pos++
				}
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset+copySize > uint64(len(base)) {
				return nil, errors.New("gitstore: delta copy is out of range")
			}
			out = append(out, base[copyOffset:copyOffset+copySize]...)
			continue
		}
		if op == 0 {
			return nil, errors.New("gitstore: invalid delta opcode 0")
		}
		n := int(op)
		if pos+n > len(delta) {
			return nil, errors.New("gitstore: truncated delta insert")
		}
		out = append(out, delta[pos:pos+n]...)
		pos += n
	}
	if uint64(len(out)) != dstSize {
		return nil, fmt.Errorf("gitstore: delta produced %d bytes, expected %d", len(out), dstSize)
	}
	return out, nil
}

// byteReader reads from an io.Reader one byte at a time and can inflate a zlib
// stream that starts at the current position.
type byteReader struct {
	r   io.Reader
	buf [1]byte
}

func newByteReader(r io.Reader) *byteReader { return &byteReader{r: r} }

func (b *byteReader) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *byteReader) readByte() (byte, error) {
	if _, err := io.ReadFull(b.r, b.buf[:]); err != nil {
		return 0, err
	}
	return b.buf[0], nil
}

// inflate reads one zlib stream and returns its uncompressed bytes, checking the
// declared size.
func (b *byteReader) inflate(size uint64) ([]byte, error) {
	zr, err := zlib.NewReader(b)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	if uint64(len(data)) != size {
		return nil, fmt.Errorf("gitstore: packed object declares %d bytes, has %d", size, len(data))
	}
	return data, nil
}
