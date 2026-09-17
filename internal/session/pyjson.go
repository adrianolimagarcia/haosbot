package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/adrianolimagarcia/nanobot-go/internal/pyjson"
	"reflect"
	"sort"
	"strconv"
	"unicode/utf8"
)

// This file re-implements CPython's json.dumps(value, ensure_ascii=False) with
// its default separators, byte for byte, so that a Go-written session file is
// indistinguishable from a Python-written one.
//
// Differences from Go's encoding/json that matter here:
//
//   - encoding/json escapes '<', '>', '&' as \u003c, \u003e, \u0026 unless
//     SetEscapeHTML(false) is used, and it escapes U+2028/U+2029 even then.
//     CPython escapes neither (verified on 3.14.7).
//   - encoding/json emits ", " only for struct/map fields it chooses; Python
//     uses ", " and ": " everywhere.
//   - encoding/json renders float64(1) as "1" and float64(1e20) as
//     "100000000000000000000"; CPython writes "1.0" and "1e+20".
//
// Verified against CPython 3.14.7: writePyString(string(rune(cp))) equals
// json.dumps(chr(cp), ensure_ascii=False) for all 1,112,064 code points that
// are not UTF-16 surrogates (U+D800..U+DFFF, which a Go string cannot carry
// and Python cannot encode to UTF-8).

const (
	pyItemSep = ", " // CPython's default json.dumps item separator
	pyKeySep  = ": " // CPython's default json.dumps key separator
)

// writePyString writes s as a JSON string using CPython's escaping rules with
// ensure_ascii=False: only '"', '\\' and C0 control characters are escaped.
//
// Invalid UTF-8 cannot be represented in a Python str, so a Go string that
// carries it is written as U+FFFD (the same substitution encoding/json makes).
// Writing the raw bytes instead would make the whole file unreadable for
// Python, which opens session files with encoding="utf-8".
func writePyString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			buf.WriteString("\uFFFD")
			i++
			continue
		}
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, r)
			} else {
				buf.WriteString(s[i : i+size])
			}
		}
		i += size
	}
	buf.WriteByte('"')
}

// writePyFloat writes f exactly as CPython's repr(float) does, which is what
// json.dumps uses for floats.
//
// CPython switches to exponential notation when the decimal exponent is below
// -4 or at least 16; Go's 'g' verb switches at a different threshold, so the
// choice is made here explicitly. Infinity and NaN are written as the bare
// tokens Python emits (json.dumps(float('inf')) == "Infinity").
func writePyFloat(buf *bytes.Buffer, f float64) {
	// The formatting rules live in internal/pyjson so the config writer and
	// the session writer cannot drift apart.
	buf.WriteString(pyjson.FormatFloat(f))
}

// writePyNumber writes a json.Number verbatim, which is what preserves the
// int/float distinction across a round trip: Python's json.loads("1.0") is a
// float and json.loads("1") is an int, and Go's float64 marshaller would
// collapse both to "1".
func writePyNumber(buf *bytes.Buffer, n json.Number) error {
	s := n.String()
	if !json.Valid([]byte(s)) {
		return fmt.Errorf("session: %q is not a valid JSON number", s)
	}
	buf.WriteString(s)
	return nil
}

// appendPyRaw re-encodes an already-decoded JSON value using Python's escaping
// and separator rules. Key order is preserved; numbers are passed through.
func appendPyRaw(buf *bytes.Buffer, raw json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := appendPyToken(buf, dec); err != nil {
		return err
	}
	// Reject trailing garbage so a malformed raw value cannot silently
	// produce a malformed line.
	if dec.More() {
		return fmt.Errorf("session: trailing data after JSON value")
	}
	return nil
}

// appendPyToken re-encodes the next JSON value from dec.
func appendPyToken(buf *bytes.Buffer, dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	return appendPyTokenValue(buf, dec, tok)
}

func appendPyTokenValue(buf *bytes.Buffer, dec *json.Decoder, tok json.Token) error {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			buf.WriteByte('{')
			first := true
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := keyTok.(string)
				if !ok {
					return fmt.Errorf("session: object key is not a string")
				}
				if !first {
					buf.WriteString(pyItemSep)
				}
				first = false
				writePyString(buf, key)
				buf.WriteString(pyKeySep)
				if err := appendPyToken(buf, dec); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // consume '}'
				return err
			}
			buf.WriteByte('}')
			return nil
		case '[':
			buf.WriteByte('[')
			first := true
			for dec.More() {
				if !first {
					buf.WriteString(pyItemSep)
				}
				first = false
				if err := appendPyToken(buf, dec); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // consume ']'
				return err
			}
			buf.WriteByte(']')
			return nil
		default:
			return fmt.Errorf("session: unexpected JSON delimiter %q", t)
		}
	case string:
		writePyString(buf, t)
		return nil
	case json.Number:
		return writePyNumber(buf, t)
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil
	case nil:
		buf.WriteString("null")
		return nil
	default:
		return fmt.Errorf("session: unsupported JSON token %T", tok)
	}
}

// appendPyValue encodes a Go value the way CPython would.
//
// json.Number and json.RawMessage are passed through, which is what keeps
// Python-written numbers (and any field this package does not model) intact.
// Anything else falls back to encoding/json and is then re-encoded, so structs
// and typed slices still come out in Python's syntax.
func appendPyValue(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
		return nil
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil
	case string:
		writePyString(buf, t)
		return nil
	case json.Number:
		return writePyNumber(buf, t)
	case json.RawMessage:
		if len(t) == 0 {
			buf.WriteString("null")
			return nil
		}
		return appendPyRaw(buf, t)
	case float64:
		writePyFloat(buf, t)
		return nil
	case float32:
		writePyFloat(buf, float64(t))
		return nil
	case int:
		buf.WriteString(strconv.Itoa(t))
		return nil
	case int8:
		buf.WriteString(strconv.FormatInt(int64(t), 10))
		return nil
	case int16:
		buf.WriteString(strconv.FormatInt(int64(t), 10))
		return nil
	case int32:
		buf.WriteString(strconv.FormatInt(int64(t), 10))
		return nil
	case int64:
		buf.WriteString(strconv.FormatInt(t, 10))
		return nil
	case uint:
		buf.WriteString(strconv.FormatUint(uint64(t), 10))
		return nil
	case uint8:
		buf.WriteString(strconv.FormatUint(uint64(t), 10))
		return nil
	case uint16:
		buf.WriteString(strconv.FormatUint(uint64(t), 10))
		return nil
	case uint32:
		buf.WriteString(strconv.FormatUint(uint64(t), 10))
		return nil
	case uint64:
		buf.WriteString(strconv.FormatUint(t, 10))
		return nil
	case map[string]any:
		return appendPyMap(buf, t, nil)
	case []any:
		return appendPyArray(buf, t)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("session: encode %T: %w", v, err)
	}
	return appendPyRaw(buf, raw)
}

// appendPyArray writes a JSON array with Python's item separator.
func appendPyArray(buf *bytes.Buffer, items []any) error {
	buf.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			buf.WriteString(pyItemSep)
		}
		if err := appendPyValue(buf, item); err != nil {
			return err
		}
	}
	buf.WriteByte(']')
	return nil
}

// appendPyMap writes a JSON object with Python's separators.
//
// Go maps have no order, so keys listed in order are written first (this is
// the order they had in the file they were loaded from) and any remaining key
// follows in sorted order. Python preserves insertion order, so keeping the
// loaded order means a load/save cycle does not reshuffle a metadata object.
func appendPyMap(buf *bytes.Buffer, m map[string]any, order []string) error {
	keys := pyMapKeys(m, order)

	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteString(pyItemSep)
		}
		writePyString(buf, k)
		buf.WriteString(pyKeySep)
		if err := appendPyValue(buf, m[k]); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

// appendMetaMap writes the session metadata object.
//
// It differs from appendPyMap in one way: a value whose bytes were captured on
// load (or written by this package) and that has not been modified since is
// re-emitted verbatim. Python's metadata is a plain dict of arbitrary values
// and json.dumps preserves each nested dict's insertion order, which a
// map[string]any round trip would lose.
func appendMetaMap(
	buf *bytes.Buffer,
	m map[string]any,
	order []string,
	raw map[string]json.RawMessage,
	loaded map[string]any,
) error {
	keys := pyMapKeys(m, order)

	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteString(pyItemSep)
		}
		writePyString(buf, k)
		buf.WriteString(pyKeySep)
		if r, ok := raw[k]; ok && len(r) > 0 && reflect.DeepEqual(m[k], loaded[k]) {
			if err := appendPyRaw(buf, r); err != nil {
				return err
			}
			continue
		}
		if err := appendPyValue(buf, m[k]); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

// pyMapKeys returns m's keys: the recorded insertion order first, then any key
// with no recorded slot, sorted.
func pyMapKeys(m map[string]any, order []string) []string {
	keys := make([]string, 0, len(m))
	seen := make(map[string]bool, len(m))
	for _, k := range order {
		if _, ok := m[k]; ok && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	rest := make([]string, 0, len(m))
	for k := range m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}
