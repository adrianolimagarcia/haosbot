// Package pyjson formats values the way Python's json module does.
//
// Go's encoding/json and Python's json.dumps disagree on two things that
// matter for on-disk compatibility:
//
//   - Floats. Go writes the shortest representation that round-trips, so
//     float64(1) becomes "1" and 1e20 becomes "100000000000000000000". Python
//     writes "1.0" and "1e+20". A value written by one runtime and read by the
//     other silently changes type: Python's json.loads("1") is an int, not a
//     float.
//   - Strings. Go's encoder escapes <, > and & by default, and escapes
//     U+2028/U+2029 even with SetEscapeHTML(false). Python leaves all of them
//     literal.
//
// Both the session store and the configuration writer need the same rules, so
// they live here rather than being reimplemented per package. A previous
// duplication of a shared constant list drifted; the formatter is not a thing
// worth risking that on.
package pyjson

import (
	"bytes"
	"math"
	"strconv"
	"strings"
)

// FormatFloat renders f the way repr()/json.dumps does.
//
// The threshold is the part that is easy to get wrong: Python uses scientific
// notation when the decimal exponent is below -4 or at least 16, while Go's
// 'g' verb switches at a different point. The choice is therefore made
// explicitly rather than delegated to a format verb.
//
// Non-finite values are emitted as the bare tokens Python emits:
// json.dumps(float('inf')) == "Infinity", and the same for NaN.
func FormatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}

	sci := strconv.FormatFloat(f, 'e', -1, 64)
	exp := 0
	if i := strings.IndexByte(sci, 'e'); i >= 0 {
		exp, _ = strconv.Atoi(sci[i+1:])
	}
	if exp < -4 || exp >= 16 {
		return sci
	}
	plain := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(plain, ".") {
		plain += ".0"
	}
	return plain
}

// Float is a float64 that marshals the way Python's json.dumps writes floats.
//
// Using this instead of float64 keeps the int/float distinction visible on the
// wire: an untyped constant such as 1.0 still assigns directly, so call sites
// read the same.
type Float float64

// MarshalJSON implements json.Marshaler.
func (f Float) MarshalJSON() ([]byte, error) {
	return []byte(FormatFloat(float64(f))), nil
}

// Float64 returns the underlying value.
func (f Float) Float64() float64 { return float64(f) }

// UnescapeLineSeparators rewrites the \u2028 and \u2029 escapes that Go's
// encoding/json always emits into the literal characters.
//
// Go escapes these two unconditionally, for JSONP safety, and
// SetEscapeHTML(false) does not disable it. Python's json.dumps with
// ensure_ascii=False leaves them literal, so the bytes differ even though both
// documents parse to the same string.
//
// A naive bytes.Replace would corrupt input that genuinely contains the six
// characters \u2028: Go escapes the backslash, producing \\u2028, and the
// escape sequence still appears as a substring. The scan therefore counts the
// run of backslashes before the escape and skips it when that run is odd,
// because an odd run means the backslash is itself escaped and the following
// text is literal.
//
// Invalid UTF-8 is untouched: encoding/json has already replaced it with
// U+FFFD by this point.
func UnescapeLineSeparators(b []byte) []byte {
	if !bytes.Contains(b, []byte(`\u202`)) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if b[i] == '\\' && i+6 <= len(b) &&
			(string(b[i+1:i+6]) == "u2028" || string(b[i+1:i+6]) == "u2029") {
			// Count the backslashes immediately before position i.
			run := 0
			for j := i - 1; j >= 0 && b[j] == '\\'; j-- {
				run++
			}
			if run%2 == 0 {
				if b[i+5] == '8' {
					out = append(out, 0xE2, 0x80, 0xA8) // U+2028
				} else {
					out = append(out, 0xE2, 0x80, 0xA9) // U+2029
				}
				i += 6
				continue
			}
		}
		out = append(out, b[i])
		i++
	}
	return out
}
