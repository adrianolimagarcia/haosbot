package bpe

import (
	"unicode"
	"unicode/utf8"
)

// The cl100k_base pre-tokenizer.
//
// tiktoken compiles this pattern in its Rust core, where `_pat_str` for
// cl100k_base is (117 bytes, verified against enc._pat_str):
//
//	'(?i:[sdmt]|ll|ve|re)|[^\r\n\p{L}\p{N}]?+\p{L}++|\p{N}{1,3}+| ?[^\s\p{L}\p{N}]++[\r\n]*+|\s++$|\s*[\r\n]|\s+(?!\S)|\s
//
// NOTE the leading apostrophe: the first alternative is the two-or-three
// character group `'(s|d|m|t|ll|ve|re)`, not a bare `[sdmt]`. Dropping it still
// produces plausible output for most text and changes "don't" from
// ['don', "'t"] to ['d', 'on', "'t"], i.e. 2 tokens instead of 3. It is easy to
// lose when the pattern is copied out of a Python repr, so it is called out
// here and pinned by TestPretokenizerPatternShape.
//
// Go's regexp is RE2 and supports none of the possessive quantifiers, the
// negative lookahead, or `\p{L}`/`\p{N}` with the reference's Unicode tables,
// so the alternation is implemented as an explicit scanner. The semantics below
// were established by driving the real encoder, not by reading a spec:
//
//	enc.encode_ordinary("don't")            -> [15357, 956]     ['don', "'t"]
//	enc.encode_ordinary("a   ")             -> [64, 262]        ['a', '   ']
//	enc.encode_ordinary("a   b")            -> [64, 256, 293]   ['a', '  ', ' b']
//	enc.encode_ordinary("  \n")             -> [2355]           ['  \n']
//	enc.encode_ordinary("  hello")          -> [220, 24748]     [' ', ' hello']
//	enc.encode_ordinary("12345")            -> [4513, 1774]     ['123', '45']
//	enc.encode_ordinary("x\u00a0y")         -> [87, 4194, 88]   ['x', '\xa0y']
//	enc.encode_ordinary("\u017fs")          -> [129, 123, 82]   ['\u017f', 's']
//
// Every code point is covered by exactly one branch, so the pieces tile the
// input and no byte is ever dropped; TestScanPiecesTileInput asserts that.

// Piece is a half-open byte range of the input string.
type Piece struct {
	Start int
	End   int
}

// Pattern is tiktoken's cl100k_base `_pat_str`, verbatim.
//
// It is exported so the differential harness can assert that the port's
// hand-written scanner is reproducing the pattern the reference actually
// compiles, rather than one that merely looks similar. Note the leading
// apostrophe, which is part of the pattern: the first alternative is
// `'(s|d|m|t|ll|ve|re)`.
const Pattern = "'(?i:[sdmt]|ll|ve|re)|[^\\r\\n\\p{L}\\p{N}]?+\\p{L}++|\\p{N}{1,3}+" +
	"| ?[^\\s\\p{L}\\p{N}]++[\\r\\n]*+|\\s++$|\\s*[\\r\\n]|\\s+(?!\\S)|\\s"

// isSpace is `\s` in the reference's regex engine.
//
// Enumerated from tiktoken by gen/generate_vocab.py it is exactly:
//
//	U+0009..U+000D  U+0020  U+0085  U+00A0  U+1680  U+2000..U+200A
//	U+2028..U+2029  U+202F  U+205F  U+3000
//
// 25 code points, i.e. exactly Unicode's White_Space property, i.e. exactly
// Go's unicode.IsSpace. That is NOT Python's str.isspace() (which adds
// U+001C..U+001F and is what textutil.PyIsSpace implements) and it is not a
// coincidence: the generator asserts the equality on every regeneration.
func isSpace(r rune) bool { return unicode.IsSpace(r) }

// isLetter is `\p{L}`, and isNumber is `\p{N}`, in the reference's tables.
//
// These cannot be unicode.IsLetter / unicode.IsNumber: Go 1.23.5 ships Unicode
// 15.0.0 and the Rust regex crate inside tiktoken 0.14.0 ships a newer one, so
// Go's tables are a strict subset (measured: 136_104 vs 141_028 for \p{L} and
// 1_831 vs 1_911 for \p{N}, with zero code points in the other direction).
func isLetter(r rune) bool { return inRanges(letterRanges[:], r) }

func isNumber(r rune) bool { return inRanges(numberRanges[:], r) }

// Classify reports how the pre-tokenizer classifies r: as `\s`, `\p{L}` and
// `\p{N}` respectively.
//
// The encoder is the only production caller. This is exported so the
// differential harness can attribute a token-level divergence to a specific
// code point instead of only reporting a mismatched token sequence, and so a
// future Unicode bump can be diagnosed without reading the generated tables.
func Classify(r rune) (space, letter, number bool) {
	return isSpace(r), isLetter(r), isNumber(r)
}

type runeRange = struct{ lo, hi rune }

func inRanges(ranges []runeRange, r rune) bool {
	lo, hi := 0, len(ranges)-1
	for lo <= hi {
		mid := int(uint(lo+hi) >> 1)
		switch rng := ranges[mid]; {
		case r < rng.lo:
			hi = mid - 1
		case r > rng.hi:
			lo = mid + 1
		default:
			return true
		}
	}
	return false
}

// decodeRune reads one code point at s[i:].
//
// It behaves like utf8.DecodeRuneInString for valid UTF-8 and for most invalid
// input, with one addition: a CESU-8 encoded surrogate (U+D800..U+DFFF written
// as ED A0..BF 80..BF) is reported as a single U+FFFD of size 3. Python cannot
// hold that byte sequence, but Go's `"\uD800"` produces it, and tiktoken's
// Python layer rewrites a lone surrogate to exactly one U+FFFD before the Rust
// core sees it — so treating the three bytes as one replacement character is
// the closest available match.
//
// Any other malformed byte is reported as U+FFFD with size 1, matching
// utf8.DecodeRuneInString.
func decodeRune(s string, i int) (rune, int) {
	c := s[i]
	if c < utf8.RuneSelf {
		return rune(c), 1
	}
	if c == 0xED && i+2 < len(s) {
		b1, b2 := s[i+1], s[i+2]
		if b1 >= 0xA0 && b1 <= 0xBF && b2 >= 0x80 && b2 <= 0xBF {
			return utf8.RuneError, 3
		}
	}
	return utf8.DecodeRuneInString(s[i:])
}

// ScanPieces splits text into the pieces the reference's pre-tokenizer yields.
//
// The returned ranges are half-open byte offsets into text, in order, and they
// tile text exactly. The callback form is deliberately not used: the encoder
// needs the byte slices and allocating a slice of them once is cheaper than a
// closure call per piece.
func ScanPieces(text string) []Piece {
	out := make([]Piece, 0, len(text)/4+1)
	for i := 0; i < len(text); {
		n := matchPiece(text, i)
		out = append(out, Piece{Start: i, End: i + n})
		i += n
	}
	return out
}

// matchPiece returns the byte length of the piece starting at i.
//
// The branches are tried in the reference's alternation order and the first
// match wins. matchPiece never returns 0: the eight branches between them cover
// every code point.
func matchPiece(s string, i int) int {
	// Branch 1: '(?i:[sdmt]|ll|ve|re)
	//
	// The `(?i:)` group is Unicode simple case folding, whose classes were
	// enumerated from the reference (gen/generate_vocab.py prints them):
	//
	//	[sdmt] -> {D d M m S s T t U+017F}   <- U+017F LONG S folds to 's'
	//	ll     -> {L l} x {L l}
	//	ve     -> {V v} x {E e}
	//	re     -> {R r} x {E e}
	//
	// U+017F is the only non-ASCII member and it is load-bearing: without it
	// "\u017fs" is one piece instead of two.
	if s[i] == '\'' {
		if i+1 < len(s) {
			r, size := decodeRune(s, i+1)
			switch r {
			case 'd', 'D', 'm', 'M', 's', 'S', 't', 'T', 0x017F:
				return 1 + size
			case 'l', 'L':
				if i+1+size < len(s) {
					if r2, s2 := decodeRune(s, i+1+size); r2 == 'l' || r2 == 'L' {
						return 1 + size + s2
					}
				}
			case 'v', 'V', 'r', 'R':
				if i+1+size < len(s) {
					if r2, s2 := decodeRune(s, i+1+size); r2 == 'e' || r2 == 'E' {
						return 1 + size + s2
					}
				}
			}
		}
	}

	// Branch 2: [^\r\n\p{L}\p{N}]?+\p{L}++
	//
	// The optional prefix is possessive, so it is consumed whenever it can be
	// and never given back. `\p{L}++` then requires at least one letter, which
	// is what makes " b" a single piece while "  b" is not.
	{
		j := i
		if r, size := decodeRune(s, j); r != '\r' && r != '\n' && !isLetter(r) && !isNumber(r) {
			j += size
		}
		if j < len(s) {
			if r, size := decodeRune(s, j); isLetter(r) {
				j += size
				for j < len(s) {
					r2, size2 := decodeRune(s, j)
					if !isLetter(r2) {
						break
					}
					j += size2
				}
				return j - i
			}
		}
	}

	// Branch 3: \p{N}{1,3}+
	//
	// Possessive, so a run of digits is taken three at a time and the remainder
	// is a shorter piece: "12345" is ['123', '45'].
	if r, _ := decodeRune(s, i); isNumber(r) {
		j := i
		for k := 0; k < 3 && j < len(s); k++ {
			r2, size2 := decodeRune(s, j)
			if !isNumber(r2) {
				break
			}
			j += size2
		}
		return j - i
	}

	// Branch 4:  ?[^\s\p{L}\p{N}]++[\r\n]*+
	//
	// ` ?` is greedy but backtrackable while `++` is possessive, so the space
	// is taken first and only released when nothing non-space follows it. When
	// the released form cannot match either, the branch fails — which is why
	// "a   b" gives ['a', '  ', ' b'] and not ['a', '   ', 'b'].
	if n := matchBranch4(s, i); n > 0 {
		return n
	}

	// Branch 5: \s++$
	//
	// `$` is end-of-haystack in the Rust engine. The possessive `++` cannot
	// give back, so this matches only a whitespace run that reaches the end.
	if j := scanSpace(s, i); j == len(s) && j > i {
		return j - i
	}

	// Branch 6: \s*[\r\n]
	//
	// `\s*` is greedy and backtrackable, so the match is the whitespace run up
	// to and including its LAST CR or LF.
	{
		j := i
		lastNewline := -1
		for j < len(s) {
			r, size := decodeRune(s, j)
			if !isSpace(r) {
				break
			}
			if r == '\r' || r == '\n' {
				lastNewline = j
			}
			j += size
		}
		if lastNewline >= 0 {
			return lastNewline + 1 - i
		}
	}

	// Branch 7: \s+(?!\S)
	//
	// `\s+` is greedy and backtracks one code point at a time until the next
	// code point is not `\S`, i.e. is whitespace or the end of the input. That
	// leaves the final whitespace code point to the next piece. "a   b" starts
	// here with a three-space run, gives one back, and yields "  ".
	{
		j := i
		lastRuneStart := -1
		for j < len(s) {
			r, size := decodeRune(s, j)
			if !isSpace(r) {
				break
			}
			lastRuneStart = j
			j += size
		}
		if j > i {
			if j == len(s) {
				return j - i
			}
			if lastRuneStart > i {
				return lastRuneStart - i
			}
		}
	}

	// Branch 8: \s
	//
	// Reached only for a single whitespace code point that is not at the end of
	// the input. Every other code point is consumed by an earlier branch, so
	// this cannot fail; the size is returned unconditionally to guarantee the
	// scan always advances.
	_, size := decodeRune(s, i)
	return size
}

// matchBranch4 implements ` ?[^\s\p{L}\p{N}]++[\r\n]*+`, returning 0 on no
// match.
func matchBranch4(s string, i int) int {
	j := i
	if s[j] == ' ' {
		j++
	}
	if k := scanNonSpaceLetterNumber(s, j); k > j {
		return scanCRLF(s, k) - i
	}
	// ` ?` releases its space and the branch is retried from i. This can only
	// help when the character at i is not a space, because a space is itself
	// excluded from [^\s\p{L}\p{N}].
	if j != i {
		if k := scanNonSpaceLetterNumber(s, i); k > i {
			return scanCRLF(s, k) - i
		}
	}
	return 0
}

// scanNonSpaceLetterNumber consumes the run of code points that are neither
// whitespace, nor `\p{L}`, nor `\p{N}`.
func scanNonSpaceLetterNumber(s string, i int) int {
	for i < len(s) {
		r, size := decodeRune(s, i)
		if isSpace(r) || isLetter(r) || isNumber(r) {
			break
		}
		i += size
	}
	return i
}

// scanCRLF consumes the run of CR and LF code points.
func scanCRLF(s string, i int) int {
	for i < len(s) && (s[i] == '\r' || s[i] == '\n') {
		i++
	}
	return i
}

// scanSpace consumes the run of whitespace code points starting at i.
func scanSpace(s string, i int) int {
	for i < len(s) {
		r, size := decodeRune(s, i)
		if !isSpace(r) {
			break
		}
		i += size
	}
	return i
}
