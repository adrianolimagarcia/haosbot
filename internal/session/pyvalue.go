package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/pyjson"
)

// Python value semantics for values that arrived as decoded JSON.
//
// The session transcript is a list of JSON objects, so every value this package
// inspects is a JSON value. Python and Go disagree about several operations on
// those values — truthiness, str(), repr() — and the reference's replay path
// depends on all three, so they are implemented here rather than approximated
// with Go idioms.

// pyTruthy reports Python truthiness of a raw JSON value.
//
// False for None, False, 0, 0.0, "", [] and {}; true for everything else.
func pyTruthy(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	switch trimmed[0] {
	case 'n': // null
		return false
	case 't':
		return true
	case 'f':
		return false
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return true
		}
		return s != ""
	case '[':
		items, err := arrayItems(trimmed)
		if err != nil {
			return true
		}
		return len(items) > 0
	case '{':
		fields, err := objectFields(trimmed)
		if err != nil {
			return true
		}
		return len(fields) > 0
	default:
		// A number: falsy only when it is exactly zero.
		f, err := jsonNumberFloat(trimmed)
		if err != nil {
			return true
		}
		return f != 0
	}
}

// jsonNumberFloat parses a raw JSON number as a float64.
func jsonNumberFloat(raw json.RawMessage) (float64, error) {
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, err
	}
	return n.Float64()
}

// pyStr renders a raw JSON value the way Python's str() does.
//
// str() of a container is repr(), so the container branches below reproduce
// CPython's repr formatting (single quotes by default, ", " between items and
// ": " between a key and its value).
//
// KNOWN APPROXIMATION: the escaping of non-printable characters inside strings
// uses Go's unicode.IsPrint for code points outside the ASCII range, while
// CPython uses str.isprintable(), whose category set (Cc, Cf, Cs, Co, Cn, Zl,
// Zp, Zs minus U+0020) is not identical. The two agree on all of ASCII and on
// the overwhelming majority of the BMP; the differential corpus records which
// cases were exercised.
func pyStr(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "None"
	}
	switch trimmed[0] {
	case 'n':
		return "None"
	case 't':
		return "True"
	case 'f':
		return "False"
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return string(trimmed)
		}
		return s
	case '[':
		items, err := arrayItems(trimmed)
		if err != nil {
			return string(trimmed)
		}
		parts := make([]string, 0, len(items))
		for _, item := range items {
			parts = append(parts, pyRepr(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case '{':
		fields, err := objectFields(trimmed)
		if err != nil {
			return string(trimmed)
		}
		parts := make([]string, 0, len(fields))
		for _, f := range fields {
			parts = append(parts, pyReprString(f.key)+": "+pyRepr(f.val))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return pyNumberString(trimmed)
	}
}

// pyRepr renders a raw JSON value the way Python's repr() does.
func pyRepr(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return pyReprString(s)
		}
	}
	return pyStr(trimmed)
}

// pyNumberString renders a JSON number the way str() does for the Python value
// json.loads produces from the same literal: an int when the literal has no
// fraction or exponent, a float otherwise (so "1.0" stays "1.0" and "1e2"
// becomes "100.0").
func pyNumberString(raw json.RawMessage) string {
	literal := string(raw)
	if strings.ContainsAny(literal, ".eE") {
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return literal
		}
		f, err := n.Float64()
		if err != nil {
			return literal
		}
		return pyjson.FormatFloat(f)
	}
	return literal
}

// pyReprString renders a Go string the way CPython's repr() renders a str.
//
// Mirrors unicode_repr (Objects/unicodeobject.c): the quote character is '
// unless the string contains ' and no ", in which case it is "; the quote
// itself and the backslash are backslash-escaped; \t, \n and \r use their short
// escapes; other characters below U+0020 and U+007F use \xHH; and non-ASCII
// characters that are not printable use \xHH, \uHHHH or \UHHHHHHHH.
func pyReprString(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}

	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x7f:
			b.WriteRune(r)
		case !pyIsPrintable(r):
			switch {
			case r <= 0xff:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r <= 0xffff:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// pyIsPrintable approximates CPython's str.isprintable() for a single code
// point. See the note on pyStr for the known difference from Go's
// unicode.IsPrint.
func pyIsPrintable(r rune) bool {
	if r == ' ' {
		return true
	}
	if r < utf8.RuneSelf {
		return r >= 0x20 && r != 0x7f
	}
	return unicode.IsPrint(r)
}
