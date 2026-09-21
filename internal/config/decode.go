package config

// JSON decoding engine with pydantic-compatible semantics.
//
// The Python reference validates the parsed JSON with `Config.model_validate(data)`
// (loader.py:124). This file reproduces the parts of that behavior the schema
// actually depends on:
//
//   - alias resolution: an explicit `validation_alias` AliasChoices wins, then
//     `populate_by_name` accepts the raw snake_case field name; without an
//     explicit alias the generated to_camel name is tried first, then the field
//     name (config_base.py:12-15).
//   - unknown keys: ignored for models on `Base` (pydantic's default extra policy
//     is "ignore"), preserved for `ChannelsConfig` / `ProvidersConfig`
//     (extra="allow", schema.py:31 and schema.py:247).
//   - pydantic "lax" coercion, verified empirically against pydantic 2.13.5:
//     bool->int, integral float->int, numeric string->int/float, bool strings,
//     and NO coercion into `str`.
//   - min/max bounds produce errors, never clamping.
//   - error locations name the alias that matched in the input (verified: a
//     bound violation on `session_ttl_minutes` reports the key the user wrote).
//   - model-level ("after") validators only run when every field validated.

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Field metadata + alias resolution
// ---------------------------------------------------------------------------

// fieldDef describes one pydantic field's name and explicit validation aliases.
type fieldDef struct {
	// name is the Python field name (snake_case).
	name string
	// aliases is the `validation_alias=AliasChoices(...)` tuple, in order. Empty
	// means the only alias is the generated to_camel(name) plus the field name.
	aliases []string
}

// get resolves the field inside o, reproducing pydantic's lookup order:
// AliasChoices entries in declaration order, then the field name
// (populate_by_name=True). It also returns the key that matched, because
// pydantic reports that key in the error location.
func (f fieldDef) get(o *jmap) (value any, key string, ok bool) {
	if len(f.aliases) > 0 {
		for _, a := range f.aliases {
			if v, found := o.m[a]; found {
				return v, a, true
			}
		}
		if v, found := o.m[f.name]; found {
			return v, f.name, true
		}
		return nil, "", false
	}
	camel := toCamel(f.name)
	if v, found := o.m[camel]; found {
		return v, camel, true
	}
	if v, found := o.m[f.name]; found {
		return v, f.name, true
	}
	return nil, "", false
}

// accepts reports whether key is consumed by this field (alias or name).
func (f fieldDef) accepts(key string) bool {
	if len(f.aliases) > 0 {
		for _, a := range f.aliases {
			if a == key {
				return true
			}
		}
		return key == f.name
	}
	return key == toCamel(f.name) || key == f.name
}

// jmap is a decoded JSON object with its issue path.
type jmap struct {
	m    map[string]any
	path []PathPart
}

func (o *jmap) child(name string) *jmap {
	return &jmap{m: nil, path: appendPath(o.path, Field(name))}
}

func appendPath(p []PathPart, extra ...PathPart) []PathPart {
	out := make([]PathPart, 0, len(p)+len(extra))
	out = append(out, p...)
	out = append(out, extra...)
	return out
}

// ---------------------------------------------------------------------------
// Collector
// ---------------------------------------------------------------------------

type collector struct {
	issues []Issue
}

func (c *collector) add(path []PathPart, code, message string) {
	cp := make([]PathPart, len(path))
	copy(cp, path)
	c.issues = append(c.issues, Issue{Path: cp, Message: FriendlyMessage(message, code)})
}

// at builds the issue path for `key` inside object o.
func (c *collector) at(o *jmap, key string) []PathPart {
	return appendPath(o.path, Field(key))
}

// ---------------------------------------------------------------------------
// Primitive coercion (pydantic lax mode)
// ---------------------------------------------------------------------------

func isJSONNull(v any) bool { return v == nil }

// asInt ports pydantic's lax `int` validation.
func asInt(v any) (int, bool, string, string) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true, "", ""
		}
		return 0, true, "", ""
	case json.Number:
		if i, err := strconv.ParseInt(t.String(), 10, 64); err == nil {
			return int(i), true, "", ""
		}
		f, err := strconv.ParseFloat(t.String(), 64)
		if err != nil {
			return 0, false, "int_parsing", "Input should be a valid integer, unable to parse string as an integer"
		}
		if f != math.Trunc(f) {
			return 0, false, "int_from_float", "Input should be a valid integer, got a number with a fractional part"
		}
		return int(f), true, "", ""
	case float64:
		if t != math.Trunc(t) {
			return 0, false, "int_from_float", "Input should be a valid integer, got a number with a fractional part"
		}
		return int(t), true, "", ""
	case string:
		s := strings.TrimSpace(t)
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return int(i), true, "", ""
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil && f == math.Trunc(f) {
			return int(f), true, "", ""
		}
		return 0, false, "int_parsing", "Input should be a valid integer, unable to parse string as an integer"
	default:
		return 0, false, "int_type", "Input should be a valid integer"
	}
}

// asFloat ports pydantic's lax `float` validation.
func asFloat(v any) (float64, bool, string, string) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true, "", ""
		}
		return 0, true, "", ""
	case json.Number:
		f, err := strconv.ParseFloat(t.String(), 64)
		if err != nil {
			return 0, false, "float_parsing", "Input should be a valid number, unable to parse string as a number"
		}
		return f, true, "", ""
	case float64:
		return t, true, "", ""
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false, "float_parsing", "Input should be a valid number, unable to parse string as a number"
		}
		return f, true, "", ""
	default:
		return 0, false, "float_type", "Input should be a valid number"
	}
}

// boolTrueStrings / boolFalseStrings mirror pydantic's BOOL_TRUE / BOOL_FALSE.
var (
	boolTrueStrings  = map[string]bool{"1": true, "true": true, "t": true, "yes": true, "y": true, "on": true}
	boolFalseStrings = map[string]bool{"0": true, "false": true, "f": true, "no": true, "n": true, "off": true}
)

// asBool ports pydantic's lax `bool` validation.
func asBool(v any) (bool, bool, string, string) {
	switch t := v.(type) {
	case bool:
		return t, true, "", ""
	case json.Number:
		i, err := strconv.ParseInt(t.String(), 10, 64)
		if err == nil && (i == 0 || i == 1) {
			return i == 1, true, "", ""
		}
		return false, false, "bool_parsing", "Input should be a valid boolean, unable to interpret input"
	case float64:
		if t == 0 || t == 1 {
			return t == 1, true, "", ""
		}
		return false, false, "bool_parsing", "Input should be a valid boolean, unable to interpret input"
	case string:
		s := strings.ToLower(t)
		if boolTrueStrings[s] {
			return true, true, "", ""
		}
		if boolFalseStrings[s] {
			return false, true, "", ""
		}
		return false, false, "bool_parsing", "Input should be a valid boolean, unable to interpret input"
	default:
		return false, false, "bool_type", "Input should be a valid boolean"
	}
}

// asString ports pydantic's `str` validation, which does NOT coerce numbers,
// booleans or null in lax mode.
func asString(v any) (string, bool, string, string) {
	if s, ok := v.(string); ok {
		return s, true, "", ""
	}
	return "", false, "string_type", "Input should be a valid string"
}

// ---------------------------------------------------------------------------
// Composite coercion
// ---------------------------------------------------------------------------

// asStringList ports `list[str]`.
func (c *collector) asStringList(v any, path []PathPart, def []string) []string {
	if isJSONNull(v) {
		c.add(path, "list_type", "Input should be a valid list")
		return def
	}
	arr, ok := v.([]any)
	if !ok {
		c.add(path, "list_type", "Input should be a valid list")
		return def
	}
	out := make([]string, 0, len(arr))
	for i, item := range arr {
		s, ok2, code, msg := asString(item)
		if !ok2 {
			c.add(appendPath(path, Index(i)), code, msg)
			continue
		}
		out = append(out, s)
	}
	return out
}

// asStringMap ports `dict[str, str]`. `optional` selects `dict[str,str] | None`.
func (c *collector) asStringMap(v any, path []PathPart, optional bool) map[string]string {
	if isJSONNull(v) {
		if optional {
			return nil
		}
		c.add(path, "dict_type", "Input should be a valid dictionary")
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		c.add(path, "dict_type", "Input should be a valid dictionary")
		return nil
	}
	out := make(map[string]string, len(m))
	for k, item := range m {
		s, ok2, code, msg := asString(item)
		if !ok2 {
			c.add(appendPath(path, Field(k)), code, msg)
			continue
		}
		out[k] = s
	}
	return out
}

// asAnyMap ports `dict[str, Any]` (no element validation).
func (c *collector) asAnyMap(v any, path []PathPart) map[string]any {
	if isJSONNull(v) {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		c.add(path, "dict_type", "Input should be a valid dictionary")
		return nil
	}
	return m
}

// asObject narrows a value to a JSON object for a nested model, emitting
// pydantic's `model_type` error otherwise.
func (c *collector) asObject(v any, path []PathPart, model string) (*jmap, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		c.add(path, "model_type", "Input should be a valid dictionary or instance of "+model)
		return nil, false
	}
	return &jmap{m: m, path: appendPath(path)}, true
}

// ---------------------------------------------------------------------------
// Scalar field readers
// ---------------------------------------------------------------------------

func (c *collector) readString(o *jmap, f fieldDef, def string) string {
	v, key, ok := f.get(o)
	if !ok {
		return def
	}
	s, ok2, code, msg := asString(v)
	if !ok2 {
		c.add(c.at(o, key), code, msg)
		return def
	}
	return s
}

func (c *collector) readOptString(o *jmap, f fieldDef, def *string) *string {
	v, key, ok := f.get(o)
	if !ok {
		return def
	}
	if isJSONNull(v) {
		return nil
	}
	s, ok2, code, msg := asString(v)
	if !ok2 {
		c.add(c.at(o, key), code, msg)
		return def
	}
	return &s
}

// readOptBool reads a tri-state boolean. The pointer distinguishes "absent or
// null" from an explicit false, which is what a setting whose SAFE value is true
// needs: an unset key must not be able to read as "disabled".
func (c *collector) readOptBool(o *jmap, f fieldDef, def *bool) *bool {
	v, key, ok := f.get(o)
	if !ok {
		return def
	}
	if isJSONNull(v) {
		return nil
	}
	b, ok2, code, msg := asBool(v)
	if !ok2 {
		c.add(c.at(o, key), code, msg)
		return def
	}
	return &b
}

func (c *collector) readBool(o *jmap, f fieldDef, def bool) bool {
	v, key, ok := f.get(o)
	if !ok {
		return def
	}
	b, ok2, code, msg := asBool(v)
	if !ok2 {
		c.add(c.at(o, key), code, msg)
		return def
	}
	return b
}

func (c *collector) readFloat(o *jmap, f fieldDef, def float64) float64 {
	v, key, ok := f.get(o)
	if !ok {
		return def
	}
	fv, ok2, code, msg := asFloat(v)
	if !ok2 {
		c.add(c.at(o, key), code, msg)
		return def
	}
	return fv
}

// readStringList reads a `list[str]` field.
func (c *collector) readStringList(o *jmap, f fieldDef, def []string) []string {
	v, key, ok := f.get(o)
	if !ok {
		return def
	}
	return c.asStringList(v, c.at(o, key), def)
}

// intBound is an inclusive numeric bound (pydantic ge/le).
type intBound struct {
	set bool
	val int
}

// Ge builds an inclusive lower bound.
func Ge(v int) intBound { return intBound{set: true, val: v} }

// Le builds an inclusive upper bound.
func Le(v int) intBound { return intBound{set: true, val: v} }

// readInt reads an int field applying inclusive bounds. Bounds are reported as
// errors; the value is never clamped.
func (c *collector) readInt(o *jmap, f fieldDef, def int, min, max intBound) int {
	v, key, ok := f.get(o)
	if !ok {
		return def
	}
	iv, ok2, code, msg := asInt(v)
	if !ok2 {
		c.add(c.at(o, key), code, msg)
		return def
	}
	if min.set && iv < min.val {
		c.add(c.at(o, key), "greater_than_equal",
			"Input should be greater than or equal to "+strconv.Itoa(min.val))
		return def
	}
	if max.set && iv > max.val {
		c.add(c.at(o, key), "less_than_equal",
			"Input should be less than or equal to "+strconv.Itoa(max.val))
		return def
	}
	return iv
}

// readLiteral reads a Literal[...] field.
func (c *collector) readLiteral(o *jmap, f fieldDef, def string, allowed ...string) string {
	v, key, ok := f.get(o)
	if !ok {
		return def
	}
	if s, isStr := v.(string); isStr {
		for _, a := range allowed {
			if s == a {
				return s
			}
		}
	}
	c.add(c.at(o, key), "literal_error", "Input should be "+literalList(allowed))
	return def
}

// readOptLiteral reads a `Literal[...] | None` field.
func (c *collector) readOptLiteral(o *jmap, f fieldDef, def *string, allowed ...string) *string {
	v, key, ok := f.get(o)
	if !ok {
		return def
	}
	if isJSONNull(v) {
		return nil
	}
	if s, isStr := v.(string); isStr {
		for _, a := range allowed {
			if s == a {
				cp := s
				return &cp
			}
		}
	}
	c.add(c.at(o, key), "literal_error", "Input should be "+literalList(allowed))
	return def
}

// stringMatcher abstracts regexp.Regexp for pattern-constrained fields.
type stringMatcher interface{ MatchString(string) bool }

// readPattern reads an optional string field constrained by a regex.
func (c *collector) readPattern(o *jmap, f fieldDef, def *string, pattern string, re stringMatcher) *string {
	v, key, ok := f.get(o)
	if !ok {
		return def
	}
	if isJSONNull(v) {
		return nil
	}
	s, ok2, code, msg := asString(v)
	if !ok2 {
		c.add(c.at(o, key), code, msg)
		return def
	}
	if !re.MatchString(s) {
		c.add(c.at(o, key), "string_pattern_mismatch",
			"String should match pattern '"+pattern+"'")
		return def
	}
	return &s
}

func literalList(allowed []string) string {
	quoted := make([]string, len(allowed))
	for i, a := range allowed {
		quoted[i] = "'" + a + "'"
	}
	switch len(quoted) {
	case 0:
		return ""
	case 1:
		return quoted[0]
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
}
