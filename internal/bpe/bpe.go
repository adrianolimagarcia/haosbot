package bpe

import (
	"container/heap"
	"math"
	"strings"
)

// EncodeOrdinary encodes text as natural text, ignoring special tokens.
//
// It is Encoding.encode_ordinary, equivalently `encode(text,
// disallowed_special=())`: the five special token strings are encoded as
// ordinary bytes.
func EncodeOrdinary(text string) ([]uint32, error) {
	v, err := getVocab()
	if err != nil {
		return nil, err
	}
	return v.encodeOrdinary(text), nil
}

// Encode encodes text the way tiktoken's defaults do.
//
// tiktoken's default is `allowed_special=set(), disallowed_special="all"`, so
// this returns a *SpecialTokenError when text contains any of the five special
// token strings, and is otherwise identical to EncodeOrdinary. Every call site
// in the reference uses these defaults, and the `except Exception` around them
// in helpers.py is what routes such text to the byte heuristic — so callers
// here must treat the error as "fall back", not as "fail".
func Encode(text string) ([]uint32, error) {
	if tok := findSpecialToken(text); tok != "" {
		return nil, &SpecialTokenError{Token: tok}
	}
	return EncodeOrdinary(text)
}

// TokenCount is `len(enc.encode(text))`, with the same special-token error.
func TokenCount(text string) (int, error) {
	tokens, err := Encode(text)
	if err != nil {
		return 0, err
	}
	return len(tokens), nil
}

// findSpecialToken reports the first special token string appearing in text,
// or "".
//
// tiktoken builds a `re.compile("(tok1|tok2|...)")` over the disallowed set and
// searches with it; the order of that alternation is a frozenset iteration
// order, but no special token is a substring of another, so any order finds the
// same set. Scanning for each token in turn is equivalent and allocation-free.
func findSpecialToken(text string) string {
	for _, st := range specialTokens {
		if strings.Contains(text, st.text) {
			return st.text
		}
	}
	return ""
}

// DecodeBytes concatenates the bytes of tokens, mirroring
// Encoding.decode_bytes.
//
// Token IDs outside the vocabulary are skipped rather than panicking the way
// the Rust decoder's `unwrap()` does; no encoder path can produce one.
func DecodeBytes(tokens []uint32) []byte {
	v, err := getVocab()
	if err != nil {
		return nil
	}
	n := 0
	for _, t := range tokens {
		if b, ok := v.token(t); ok {
			n += len(b)
		}
	}
	out := make([]byte, 0, n)
	for _, t := range tokens {
		if b, ok := v.token(t); ok {
			out = append(out, b...)
		}
	}
	return out
}

// Decode is Encoding.decode: the token bytes, then Python's
// `bytes.decode("utf-8", errors="replace")`.
func Decode(tokens []uint32) string {
	return decodeUTF8Replace(DecodeBytes(tokens))
}

// encodeOrdinary is the body of Encoding.encode_ordinary.
func (v *vocabulary) encodeOrdinary(text string) []uint32 {
	out := make([]uint32, 0, len(text)/3+1)
	for _, p := range ScanPieces(text) {
		out = v.encodePiece([]byte(text[p.Start:p.End]), out)
	}
	return out
}

// encodePiece appends the tokens of one pre-tokenized piece.
//
// Mirrors CoreBPE's `if let Some(token) = encoder.get(piece) { push } else {
// _byte_pair_encode(...) }`. The whole-piece shortcut is not merely an
// optimisation for a well-formed BPE vocabulary, but it is what tiktoken does,
// so it is what this does: it makes the result independent of whether the
// greedy merge happens to reach the same token.
func (v *vocabulary) encodePiece(piece []byte, dst []uint32) []uint32 {
	if len(piece) == 0 {
		return dst
	}
	if r, ok := v.rank(piece); ok {
		return append(dst, r)
	}
	return v.mergeParts(piece, dst)
}

// part is one element of the merge list.
//
// start is the byte offset at which the part begins. rank is the merge priority
// of the PAIR formed by this part and the NEXT one — not the rank of the part
// itself. That distinction is the whole subtlety of the reference algorithm:
// reading rank as "the part's own token rank" produces output that looks
// plausible and is wrong for any piece whose BPE needs more than one merge.
type part struct {
	start int
	rank  uint32
}

// maxRank is Rust's Rank::MAX, i.e. "this pair is not a token".
const maxRank = uint32(math.MaxUint32)

// mergeParts is byte_pair_encode -> _byte_pair_merge for pieces shorter than
// 100 bytes, and _byte_pair_merge_large for the rest.
//
// tiktoken's Rust source (src/lib.rs) is the authority here, and it is worth
// restating the small path because the obvious implementation is wrong:
//
//   - `parts` is a list of (start, rank) where rank belongs to the pair
//     (parts[i], parts[i+1]), so the pair's byte span is
//     piece[parts[i].start : parts[i+2].start]. The list is seeded with one
//     entry per BYTE plus TWO sentinels, so parts always has len(piece)+1
//     entries.
//   - The initial pass fills rank from `piece[i:i+2]` and remembers the
//     leftmost minimum.
//   - A merge at i sets parts[i-1].rank and parts[i].rank and then removes
//     parts[i+1]. Because the removal has not happened yet, the new ranks are
//     read from `piece[parts[i].start : parts[i+3].start]` — the `+3` is the
//     comment's "we haven't yet deleted parts[i + 1]".
//   - After the removal the minimum is recomputed by a full scan over
//     parts[:len-1] with a strict `<`, so ties go to the leftmost pair.
//   - The output is not the ranks in parts; it is a fresh lookup of each
//     surviving span piece[parts[j].start : parts[j+1].start].
//
// The small loop is O(m*n) in the piece length, exactly like the reference's,
// which is why the reference switches to a heap at 100 bytes and so does this.
// The switch is not a micro-optimisation: CJK text is one \p{L} run, so a
// paragraph of it is a single multi-kilobyte piece, and the quadratic path
// tokenises it at well under 1 KB/s.
//
// The two paths are equivalent by construction and the differential corpus
// proves it: the sweeps below encode every Unicode scalar value concatenated
// into 4-5 MB strings, which is nothing but pieces far past the threshold.
func (v *vocabulary) mergeParts(piece []byte, dst []uint32) []uint32 {
	if len(piece) < 100 {
		return v.mergePartsSmall(piece, dst)
	}
	return v.mergePartsLarge(piece, dst)
}

func (v *vocabulary) mergePartsSmall(piece []byte, dst []uint32) []uint32 {
	if len(piece) == 0 {
		return dst
	}
	if len(piece) == 1 {
		if r, ok := v.rank(piece); ok {
			return append(dst, r)
		}
		return dst
	}

	parts := make([]part, 0, len(piece)+1)
	minRank := maxRank
	minIdx := -1
	for i := 0; i+1 < len(piece); i++ {
		r := maxRank
		if got, ok := v.rank(piece[i : i+2]); ok {
			r = got
		}
		if r < minRank {
			minRank, minIdx = r, i
		}
		parts = append(parts, part{start: i, rank: r})
	}
	parts = append(parts,
		part{start: len(piece) - 1, rank: maxRank},
		part{start: len(piece), rank: maxRank},
	)

	// getRank is the closure of the same name in the reference: the rank of the
	// pair that would start at parts[i] once parts[i+1] is gone.
	getRank := func(i int) uint32 {
		if i+3 >= len(parts) {
			return maxRank
		}
		if r, ok := v.rank(piece[parts[i].start:parts[i+3].start]); ok {
			return r
		}
		return maxRank
	}

	for minRank != maxRank {
		i := minIdx
		if i > 0 {
			parts[i-1].rank = getRank(i - 1)
		}
		parts[i].rank = getRank(i)
		parts = append(parts[:i+1], parts[i+2:]...)

		minRank, minIdx = maxRank, -1
		for j := 0; j+1 < len(parts); j++ {
			if parts[j].rank < minRank {
				minRank, minIdx = parts[j].rank, j
			}
		}
	}

	for j := 0; j+1 < len(parts); j++ {
		if r, ok := v.rank(piece[parts[j].start:parts[j+1].start]); ok {
			dst = append(dst, r)
		}
	}
	return dst
}

// mergeState is the reference's `State` (src/lib.rs:37). Indexed by byte
// offset, one entry per byte of the piece. `end` is where the token starting
// here currently ends, `nextEnd` where the token after that ends, `nextRank`
// the rank of merging this token with the next one, and `curRank` the rank this
// token was last merged at (Rank::MAX meaning "not merged").
type mergeState struct {
	prev     int
	end      int
	nextEnd  int
	nextRank uint32
	curRank  uint32
}

// mergeEntry is the reference's `Merge`. Its Ord is:
//
//	other.rank.cmp(&self.rank).then_with(|| other.start.cmp(&self.start))
//
// BinaryHeap is a max-heap, so that ordering pops the SMALLEST rank first and
// breaks ties by the SMALLEST start. Go's container/heap is a min-heap, so Less
// is the direct reading of the same relation.
type mergeEntry struct {
	start int
	rank  uint32
}

type mergeHeap []mergeEntry

func (h mergeHeap) Len() int { return len(h) }
func (h mergeHeap) Less(i, j int) bool {
	if h[i].rank != h[j].rank {
		return h[i].rank < h[j].rank
	}
	return h[i].start < h[j].start
}
func (h mergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *mergeHeap) Push(x any)   { *h = append(*h, x.(mergeEntry)) }

func (h *mergeHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// mergePartsLarge is _byte_pair_merge_large (src/lib.rs:47), the path
// byte_pair_encode takes for pieces of 100 bytes or more.
//
// It keeps the same greedy invariant — always perform the lowest-ranked
// available merge, leftmost first on ties — but finds it through a heap instead
// of rescanning the part list, so the merge loop is O(n log n).
//
// Stale heap entries are not removed; they are recognised on pop because
// `state[start].nextRank` no longer matches, and skipped. `state[start].nextRank`
// is also the validity check for a duplicate: two entries for the same start
// and the same rank are interchangeable, so the reference's non-stable heap
// order between them cannot change the output.
func (v *vocabulary) mergePartsLarge(piece []byte, dst []uint32) []uint32 {
	n := len(piece)
	state := make([]mergeState, 0, n)
	state = append(state, mergeState{
		prev: -1, end: 1, nextEnd: 2, nextRank: maxRank, curRank: maxRank,
	})

	h := make(mergeHeap, 0, n)
	for i := 0; i+1 < n; i++ {
		if r, ok := v.rank(piece[i : i+2]); ok {
			h = append(h, mergeEntry{start: i, rank: r})
			state[i].nextRank = r
		}
		// note this is happening offset by 1
		state = append(state, mergeState{
			prev: i, end: i + 2, nextEnd: i + 3, nextRank: maxRank, curRank: maxRank,
		})
	}
	heap.Init(&h)

	// potentialMerge is the reference's `potential_merge` closure: record that
	// the token starting at `start` would now end at nextEndItem, invalidate the
	// old merge, and offer the new one if it is a real token.
	potentialMerge := func(start, nextEndItem int) {
		state[start].nextEnd = nextEndItem
		state[start].nextRank = maxRank // Always invalidate the old merge
		if nextEndItem <= n {
			if r, ok := v.rank(piece[start:nextEndItem]); ok {
				heap.Push(&h, mergeEntry{start: start, rank: r})
				state[start].nextRank = r
			}
		}
	}

	for h.Len() > 0 {
		left := heap.Pop(&h).(mergeEntry)
		if left.rank == maxRank {
			break
		}
		if left.rank != state[left.start].nextRank {
			continue // This merge was invalidated, ignore it
		}

		leftStart := left.start
		rightStart := state[leftStart].end
		rightEnd := state[leftStart].nextEnd
		rightNextEnd := state[rightStart].nextEnd

		// Merge left and right into a single token
		state[leftStart].curRank = state[leftStart].nextRank
		state[leftStart].end = rightEnd
		potentialMerge(leftStart, rightNextEnd)
		if rightEnd < len(state) {
			state[rightEnd].prev = leftStart
		}
		// Update the merge that ends at leftStart
		if leftStart > 0 {
			potentialMerge(state[leftStart].prev, rightEnd)
		}
		// Invalidate the merge starting at rightStart
		state[rightStart].nextRank = maxRank
	}

	for i := 0; i < len(state); {
		if state[i].curRank != maxRank {
			dst = append(dst, state[i].curRank)
		} else if r, ok := v.rank(piece[i:state[i].end]); ok {
			dst = append(dst, r)
		}
		i = state[i].end
	}
	return dst
}
