// Package gitstore ports nanobot/utils/gitstore.py — the git-backed version
// control layer for the memory files (SOUL.md, USER.md, memory/MEMORY.md,
// memory/.dream_cursor).
//
// The Python reference does NOT shell out to the `git` binary: it imports
// dulwich, a pure-Python implementation of the git object model
// (upstream/nanobot/nanobot/utils/gitstore.py:78, :134, :244, ...). A port that
// called `git` would therefore be wrong in two observable ways: it would fail
// outright on a machine without git installed (the reference works fine), and
// it would inherit git's own configuration, identity and error handling, none
// of which the reference uses. This package therefore implements the slice of
// the git object model that the reference exercises, using only the standard
// library, and writes objects, refs and indexes that the real git CLI and
// dulwich both read back cleanly (verified in compat/).
package gitstore

// This file ports the parts of Python's difflib that the reference depends on.
//
// Two different callers need it, and they must agree exactly:
//
//   - GitStore.summarize_working_tree (gitstore.py:371) calls
//     difflib.unified_diff on the *decoded text lines* of the two versions.
//   - diff_commits (gitstore.py:290) calls dulwich's porcelain.diff, which
//     builds its patch with dulwich.patch.unified_diff — and that function
//     imports difflib.SequenceMatcher directly (dulwich/patch.py:58, :267),
//     so it is the very same matcher, only over bytes lines.
//
// Everything here is a faithful transcription of CPython 3.14's difflib,
// including the two behaviours that a reader would not guess:
//
//   - SequenceMatcher's "autojunk" heuristic: for a sequence of 200+ elements,
//     any element appearing more than 1%+1 times is dropped from the index and
//     therefore cannot anchor a matching block (difflib.py:__chain_b).
//   - get_grouped_opcodes' trimming of the leading/trailing equal runs.
//
// Ported so that both diffs are byte-identical to the reference; the
// differential test in compat/ compares them against the real Python.

import (
	"sort"
	"strconv"
)

// seqMatch is difflib.Match: a matching block a[i:i+k] == b[j:j+k].
type seqMatch struct{ i, j, k int }

// seqOpcode is a difflib opcode: one of "replace", "delete", "insert", "equal".
type seqOpcode struct {
	tag    string
	i1, i2 int
	j1, j2 int
}

// seqMatcher is difflib.SequenceMatcher over []string. Lines are compared for
// equality only, so a Go string (arbitrary bytes) is a faithful stand-in for
// both Python str elements and Python bytes elements.
type seqMatcher struct {
	a, b     []string
	isJunk   func(string) bool
	autojunk bool

	b2j      map[string][]int // element -> ascending indices in b (junk/popular purged)
	bjunk    map[string]bool
	bpopular map[string]bool

	matchingBlocks []seqMatch
	opcodes        []seqOpcode
}

// newSeqMatcher mirrors SequenceMatcher(isjunk, a, b, autojunk=True) followed
// by set_seqs. isJunk is nil for every call site in the reference.
func newSeqMatcher(isJunk func(string) bool, a, b []string) *seqMatcher {
	m := &seqMatcher{a: a, b: b, isJunk: isJunk, autojunk: true}
	m.chainB()
	return m
}

// chainB mirrors SequenceMatcher.__chain_b (difflib.py).
func (m *seqMatcher) chainB() {
	b := m.b
	m.b2j = make(map[string][]int)
	for i, elt := range b {
		m.b2j[elt] = append(m.b2j[elt], i)
	}

	m.bjunk = make(map[string]bool)
	if m.isJunk != nil {
		for elt := range m.b2j {
			if m.isJunk(elt) {
				m.bjunk[elt] = true
			}
		}
		for elt := range m.bjunk {
			delete(m.b2j, elt)
		}
	}

	// "purge popular elements that are not junk": n >= 200 and the element
	// occurs more than n//100+1 times.
	m.bpopular = make(map[string]bool)
	n := len(b)
	if m.autojunk && n >= 200 {
		ntest := n/100 + 1
		for elt, idxs := range m.b2j {
			if len(idxs) > ntest {
				m.bpopular[elt] = true
			}
		}
		for elt := range m.bpopular {
			delete(m.b2j, elt)
		}
	}
}

// findLongestMatch mirrors SequenceMatcher.find_longest_match (difflib.py).
func (m *seqMatcher) findLongestMatch(alo, ahi, blo, bhi int) seqMatch {
	a, b, b2j := m.a, m.b, m.b2j
	isbjunk := func(s string) bool { return m.bjunk[s] }

	besti, bestj, bestsize := alo, blo, 0
	j2len := map[int]int{}
	for i := alo; i < ahi; i++ {
		newj2len := map[int]int{}
		for _, j := range b2j[a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := j2len[j-1] + 1
			newj2len[j] = k
			if k > bestsize {
				besti, bestj, bestsize = i-k+1, j-k+1, k
			}
		}
		j2len = newj2len
	}

	// Extend the best by non-junk elements on each end.
	for besti > alo && bestj > blo && !isbjunk(b[bestj-1]) && a[besti-1] == b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi &&
		!isbjunk(b[bestj+bestsize]) && a[besti+bestsize] == b[bestj+bestsize] {
		bestsize++
	}

	// Suck up the matching junk on each side of the match too.
	for besti > alo && bestj > blo && isbjunk(b[bestj-1]) && a[besti-1] == b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi &&
		isbjunk(b[bestj+bestsize]) && a[besti+bestsize] == b[bestj+bestsize] {
		bestsize++
	}

	return seqMatch{besti, bestj, bestsize}
}

// getMatchingBlocks mirrors SequenceMatcher.get_matching_blocks (difflib.py).
func (m *seqMatcher) getMatchingBlocks() []seqMatch {
	if m.matchingBlocks != nil {
		return m.matchingBlocks
	}
	la, lb := len(m.a), len(m.b)

	// The Python code pushes (alo, ahi, blo, bhi) tuples and pops from the end
	// (LIFO); keep the same order even though the result is sorted afterwards.
	type region struct{ alo, ahi, blo, bhi int }
	stack := []region{{0, la, 0, lb}}
	var blocks []seqMatch
	for len(stack) > 0 {
		r := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		x := m.findLongestMatch(r.alo, r.ahi, r.blo, r.bhi)
		if x.k != 0 {
			blocks = append(blocks, x)
			if r.alo < x.i && r.blo < x.j {
				stack = append(stack, region{r.alo, x.i, r.blo, x.j})
			}
			if x.i+x.k < r.ahi && x.j+x.k < r.bhi {
				stack = append(stack, region{x.i + x.k, r.ahi, x.j + x.k, r.bhi})
			}
		}
	}
	sort.Slice(blocks, func(p, q int) bool {
		if blocks[p].i != blocks[q].i {
			return blocks[p].i < blocks[q].i
		}
		if blocks[p].j != blocks[q].j {
			return blocks[p].j < blocks[q].j
		}
		return blocks[p].k < blocks[q].k
	})

	// Collapse adjacent blocks.
	i1, j1, k1 := 0, 0, 0
	var nonAdjacent []seqMatch
	for _, b := range blocks {
		if i1+k1 == b.i && j1+k1 == b.j {
			k1 += b.k
		} else {
			if k1 != 0 {
				nonAdjacent = append(nonAdjacent, seqMatch{i1, j1, k1})
			}
			i1, j1, k1 = b.i, b.j, b.k
		}
	}
	if k1 != 0 {
		nonAdjacent = append(nonAdjacent, seqMatch{i1, j1, k1})
	}
	nonAdjacent = append(nonAdjacent, seqMatch{la, lb, 0})

	m.matchingBlocks = nonAdjacent
	return m.matchingBlocks
}

// getOpcodes mirrors SequenceMatcher.get_opcodes (difflib.py).
func (m *seqMatcher) getOpcodes() []seqOpcode {
	if m.opcodes != nil {
		return m.opcodes
	}
	i, j := 0, 0
	var answer []seqOpcode
	for _, blk := range m.getMatchingBlocks() {
		ai, bj, size := blk.i, blk.j, blk.k
		tag := ""
		if i < ai && j < bj {
			tag = "replace"
		} else if i < ai {
			tag = "delete"
		} else if j < bj {
			tag = "insert"
		}
		if tag != "" {
			answer = append(answer, seqOpcode{tag, i, ai, j, bj})
		}
		i, j = ai+size, bj+size
		if size != 0 {
			answer = append(answer, seqOpcode{"equal", ai, ai + size, bj, bj + size})
		}
	}
	m.opcodes = answer
	return answer
}

// getGroupedOpcodes mirrors SequenceMatcher.get_grouped_opcodes(n) (difflib.py).
func (m *seqMatcher) getGroupedOpcodes(n int) [][]seqOpcode {
	codes := m.getOpcodes()
	if len(codes) == 0 {
		codes = []seqOpcode{{"equal", 0, 1, 0, 1}}
	}
	// Fix up leading and trailing groups if they show no changes.
	if codes[0].tag == "equal" {
		c := codes[0]
		codes[0] = seqOpcode{c.tag, max(c.i1, c.i2-n), c.i2, max(c.j1, c.j2-n), c.j2}
	}
	if last := codes[len(codes)-1]; last.tag == "equal" {
		codes[len(codes)-1] = seqOpcode{last.tag, last.i1, min(last.i2, last.i1+n), last.j1, min(last.j2, last.j1+n)}
	}

	nn := n + n
	var group []seqOpcode
	var groups [][]seqOpcode
	for _, c := range codes {
		tag, i1, i2, j1, j2 := c.tag, c.i1, c.i2, c.j1, c.j2
		if tag == "equal" && i2-i1 > nn {
			group = append(group, seqOpcode{tag, i1, min(i2, i1+n), j1, min(j2, j1+n)})
			groups = append(groups, group)
			group = nil
			i1, j1 = max(i1, i2-n), max(j1, j2-n)
		}
		group = append(group, seqOpcode{tag, i1, i2, j1, j2})
	}
	if len(group) > 0 && !(len(group) == 1 && group[0].tag == "equal") {
		groups = append(groups, group)
	}
	return groups
}

// formatRangeUnified mirrors difflib._format_range_unified.
func formatRangeUnified(start, stop int) string {
	beginning := start + 1 // lines start numbering with one
	length := stop - start
	if length == 1 {
		return strconv.Itoa(beginning)
	}
	if length == 0 {
		beginning-- // empty ranges begin at line just before the range
	}
	return strconv.Itoa(beginning) + "," + strconv.Itoa(length)
}

// unifiedDiff mirrors difflib.unified_diff with fromfiledate/tofiledate empty.
//
// The reference always passes lineterm="" (gitstore.py:376), so no terminator
// is appended to the header lines or to the hunk body; the caller joins the
// result with "\n".
func unifiedDiff(a, b []string, fromFile, toFile string, n int, lineTerm string) []string {
	var out []string
	started := false
	for _, group := range newSeqMatcher(nil, a, b).getGroupedOpcodes(n) {
		if !started {
			started = true
			out = append(out, "--- "+fromFile+lineTerm)
			out = append(out, "+++ "+toFile+lineTerm)
		}
		first, last := group[0], group[len(group)-1]
		file1Range := formatRangeUnified(first.i1, last.i2)
		file2Range := formatRangeUnified(first.j1, last.j2)
		out = append(out, "@@ -"+file1Range+" +"+file2Range+" @@"+lineTerm)
		for _, op := range group {
			if op.tag == "equal" {
				for _, line := range a[op.i1:op.i2] {
					out = append(out, " "+line)
				}
				continue
			}
			if op.tag == "replace" || op.tag == "delete" {
				for _, line := range a[op.i1:op.i2] {
					out = append(out, "-"+line)
				}
			}
			if op.tag == "replace" || op.tag == "insert" {
				for _, line := range b[op.j1:op.j2] {
					out = append(out, "+"+line)
				}
			}
		}
	}
	return out
}
