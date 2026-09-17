package filediff

// Port of rapidfuzz.distance.Indel.opcodes, the alignment FileDiff.from_text
// depends on (utils/file_edit_events.py:62).
//
// rapidfuzz's Indel opcodes are lcs_seq_editops:
//
//	C++   rapidfuzz/distance/LCSseq_impl.hpp: lcs_seq_editops ->
//	      remove_common_affix + lcs_matrix + recover_alignment
//	Python rapidfuzz/distance/LCSseq_py.py: editops()/opcodes()
//
// Both were read at rapidfuzz 3.14.6 (the version installed in the reference
// venv) and the port below reproduces the Python form, which is the one whose
// semantics are documented in-tree. See indel_test.go for the differential
// evidence.
//
// Three properties matter and none of them are difflib's:
//
//  1. Only "equal", "insert" and "delete" are produced. "replace" never
//     appears; the branches that test for it are kept anyway because the
//     reference's added/deleted derivation and unified_lines both test for it.
//  2. The alignment is an optimal indel-distance (equivalently LCS)
//     alignment. difflib.SequenceMatcher is not guaranteed to find the longest
//     common subsequence and applies autojunk, so it disagrees.
//  3. The tie-break is not "prefer insert" or "prefer delete": it is whatever
//     the backward recovery walk over the bit-parallel LCS matrix produces.
//     In particular a repeated line is matched at the position the matrix
//     picks, which is neither the earliest nor the latest occurrence in
//     general. A hand-written LCS DP walk was tried first and disagreed on
//     roughly 40% of small adversarial inputs.

// Tag is an opcode tag. IndelOpcodes only ever emits TagEqual, TagInsert and
// TagDelete.
type Tag string

// Opcode tags. TagReplace mirrors rapidfuzz's tag set and is tested for by the
// added/deleted derivation and by unified_lines, but Indel never produces it.
const (
	TagEqual   Tag = "equal"
	TagInsert  Tag = "insert"
	TagDelete  Tag = "delete"
	TagReplace Tag = "replace"
)

// Opcode is one alignment block. Mirrors rapidfuzz.distance.Opcode
// (tag, src_start, src_end, dest_start, dest_end).
type Opcode struct {
	Tag       Tag
	SrcStart  int
	SrcEnd    int
	DestStart int
	DestEnd   int
}

// editop is one single-character edit operation, mirroring Editop(tag, src_pos,
// dest_pos) from rapidfuzz/distance/_initialize_py.py.
type editop struct {
	tag  Tag
	src  int
	dest int
}

// IndelOpcodes returns the rapidfuzz Indel alignment of before to after.
//
// Mirrors rapidfuzz.distance.LCSseq_py.opcodes (== Indel.opcodes). The result
// covers both sequences completely: it starts at (0,0), ends at
// (len(before), len(after)) and is contiguous, with no "replace" tag.
func IndelOpcodes(before, after []string) []Opcode {
	prefix, suffix := commonAffix(before, after)
	// s1/s2 are the affix-stripped middles; the returned positions are shifted
	// back by prefix (and the trailing region is covered by the equal block the
	// opcode conversion appends at the end).
	s1 := before[prefix : len(before)-suffix]
	s2 := after[prefix : len(after)-suffix]

	sim, matrix := lcsMatrix(s1, s2)

	srcLen := len(s1) + prefix + suffix
	destLen := len(s2) + prefix + suffix

	// dist is the indel distance: len(s1)+len(s2)-2*LCS.
	dist := len(s1) + len(s2) - 2*sim
	ops := make([]editop, dist)

	// Backward recovery walk (recover_alignment). col indexes s1, row indexes s2.
	col, row := len(s1), len(s2)
	for row != 0 && col != 0 {
		if matrix[row-1].test(col - 1) {
			// Deletion: s1[col-1] is not part of the alignment.
			dist--
			col--
			ops[dist] = editop{TagDelete, col + prefix, row + prefix}
			continue
		}
		row--
		if row != 0 && !matrix[row-1].test(col-1) {
			// Insertion: s2[row-1] is not part of the alignment.
			dist--
			ops[dist] = editop{TagInsert, col + prefix, row + prefix}
			continue
		}
		// Match: both positions advance without emitting an operation.
		col--
	}
	for col != 0 {
		dist--
		col--
		ops[dist] = editop{TagDelete, col + prefix, row + prefix}
	}
	for row != 0 {
		dist--
		row--
		ops[dist] = editop{TagInsert, col + prefix, row + prefix}
	}

	return opsToOpcodes(ops, srcLen, destLen)
}

// opsToOpcodes mirrors Editops.as_opcodes (rapidfuzz/distance/_initialize_py.py).
// Single operations are grouped into contiguous blocks and the gaps between
// them become "equal" blocks.
func opsToOpcodes(ops []editop, srcLen, destLen int) []Opcode {
	blocks := make([]Opcode, 0, len(ops)+1)
	srcPos, destPos := 0, 0
	for i := 0; i < len(ops); {
		if srcPos < ops[i].src || destPos < ops[i].dest {
			blocks = append(blocks, Opcode{
				Tag:       TagEqual,
				SrcStart:  srcPos,
				SrcEnd:    ops[i].src,
				DestStart: destPos,
				DestEnd:   ops[i].dest,
			})
			srcPos, destPos = ops[i].src, ops[i].dest
		}
		srcBegin, destBegin := srcPos, destPos
		tag := ops[i].tag
		for i < len(ops) && ops[i].tag == tag && srcPos == ops[i].src && destPos == ops[i].dest {
			switch tag {
			case TagReplace:
				srcPos++
				destPos++
			case TagInsert:
				destPos++
			case TagDelete:
				srcPos++
			}
			i++
		}
		blocks = append(blocks, Opcode{
			Tag:       tag,
			SrcStart:  srcBegin,
			SrcEnd:    srcPos,
			DestStart: destBegin,
			DestEnd:   destPos,
		})
	}
	if srcPos < srcLen || destPos < destLen {
		blocks = append(blocks, Opcode{
			Tag:       TagEqual,
			SrcStart:  srcPos,
			SrcEnd:    srcLen,
			DestStart: destPos,
			DestEnd:   destLen,
		})
	}
	return blocks
}

// commonAffix mirrors common_affix (rapidfuzz/_common_py.py:70-73): the common
// prefix, then the common suffix of what remains. Stripping the prefix before
// looking for the suffix is what keeps the two from overlapping.
func commonAffix(s1, s2 []string) (prefix, suffix int) {
	for prefix < len(s1) && prefix < len(s2) && s1[prefix] == s2[prefix] {
		prefix++
	}
	for suffix < len(s1)-prefix && suffix < len(s2)-prefix &&
		s1[len(s1)-1-suffix] == s2[len(s2)-1-suffix] {
		suffix++
	}
	return prefix, suffix
}

// lcsMatrix mirrors LCSseq_py._matrix: it returns the LCS length and one
// bit-vector per element of s2, where bit j of row i records whether s1[j] is
// matched after s2[:i+1] has been processed.
//
// The bit-parallel update is Hyyrö's:
//
//	S = (S + u) | (S - u),  u = S & Matches(s2[i])
//
// with S initialised to len(s1) one-bits, exactly as the Python reference does.
// Bits at or above len(s1) may be set by a carry out of the window; they are
// stored (so that the addition wraps like a fixed-width word vector) and are
// never read, because sim and the recovery walk only ever look at bits below
// len(s1).
func lcsMatrix(s1, s2 []string) (sim int, matrix []bitvec) {
	if len(s1) == 0 {
		return 0, nil
	}

	matches := make(map[string]bitvec, len(s1))
	for j, line := range s1 {
		m, ok := matches[line]
		if !ok {
			m = newBitvec(len(s1))
			matches[line] = m
		}
		m.set(j)
	}

	s := newBitvec(len(s1))
	for j := 0; j < len(s1); j++ {
		s.set(j)
	}

	matrix = make([]bitvec, len(s2))
	for i, line := range s2 {
		u := andBitvec(s, matches[line])
		s = orBitvec(addBitvec(s, u), subBitvec(s, u))
		matrix[i] = s
	}

	// bin(S)[-len(s1):].count("0") — the zero bits inside the window.
	sim = len(s1) - s.popcountBelow(len(s1))
	return sim, matrix
}

// ---------------------------------------------------------------------------
// Fixed-width little-endian bit vectors
// ---------------------------------------------------------------------------

// bitvec is a little-endian bit vector over 64-bit words. Bit j lives in word
// j/64 at position j%64, matching the C++ PatternMatchVector and the Python
// reference's integer bit positions.
type bitvec []uint64

func newBitvec(nbits int) bitvec { return make(bitvec, (nbits+63)/64) }

func (b bitvec) set(bit int) { b[bit>>6] |= 1 << uint(bit&63) }

func (b bitvec) test(bit int) bool { return b[bit>>6]&(1<<uint(bit&63)) != 0 }

// popcountBelow counts the set bits at positions below nbits. Bits at or above
// nbits can only have been set by a carry out of the window, and the reference's
// bin(S)[-len(s1):] ignores them, so they are excluded here too.
func (b bitvec) popcountBelow(nbits int) int {
	full := nbits / 64
	n := 0
	for i := 0; i < full && i < len(b); i++ {
		n += popcount64(b[i])
	}
	if rem := nbits % 64; rem != 0 && full < len(b) {
		n += popcount64(b[full] & ((uint64(1) << uint(rem)) - 1))
	}
	return n
}

func popcount64(x uint64) int {
	n := 0
	for x != 0 {
		x &= x - 1
		n++
	}
	return n
}

// andBitvec intersects two vectors. A nil right-hand side means "no matches",
// which is how a line of s2 that does not occur in s1 is represented.
func andBitvec(a, b bitvec) bitvec {
	out := make(bitvec, len(a))
	if b == nil {
		return out
	}
	for i := range a {
		out[i] = a[i] & b[i]
	}
	return out
}

func orBitvec(a, b bitvec) bitvec {
	out := make(bitvec, len(a))
	for i := range a {
		out[i] = a[i] | b[i]
	}
	return out
}

// addBitvec is a fixed-width addition: the carry out of the top word is
// discarded, exactly like the C++ addc64 chain over a fixed number of words.
func addBitvec(a, b bitvec) bitvec {
	out := make(bitvec, len(a))
	var carry uint64
	for i := range a {
		s := a[i] + b[i]
		var c uint64
		if s < a[i] {
			c = 1
		}
		s2 := s + carry
		if s2 < s {
			c = 1
		}
		out[i] = s2
		carry = c
	}
	return out
}

// subBitvec is a fixed-width subtraction. In the LCS update u is a bitwise
// subset of S, so the result is never negative and no borrow leaves the top.
func subBitvec(a, b bitvec) bitvec {
	out := make(bitvec, len(a))
	var borrow uint64
	for i := range a {
		d := a[i] - b[i]
		var bo uint64
		if a[i] < b[i] {
			bo = 1
		}
		d2 := d - borrow
		if d < borrow {
			bo = 1
		}
		out[i] = d2
		borrow = bo
	}
	return out
}
