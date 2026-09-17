package bpe

import (
	"strings"
	"unicode/utf8"
)

// Python-compatible UTF-8 decoding.
//
// Go's utf8.DecodeRuneInString is NOT equivalent to Python's
// `bytes.decode("utf-8", errors=...)`, and the difference is reachable:
// truncate_text_to_tokens decodes a PREFIX of a token sequence, which routinely
// cuts a multi-byte code point in half. Measured against CPython 3.14.7 with
// errors="replace":
//
//	input               Python    Go (one RuneError per bad byte)
//	------------------  --------  ------------------------------
//	b"\xe4\xb8"         1 x FFFD  2 x FFFD
//	b"\xf0\x9f\x98"     1 x FFFD  3 x FFFD
//	b"\xc3"             1 x FFFD  1 x FFFD
//	b"\xed\xa0\x80"     3 x FFFD  3 x FFFD
//	b"\xf4\x90\x80\x80" 4 x FFFD  4 x FFFD
//	b"\x80\x80"         2 x FFFD  2 x FFFD
//	b"\xc0\x80"         2 x FFFD  2 x FFFD
//	b"\xe4\xb8\x41"     FFFD 'A'  FFFD 'A'
//
// Python follows the WHATWG/Unicode "maximal subpart" rule: an ill-formed
// subsequence contributes ONE U+FFFD, except that a byte which cannot continue
// the sequence is pushed back and reconsidered as a potential lead byte. Go
// reports RuneError with size 1 for the lead byte and then re-examines every
// continuation byte, so a merely TRUNCATED sequence yields one replacement per
// byte.
//
// decodeUTF8 implements the WHATWG algorithm once; both the "replace" and the
// "ignore" flavour the reference needs are the same walk with a different
// error action, so they share it rather than risking a second, drifting copy.

// decodeUTF8Replace is Python's `bytes.decode("utf-8", errors="replace")`.
func decodeUTF8Replace(b []byte) string { return decodeUTF8(b, true) }

// decodeUTF8Ignore is Python's `bytes.decode("utf-8", errors="ignore")`.
func decodeUTF8Ignore(b []byte) string { return decodeUTF8(b, false) }

func decodeUTF8(b []byte, replacement bool) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	sb.Grow(len(b) + 4)
	i := 0
	for i < len(b) {
		c := b[i]
		if c < 0x80 {
			sb.WriteByte(c)
			i++
			continue
		}
		need, lo, hi, ok := utf8Lead(c)
		if !ok {
			// A continuation byte with no lead, an overlong lead (0xC0/0xC1),
			// or a lead above U+10FFFF (0xF5..0xFF): one byte, one error.
			if replacement {
				sb.WriteRune(utf8.RuneError)
			}
			i++
			continue
		}
		j := i + 1
		good := true
		for k := 0; k < need; k++ {
			if j >= len(b) {
				// Truncated at end of input: the whole partial sequence is a
				// single ill-formed subsequence.
				good = false
				j = len(b)
				break
			}
			cb := b[j]
			if cb < lo || cb > hi {
				// Push cb back: do not consume it, so it is reconsidered as a
				// potential lead byte on the next iteration.
				good = false
				break
			}
			j++
			lo, hi = 0x80, 0xBF
		}
		if good {
			sb.Write(b[i:j])
			i = j
			continue
		}
		if replacement {
			sb.WriteRune(utf8.RuneError)
		}
		i = j
	}
	return sb.String()
}

// utf8Lead classifies a UTF-8 lead byte.
//
// need is the number of continuation bytes that must follow, and lo..hi is the
// range the first one must fall in — narrower for the three leads that would
// otherwise admit overlong encodings (0xE0, 0xF0) or surrogates (0xED) or
// values past U+10FFFF (0xF4).
func utf8Lead(c byte) (need int, lo, hi byte, ok bool) {
	switch {
	case c >= 0xC2 && c <= 0xDF:
		return 1, 0x80, 0xBF, true
	case c >= 0xE0 && c <= 0xEF:
		lo, hi = 0x80, 0xBF
		if c == 0xE0 {
			lo = 0xA0
		} else if c == 0xED {
			hi = 0x9F
		}
		return 2, lo, hi, true
	case c >= 0xF0 && c <= 0xF4:
		lo, hi = 0x80, 0xBF
		if c == 0xF0 {
			lo = 0x90
		} else if c == 0xF4 {
			hi = 0x8F
		}
		return 3, lo, hi, true
	}
	return 0, 0, 0, false
}
