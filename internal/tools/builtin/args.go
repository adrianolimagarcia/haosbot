package builtin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// ---------------------------------------------------------------------------
// Parameter schema fragments
// ---------------------------------------------------------------------------
//
// The reference builds parameter objects with tool_parameters_schema
// (nanobot/agent/tools/schema.py:217-235), which emits
// {"type":"object","properties":{...},"additionalProperties":false} plus an
// optional "required" list, from StringSchema / IntegerSchema / BooleanSchema
// fragments (schema.py:20-145). A nullable fragment emits "type":["x","null"].

// strProp is a StringSchema fragment (schema.py:38-51).
func strProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// nullableStrProp is a nullable StringSchema fragment (schema.py:38-51).
func nullableStrProp(description string) map[string]any {
	return map[string]any{"type": []string{"string", "null"}, "description": description}
}

// intProp is an IntegerSchema fragment (schema.py:72-85).
func intProp(description string, minimum, maximum int) map[string]any {
	return map[string]any{
		"type":        "integer",
		"description": description,
		"minimum":     minimum,
		"maximum":     maximum,
	}
}

// minIntProp is an IntegerSchema fragment with a lower bound only.
func minIntProp(description string, minimum int) map[string]any {
	return map[string]any{
		"type":        "integer",
		"description": description,
		"minimum":     minimum,
	}
}

// nullableIntProp is a nullable IntegerSchema fragment (schema.py:72-85).
func nullableIntProp(description string, minimum, maximum int) map[string]any {
	return map[string]any{
		"type":        []string{"integer", "null"},
		"description": description,
		"minimum":     minimum,
		"maximum":     maximum,
	}
}

// boolProp is a BooleanSchema fragment with a default (schema.py:136-145).
func boolProp(description string, def bool) map[string]any {
	return map[string]any{"type": "boolean", "description": description, "default": def}
}

// plainBoolProp is a BooleanSchema fragment without a default (schema.py:136-145).
func plainBoolProp(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

// nullableMinIntProp is a nullable IntegerSchema fragment with a lower bound only.
func nullableMinIntProp(description string, minimum int) map[string]any {
	return map[string]any{
		"type":        []string{"integer", "null"},
		"description": description,
		"minimum":     minimum,
	}
}

// nullableBoolProp is a nullable BooleanSchema fragment (schema.py:136-145).
func nullableBoolProp(description string, def bool) map[string]any {
	return map[string]any{
		"type":        []string{"boolean", "null"},
		"description": description,
		"default":     def,
	}
}

// objectSchema renders a strict parameter object. Key order is not significant
// (encoding/json sorts map keys), but the output is deterministic.
func objectSchema(required []string, props map[string]any) json.RawMessage {
	root := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		root["required"] = required
	}
	return mustJSON(root)
}

// mustJSON marshals a schema fragment. Every fragment in this package is built
// from plain JSON values, so a failure is a programming error, not a runtime
// condition.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("builtin: invalid parameter schema: %v", err))
	}
	return b
}

// ---------------------------------------------------------------------------
// Argument decoding
// ---------------------------------------------------------------------------

// argObject is a decoded tool argument object.
type argObject map[string]any

// parseArgs decodes raw tool arguments into an object and rejects keys the tool
// does not declare, mirroring the reference's strict parameter objects
// (additionalProperties=false, schema.py:225-229) and the registry's error
// message shape (registry.py:141-146).
//
// Absent, empty and null arguments become an empty object, mirroring
// Tool.normalize/parse_tool_arguments (base.py:112) and this repository's
// tools.NormalizeArguments.
func parseArgs(toolName string, raw json.RawMessage, allowed ...string) (argObject, *tools.Result) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return argObject{}, nil
	}
	if trimmed[0] != '{' {
		return nil, invalidParams(toolName, "parameters must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(trimmed)))
	// UseNumber keeps integers exact instead of round-tripping through float64.
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, invalidParams(toolName, err.Error())
	}
	if dec.More() {
		return nil, invalidParams(toolName, "arguments are not valid JSON")
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	for key := range obj {
		if _, ok := allowedSet[key]; !ok {
			return nil, invalidParams(toolName, "unexpected parameter "+key)
		}
	}
	return argObject(obj), nil
}

// invalidParams builds the validation error the reference registry produces
// (registry.py:141-146): `Error: Invalid parameters for tool '<name>': <detail>`.
func invalidParams(toolName, detail string) *tools.Result {
	r := tools.Errf("Error: Invalid parameters for tool '%s': %s", toolName, detail)
	return &r
}

// requiredString returns a required string argument. A missing or null value is
// reported as "missing required <key>", matching validate_params (base.py:88-94).
func requiredString(toolName string, a argObject, key string) (string, *tools.Result) {
	v, ok := a[key]
	if !ok || v == nil {
		return "", invalidParams(toolName, "missing required "+key)
	}
	s, ok := coerceString(v)
	if !ok {
		return "", invalidParams(toolName, key+" should be string")
	}
	return s, nil
}

// optionalString returns (value, present, errorResult).
func optionalString(toolName string, a argObject, key string) (string, bool, *tools.Result) {
	v, ok := a[key]
	if !ok || v == nil {
		return "", false, nil
	}
	s, ok := coerceString(v)
	if !ok {
		return "", false, invalidParams(toolName, key+" should be string")
	}
	return s, true, nil
}

// optionalInt returns (value, present, errorResult).
func optionalInt(toolName string, a argObject, key string) (int, bool, *tools.Result) {
	v, ok := a[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	n, ok := coerceInt(v)
	if !ok {
		return 0, false, invalidParams(toolName, key+" should be integer")
	}
	return n, true, nil
}

// optionalBool returns (value, present, errorResult).
func optionalBool(toolName string, a argObject, key string) (bool, bool, *tools.Result) {
	v, ok := a[key]
	if !ok || v == nil {
		return false, false, nil
	}
	b, ok := coerceBool(v)
	if !ok {
		return false, false, invalidParams(toolName, key+" should be boolean")
	}
	return b, true, nil
}

// ---------------------------------------------------------------------------
// Schema-driven coercion (mirrors Tool._cast_value, base.py:258-295)
// ---------------------------------------------------------------------------

func coerceString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		return strconv.FormatBool(t), true
	}
	return "", false
}

func coerceInt(v any) (int, bool) {
	switch t := v.(type) {
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n), true
		}
		f, err := t.Float64()
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) || f != math.Trunc(f) {
			return 0, false
		}
		if f < math.MinInt64 || f > math.MaxInt64 {
			return 0, false
		}
		return int(f), true
	case string:
		// Python int(" 5 ") succeeds; int("5.5") raises and the value stays a
		// string, which then fails validation (base.py:270-274).
		if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return int(n), true
		}
		return 0, false
	}
	return 0, false
}

func coerceBool(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1", "yes":
			return true, true
		case "false", "0", "no":
			return false, true
		}
	}
	return false, false
}
