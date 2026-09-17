package filediff

import (
	"reflect"
	"testing"
)

// op is a compact literal for the expected opcode table.
func op(tag Tag, ss, se, ds, de int) Opcode {
	return Opcode{Tag: tag, SrcStart: ss, SrcEnd: se, DestStart: ds, DestEnd: de}
}

// TestIndelOpcodesReferenceCases pins the alignment for inputs whose expected
// opcodes were READ OUT OF THE FROZEN REFERENCE, not derived by hand:
// rapidfuzz.distance.Indel.opcodes via compat/python/dump_apply_patch.py at
// HKUDS/nanobot@1bb712d3488915ca4ed9ccc1a93067ff722f5ab9.
//
// These particular inputs are the ones that falsified three earlier
// implementations of this port (see doc.go), so they are the regression net for
// the tie-breaking rule.
func TestIndelOpcodesReferenceCases(t *testing.T) {
	tests := []struct {
		name   string
		before []string
		after  []string
		want   []Opcode
	}{
		{
			// Tie-breaking case 1: a naive forward walk emits
			// equal/delete/insert here; the reference prefers the
			// insert first and only then deletes.
			name:   "prefer_insert_then_delete",
			before: []string{"a", "b"},
			after:  []string{"b", "a"},
			want: []Opcode{
				op(TagInsert, 0, 0, 0, 1),
				op(TagEqual, 0, 1, 1, 2),
				op(TagDelete, 1, 2, 2, 2),
			},
		},
		{
			// Tie-breaking case 2: the same local DP values as the case
			// above produce a different outcome, which is why no local
			// tie-break rule can reproduce the reference.
			name:   "delete_first_then_insert",
			before: []string{"b", "a"},
			after:  []string{"a", "a", "c"},
			want: []Opcode{
				op(TagDelete, 0, 1, 0, 0),
				op(TagEqual, 1, 2, 0, 1),
				op(TagInsert, 2, 2, 1, 3),
			},
		},
		{
			name:   "repeated_single_element",
			before: []string{"a"},
			after:  []string{"a", "a", "a"},
			want: []Opcode{
				op(TagEqual, 0, 1, 0, 1),
				op(TagInsert, 1, 1, 1, 3),
			},
		},
		{
			name:   "gap_deletion_is_one_run",
			before: []string{"a", "b", "c", "d"},
			after:  []string{"a", "d"},
			want: []Opcode{
				op(TagEqual, 0, 1, 0, 1),
				op(TagDelete, 1, 3, 1, 1),
				op(TagEqual, 3, 4, 1, 2),
			},
		},
		{
			name:   "empty_before",
			before: nil,
			after:  []string{"a", "b"},
			want:   []Opcode{op(TagInsert, 0, 0, 0, 2)},
		},
		{
			name:   "empty_after",
			before: []string{"a", "b"},
			after:  nil,
			want:   []Opcode{op(TagDelete, 0, 2, 0, 0)},
		},
		{
			name:   "both_empty",
			before: nil,
			after:  nil,
			want:   nil,
		},
		{
			name:   "identical",
			before: []string{"a", "b"},
			after:  []string{"a", "b"},
			want:   []Opcode{op(TagEqual, 0, 2, 0, 2)},
		},
		{
			// Reference value: with no common element the alignment emits the
			// insertion first, then the deletion — the reverse of the
			// delete-first shape a hand-written implementation tends to
			// produce.
			name:   "completely_different",
			before: []string{"a", "b"},
			after:  []string{"c", "d"},
			want: []Opcode{
				op(TagInsert, 0, 0, 0, 2),
				op(TagDelete, 0, 2, 2, 2),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IndelOpcodes(tc.before, tc.after)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("IndelOpcodes(%q, %q)\n got %v\nwant %v", tc.before, tc.after, got, tc.want)
			}
		})
	}
}

// TestIndelOpcodesInvariants checks the structural contract every opcode
// sequence must satisfy, independently of the reference: the tags cover both
// inputs exactly, in order, with no gaps and no overlap.
func TestIndelOpcodesInvariants(t *testing.T) {
	sequences := [][2][]string{
		{{"a", "b", "c"}, {"a", "x", "c"}},
		{{"a", "b"}, {"b", "a"}},
		{{"b", "a"}, {"a", "a", "c"}},
		{{"one", "two", "three"}, {"one", "2", "three", "four"}},
		{{"same", "same", "same"}, {"same"}},
		{{"x"}, {"y", "z", "w"}},
		{{"a", "b", "c", "d", "e"}, {"c", "d"}},
		{nil, nil},
		{{""}, {""}},
		{{"a", "", "b"}, {"", "b"}},
	}
	for _, seq := range sequences {
		before, after := seq[0], seq[1]
		ops := IndelOpcodes(before, after)

		srcPos, destPos := 0, 0
		for i, o := range ops {
			switch o.Tag {
			case TagEqual, TagDelete, TagInsert, TagReplace:
			default:
				t.Fatalf("%q->%q op %d: unknown tag %q", before, after, i, o.Tag)
			}
			if o.SrcStart != srcPos {
				t.Fatalf("%q->%q op %d: src gap at %d, want %d", before, after, i, o.SrcStart, srcPos)
			}
			if o.DestStart != destPos {
				t.Fatalf("%q->%q op %d: dest gap at %d, want %d", before, after, i, o.DestStart, destPos)
			}
			if o.SrcEnd < o.SrcStart || o.DestEnd < o.DestStart {
				t.Fatalf("%q->%q op %d: inverted range %v", before, after, i, o)
			}
			if o.Tag == TagEqual {
				if o.SrcEnd-o.SrcStart != o.DestEnd-o.DestStart {
					t.Fatalf("%q->%q op %d: unequal-length equal block %v", before, after, i, o)
				}
				for k := 0; k < o.SrcEnd-o.SrcStart; k++ {
					if before[o.SrcStart+k] != after[o.DestStart+k] {
						t.Fatalf("%q->%q op %d: equal block %v covers differing elements",
							before, after, i, o)
					}
				}
			}
			srcPos, destPos = o.SrcEnd, o.DestEnd
		}
		if srcPos != len(before) || destPos != len(after) {
			t.Fatalf("%q->%q: covered (%d,%d), want (%d,%d)", before, after, srcPos, destPos, len(before), len(after))
		}
	}
}

// TestIndelOpcodesDoesNotEmitReplace documents the divergence from
// difflib.SequenceMatcher, whose opcodes would use a "replace" tag here. The
// reference is rapidfuzz's Indel, which never does.
func TestIndelOpcodesDoesNotEmitReplace(t *testing.T) {
	for _, o := range IndelOpcodes([]string{"a", "b"}, []string{"c", "d"}) {
		if o.Tag == TagReplace {
			t.Fatalf("Indel must not emit a replace tag, got %v", o)
		}
	}
}

// TestIndelOpcodesDoesNotMutateInputs guards the bit-parallel implementation
// against aliasing the caller's slices.
func TestIndelOpcodesDoesNotMutateInputs(t *testing.T) {
	before := []string{"a", "b", "c"}
	after := []string{"c", "b", "a"}
	beforeCopy := append([]string(nil), before...)
	afterCopy := append([]string(nil), after...)
	IndelOpcodes(before, after)
	if !reflect.DeepEqual(before, beforeCopy) || !reflect.DeepEqual(after, afterCopy) {
		t.Fatalf("inputs mutated: before=%q after=%q", before, after)
	}
}

// TestBitvecHelpers covers the bit-parallel primitives directly, including the
// nil-receiver path that would otherwise panic when a symbol is absent from the
// left-hand sequence.
func TestBitvecHelpers(t *testing.T) {
	v := newBitvec(130)
	v.set(0)
	v.set(63)
	v.set(64)
	v.set(129)
	for _, bit := range []int{0, 63, 64, 129} {
		if !v.test(bit) {
			t.Fatalf("bit %d not set", bit)
		}
	}
	for _, bit := range []int{1, 62, 65, 128} {
		if v.test(bit) {
			t.Fatalf("bit %d unexpectedly set", bit)
		}
	}
	if got := v.popcountBelow(130); got != 4 {
		t.Fatalf("popcountBelow(130) = %d, want 4", got)
	}
	if got := v.popcountBelow(64); got != 2 {
		t.Fatalf("popcountBelow(64) = %d, want 2 (bits 0 and 63)", got)
	}
	if got := v.popcountBelow(0); got != 0 {
		t.Fatalf("popcountBelow(0) = %d, want 0", got)
	}

	// A nil vector must behave as all-zero rather than panicking: the caller
	// indexes a symbol map that may not contain the current element.
	var nilVec bitvec
	if got := andBitvec(v, nilVec); got.popcountBelow(130) != 0 {
		t.Fatal("andBitvec with a nil right operand must yield zero")
	}
	if got := andBitvec(v, nilVec); len(got) != len(v) {
		t.Fatalf("andBitvec with a nil right operand returned %d words, want %d: the "+
			"result feeds addBitvec/subBitvec, which index it word by word",
			len(got), len(v))
	}

	// orBitvec and the arithmetic helpers are only ever called with operands of
	// equal width, which is what the LCS update guarantees; they are not
	// general-purpose and are not required to widen.
	if got := orBitvec(newBitvec(130), v); got.popcountBelow(130) != 4 {
		t.Fatalf("orBitvec(zeros, v) popcount = %d, want 4", got.popcountBelow(130))
	}
}
