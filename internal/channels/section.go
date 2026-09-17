package channels

import (
	"encoding/json"
	"strings"
)

// Section is a channel's configuration section.
//
// Python types this parameter `Any` (base.py:35) and duck-types it at every
// access site: a decoded dict is read with .get(), anything else with getattr()
// (base.py:229-234, :239-245). The two paths are NOT equivalent — only the dict
// path honours the camelCase `allowFrom` alias (base.py:241-245) — so this port
// keeps both and makes the distinction part of the type instead of guessing.
//
// The zero Section behaves as an object section with no attributes, which is
// what Python does with a None config: no allowlist, streaming off. That is the
// deny-by-default direction, so a zero value can never accidentally authorize a
// sender.
type Section struct {
	raw map[string]any
	obj *ObjectSection
}

// ObjectSection is the attribute-shaped section: the Go equivalent of the
// Pydantic model or SimpleNamespace that the Python manager can pass instead of
// a dict.
//
// Both fields are `any` rather than their obvious concrete types on purpose.
// Python applies no type check on this path: a string `allow_from` is matched
// with `in`, which is a SUBSTRING test, and a non-bool `streaming` is evaluated
// for truthiness. Typing these as []string and bool would make those inputs
// unrepresentable and silently change the authorization decision.
type ObjectSection struct {
	// AllowFrom is the `allow_from` attribute.
	AllowFrom any
	// Streaming is the `streaming` attribute.
	Streaming any
}

// NewMapSection builds a Section from a decoded configuration dict, which is
// what a real deployment always uses: ChannelsConfig stores each channel
// section as a raw JSON object (schema.py:23-39, extra="allow").
//
// A nil map is treated as an empty map, matching Python, where {} and a missing
// section both reach the dict branch.
func NewMapSection(m map[string]any) Section {
	if m == nil {
		m = map[string]any{}
	}
	return Section{raw: m}
}

// NewObjectSection builds a Section from typed fields.
func NewObjectSection(o ObjectSection) Section { return Section{obj: &o} }

// IsMap reports whether the section is the decoded-dict form.
func (s Section) IsMap() bool { return s.raw != nil }

// Map returns the decoded configuration dict, when the section has one.
func (s Section) Map() (map[string]any, bool) { return s.raw, s.raw != nil }

// Object returns the attribute-shaped section, when the section has one.
func (s Section) Object() (ObjectSection, bool) {
	if s.obj == nil {
		return ObjectSection{}, false
	}
	return *s.obj, true
}

// allowValue is the resolved allowlist, reproducing Python's
//
//	config.get("allow_from") or config.get("allowFrom") or []   # dict path
//	getattr(config, "allow_from", None) or []                   # object path
//
// The `or` chain is load-bearing: a FALSY value falls through to the alias, so
// {"allow_from": [], "allowFrom": ["alice"]} authorizes "alice". The result is
// returned untyped because a non-list value is legal input whose semantics
// (substring, dict-key membership) must be reproduced by the caller.
func (s Section) allowValue() any {
	if s.raw != nil {
		if v := s.raw["allow_from"]; pyTruthy(v) {
			return v
		}
		if v := s.raw["allowFrom"]; pyTruthy(v) {
			return v
		}
		return []any{}
	}
	if s.obj != nil {
		if pyTruthy(s.obj.AllowFrom) {
			return s.obj.AllowFrom
		}
		return []any{}
	}
	return []any{}
}

// streamingValue is the raw `streaming` setting. Python: getattr(config,
// "streaming", False) on the object path, .get("streaming", False) on the dict
// path — with NO camelCase alias, unlike the allowlist.
func (s Section) streamingValue() any {
	if s.raw != nil {
		return s.raw["streaming"]
	}
	if s.obj != nil {
		return s.obj.Streaming
	}
	return nil
}

// pyTruthy reproduces Python's bool() for the values a JSON configuration can
// hold: None, false, 0, 0.0, "" and empty containers are falsy; everything else
// is truthy, including the strings "0" and "false" and a NaN float.
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	case float32:
		return t != 0
	case json.Number:
		// Reachable when a caller hands a raw decoded section straight to the
		// channel instead of a config-model value. Python has one number type
		// in JSON, so 0 is falsy however it was decoded.
		f, err := t.Float64()
		return err != nil || f != 0
	case []any:
		return len(t) != 0
	case []string:
		return len(t) != 0
	case map[string]any:
		return len(t) != 0
	}
	// An arbitrary Go value has no Python __bool__/__len__ to consult; Python
	// treats an object with neither as truthy, so this is the faithful default.
	return true
}

// containsToken reports whether token is "in" value, using Python's `in`
// operator semantics.
//
// This is deliberately not a list membership test:
//
//   - a list matches only elements that are STRINGS equal to token, because
//     Python's `==` between a str and an int/bool/None is False. An allowlist
//     entry of 12345 therefore does NOT match the sender "12345";
//   - a string is a SUBSTRING test, so allow_from "*" authorizes everyone and
//     allow_from "alice,bob" authorizes "alice";
//   - a dict is a KEY membership test, so {"*": 1} authorizes everyone.
//
// DIVERGENCE (documented, not silent): for a value Python cannot iterate over
// (an int, a bool, None) the reference raises TypeError out of is_allowed and
// the channel handler dies. Go has no exception channel to carry that, so the
// lookup reports "no match" and the sender is denied. Failing closed is the
// only safe translation of an unrepresentable failure.
func containsToken(value any, token string) bool {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s == token {
				return true
			}
		}
		return false
	case []string:
		for _, item := range v {
			if item == token {
				return true
			}
		}
		return false
	case string:
		// Python's `in` on str is a substring test. Byte-level containment and
		// code-point containment agree for valid UTF-8, which is all Python
		// can produce.
		return strings.Contains(v, token)
	case map[string]any:
		_, ok := v[token]
		return ok
	case map[string]string:
		_, ok := v[token]
		return ok
	}
	return false
}
