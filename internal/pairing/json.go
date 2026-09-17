package pairing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Ordered JSON model.
//
// Python's json module round-trips through dict, which preserves key order, so
// json.load followed by json.dumps reproduces the file's key order. Go's
// encoding/json round-trips through map, which does not. Because pairing.json
// is shared with the Python runtime, this package keeps the order: objects are
// decoded into jsonObject and re-encoded in the order the keys were read.
//
// Values are exactly the JSON shapes Python produces: nil (null), bool, string,
// json.Number, []any (array) and *jsonObject (object). json.Number is retained
// rather than float64 so that an integer literal is not silently promoted to a
// float — Python distinguishes 5 (int) from 5.0 (float) and re-encodes them
// differently.

// jsonObject is a JSON object whose key order is preserved.
type jsonObject struct {
	keys []string
	vals map[string]any
}

func newJSONObject() *jsonObject {
	return &jsonObject{vals: map[string]any{}}
}

// get returns the value for key, or nil when absent. A nil receiver reads as an
// empty object, so callers do not need a nil check.
func (o *jsonObject) get(key string) any {
	if o == nil {
		return nil
	}
	return o.vals[key]
}

func (o *jsonObject) len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// set stores key. An existing key keeps its original position, matching dict
// assignment in Python.
func (o *jsonObject) set(key string, value any) {
	if o.vals == nil {
		o.vals = map[string]any{}
	}
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = value
}

// del removes key, preserving the relative order of the remaining keys.
func (o *jsonObject) del(key string) {
	if o == nil {
		return
	}
	if _, ok := o.vals[key]; !ok {
		return
	}
	delete(o.vals, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// decodeJSONDocument parses exactly one JSON document, preserving object key
// order.
//
// Trailing content is an error: Python's json.load reads the whole file and
// raises on trailing data, while json.Decoder would silently stop after the
// first value.
//
// DIVERGENCE (documented, not silent): a file that is not valid UTF-8 makes
// Python raise UnicodeDecodeError, which is NOT a json.JSONDecodeError and is
// therefore not caught by _load's corruption handler — it propagates out of the
// store. Go strings carry arbitrary bytes, the decoder fails, and this package
// treats the file as corrupt (the JSONDecodeError path). The reachable behavior
// for every well-formed file is identical.
func decodeJSONDocument(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	value, err := decodeJSONValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("pairing: trailing data after JSON document")
		}
		return nil, err
	}
	return value, nil
}

func decodeJSONValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return tok, nil // nil, bool, string or json.Number
	}
	switch delim {
	case '{':
		obj := newJSONObject()
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyTok.(string)
			if !ok {
				return nil, fmt.Errorf("pairing: object key is not a string")
			}
			value, err := decodeJSONValue(dec)
			if err != nil {
				return nil, err
			}
			obj.set(key, value)
		}
		if _, err := dec.Token(); err != nil { // consume '}'
			return nil, err
		}
		return obj, nil
	case '[':
		arr := []any{}
		for dec.More() {
			value, err := decodeJSONValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, value)
		}
		if _, err := dec.Token(); err != nil { // consume ']'
			return nil, err
		}
		return arr, nil
	}
	return nil, fmt.Errorf("pairing: unexpected delimiter %q", delim)
}

// encodeJSONDocument renders value exactly as json.dumps(value, indent=2,
// ensure_ascii=False) would.
func encodeJSONDocument(value any) string {
	var b strings.Builder
	writeJSONValue(&b, value, 0)
	return b.String()
}

func writeJSONValue(b *strings.Builder, value any, indent int) {
	switch v := value.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if v {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		b.WriteString(pyJSONString(v))
	case json.Number:
		b.WriteString(pyJSONNumber(v))
	case float64:
		// Not produced by decodeJSONDocument, but reachable if a caller hands a
		// Go value to the writer; rendered with Python's float rules so it
		// cannot silently become an integer in the file.
		b.WriteString(pyFloatRepr(v))
	case int:
		b.WriteString(strconv.Itoa(v))
	case int64:
		b.WriteString(strconv.FormatInt(v, 10))
	case []any:
		if len(v) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, item := range v {
			writeJSONIndent(b, indent+1)
			writeJSONValue(b, item, indent+1)
			if i < len(v)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		writeJSONIndent(b, indent)
		b.WriteByte(']')
	case *jsonObject:
		if v.len() == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, key := range v.keys {
			writeJSONIndent(b, indent+1)
			b.WriteString(pyJSONString(key))
			b.WriteString(": ")
			writeJSONValue(b, v.vals[key], indent+1)
			if i < len(v.keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		writeJSONIndent(b, indent)
		b.WriteByte('}')
	default:
		// Unreachable for values produced by decodeJSONDocument. Encoding
		// through encoding/json would change the layout, so the value is
		// reported as null rather than silently reformatted.
		b.WriteString("null")
	}
}

func writeJSONIndent(b *strings.Builder, indent int) {
	for i := 0; i < indent; i++ {
		b.WriteString("  ")
	}
}

// pyJSONString renders s the way json.dumps does with ensure_ascii=False:
// ", \ and the five named control escapes, other control characters as
// \u00xx (lowercase, four digits), and every other code point passed through.
//
// DEL (U+007F) is NOT escaped, matching Python. encoding/json escapes <, > and
// & as \u003c/\u003e/\u0026 and always escapes U+2028/U+2029; Python does
// neither, so those would be visible file differences.
func pyJSONString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// pyJSONNumber renders a decoded JSON number the way Python re-encodes it.
//
// Python's json.load produces an int for a literal without a fraction or
// exponent and a float otherwise; json.dumps then writes the int verbatim and
// the float through repr(). Reproducing that split matters: 9000000000.0 must
// not become 9000000000, because that would change the value's JSON type and
// would be read back as an int by the Python side.
func pyJSONNumber(n json.Number) string {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		// Integer literal. big.Int canonicalizes it exactly as Python's int
		// parser does ("-0" becomes "0", leading zeros cannot occur in valid
		// JSON) and keeps arbitrary precision.
		if i, ok := new(big.Int).SetString(s, 10); ok {
			return i.String()
		}
		return s
	}
	f, err := n.Float64()
	if err != nil {
		return s
	}
	return pyFloatRepr(f)
}

// PyFloatRepr renders f exactly as Python's repr()/str() does.
//
// Exported for the same reason as PyStr: a stored timestamp read back through
// PendingRequest is a raw value, and a caller that displays it must render it
// the way the Python runtime does — including the trailing ".0" that keeps an
// integral timestamp a float rather than silently turning it into an integer.
func PyFloatRepr(f float64) string { return pyFloatRepr(f) }

// pyFloatRepr renders f exactly as Python's repr()/str() does.
//
// CPython uses _Py_dg_dtoa in shortest-round-trip mode and then switches to
// exponential notation only when the decimal point falls outside
// -4 < decpt <= 16. strconv's own formatting differs ('g' switches at
// decpt >= 21 and never appends the trailing ".0"), so the digits are taken
// from the shortest 'e' form and the layout is rebuilt here.
func pyFloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case f == 0:
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}

	neg := math.Signbit(f)
	// "d.dddddde±dd": shortest digit string plus a decimal exponent.
	sci := strconv.FormatFloat(math.Abs(f), 'e', -1, 64)
	mantissa, expText, _ := strings.Cut(sci, "e")
	exp, err := strconv.Atoi(expText)
	if err != nil {
		return sci
	}
	digits := strings.Replace(mantissa, ".", "", 1)
	// decpt is the position of the decimal point relative to the digit string.
	decpt := exp + 1

	var out string
	switch {
	case decpt <= -4 || decpt > 16:
		out = digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		sign := "+"
		if exp < 0 {
			sign = "-"
			exp = -exp
		}
		out += "e" + sign + fmt.Sprintf("%02d", exp)
	case decpt <= 0:
		out = "0." + strings.Repeat("0", -decpt) + digits
	case decpt >= len(digits):
		out = digits + strings.Repeat("0", decpt-len(digits)) + ".0"
	default:
		out = digits[:decpt] + "." + digits[decpt:]
	}
	if neg {
		return "-" + out
	}
	return out
}

// PyStr renders v the way Python's str() does, for the JSON values a pairing
// entry can hold.
//
// It is exported because PendingRequest.SenderID and the other stored fields
// are `any`: the reference reads them straight out of the file and renders them
// with str(), so a caller that displays a pending request needs exactly this
// function to agree with the Python command handler.
func PyStr(v any) string { return pyStr(v) }

// pyStr is Python's str() for the values pairing.json can hold.
//
// store.py calls str() on values read straight out of the file, so a
// hand-edited entry such as "approved": {"telegram": [12345]} is stored as the
// string "12345", and a container is stored as its Python repr. Reproducing the
// repr for lists and dicts keeps that path byte-identical instead of merely
// close.
func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case string:
		return t
	case json.Number:
		return pyJSONNumber(t)
	case float64:
		return pyFloatRepr(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case []any:
		return pyReprSeq(t)
	case []string:
		items := make([]any, len(t))
		for i, item := range t {
			items[i] = item
		}
		return pyReprSeq(items)
	case map[string]any:
		return pyReprMap(t)
	case *jsonObject:
		return pyReprObject(t)
	default:
		// No JSON value reaches this branch. A Go-only value is rendered with
		// fmt so the store degrades to something readable rather than empty.
		return fmt.Sprint(v)
	}
}

func pyReprSeq(items []any) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(pyRepr(item))
	}
	b.WriteByte(']')
	return b.String()
}

// pyReprMap renders a plain Go map as a Python dict repr.
//
// Go maps have no order, so the keys are sorted. That is deterministic, and it
// agrees with Python whenever the dict holds a single key — which is the only
// case in which Python's order is inferable from the value alone. Values read
// back from the store file never take this path: they are *jsonObject, which
// does carry the file's key order.
func pyReprMap(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(pyReprString(key))
		b.WriteString(": ")
		b.WriteString(pyRepr(m[key]))
	}
	b.WriteByte('}')
	return b.String()
}

func pyReprObject(o *jsonObject) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, key := range o.keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(pyReprString(key))
		b.WriteString(": ")
		b.WriteString(pyRepr(o.vals[key]))
	}
	b.WriteByte('}')
	return b.String()
}

// pyRepr is Python's repr() for JSON values. For everything except a string it
// agrees with str() in Python 3.
func pyRepr(v any) string {
	if s, ok := v.(string); ok {
		return pyReprString(s)
	}
	return pyStr(v)
}

// pyReprString is Python's repr() of a str: single quotes unless the value
// contains a single quote and no double quote, in which case double quotes are
// used. Named escapes are \\, the active quote, \n, \r and \t; other
// non-printable code points become \xNN, \uNNNN or \UNNNNNNNN.
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
		case r == '\\':
			b.WriteString(`\\`)
		case byte(r) == quote && r < 0x80:
			b.WriteByte('\\')
			b.WriteByte(quote)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case unicode.IsPrint(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}
