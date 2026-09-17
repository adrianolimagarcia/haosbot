package telegram

import (
	"encoding/json"
	"sort"
	"strconv"
)

// The Telegram management contract, ported from
// nanobot/channels/telegram/manifest.py and the pieces of
// nanobot/channels/contracts.py and nanobot/channels/_manifest.py it depends on.
//
// The reference builds SETUP_SPEC at import time from a ChannelPlugin; the Go
// port is a STATIC descriptor, which is what the channel registry needs. The
// values are pinned against the reference by the differential test, field by
// field and in order, because the order is what the WebUI renders.

// FieldKind is ChannelFieldSpec.kind / FieldKind.
type FieldKind string

// The kinds the Telegram contract uses.
const (
	KindString FieldKind = "string"
	KindSecret FieldKind = "secret"
	KindList   FieldKind = "list"
	KindEnum   FieldKind = "enum"
	KindBool   FieldKind = "bool"
	KindInt    FieldKind = "int"
	KindFloat  FieldKind = "float"
)

// SetupField is ChannelFieldSpec.
//
// Choices is stored SORTED, because ChannelFieldSpec holds a frozenset and every
// consumer that serialises it sorts: to_public_dict writes `sorted(choices)` and
// the setup payload the WebUI reads is therefore alphabetical, not declaration
// order. Default is nil for "no default" — which is NOT the same as a false or
// zero default, and to_public_dict only writes default_value when it is non-nil.
type SetupField struct {
	Name     string
	Kind     FieldKind
	Choices  []string
	Default  any
	Writable bool
	Snapshot bool
}

// SetupRequirement is SetupRequirement: a requirement satisfied by any one
// complete group of fields.
type SetupRequirement struct {
	Alternatives [][]string
}

// SimpleField is SetupRequirement.simple_field: the single field name when the
// requirement is exactly one group of one field, otherwise "".
func (r SetupRequirement) SimpleField() string {
	if len(r.Alternatives) == 1 && len(r.Alternatives[0]) == 1 {
		return r.Alternatives[0][0]
	}
	return ""
}

// SetupSpec is ChannelSetupSpec.
type SetupSpec struct {
	Fields             []SetupField
	Required           []SetupRequirement
	OfficialURL        string
	VerifiesConnection bool
	// HasValidator records that the reference attaches a Python callable
	// (`validate`). Go has no comparable identity to store, so this is a flag;
	// the function itself is this package's Validate.
	HasValidator bool
}

// SETUP_SPEC is the Telegram channel's ChannelSetupSpec (manifest.py:8-33).
//
// The 19 fields are in declaration order and the defaults are the ones
// TelegramConfig declares, including the emoji default for reactEmoji. The
// groupPolicy choices are GROUP_POLICIES — all three, even though
// TelegramConfig's annotation only accepts two — which is why the WebUI offers
// "allowlist" and the model then rejects it.
var SETUP_SPEC = &SetupSpec{
	Fields: []SetupField{
		{Name: "token", Kind: KindSecret, Writable: true, Snapshot: true},
		{Name: "proxy", Kind: KindString, Writable: true, Snapshot: true},
		{Name: "allowFrom", Kind: KindList, Writable: true, Snapshot: true},
		{Name: "groupPolicy", Kind: KindEnum, Choices: GroupPolicies, Default: "mention", Writable: true, Snapshot: true},
		{Name: "mode", Kind: KindEnum, Choices: []string{"polling", "webhook"}, Default: "polling", Writable: true, Snapshot: true},
		{Name: "replyToMessage", Kind: KindBool, Default: false, Writable: true, Snapshot: true},
		{Name: "reactEmoji", Kind: KindString, Default: "👀", Writable: true, Snapshot: true},
		{Name: "connectionPoolSize", Kind: KindInt, Default: 32, Writable: true, Snapshot: true},
		{Name: "poolTimeout", Kind: KindFloat, Default: 5.0, Writable: true, Snapshot: true},
		{Name: "streaming", Kind: KindBool, Default: true, Writable: true, Snapshot: true},
		{Name: "inlineKeyboards", Kind: KindBool, Default: false, Writable: true, Snapshot: true},
		{Name: "richMessages", Kind: KindBool, Default: false, Writable: true, Snapshot: true},
		{Name: "streamEditInterval", Kind: KindFloat, Default: 0.6, Writable: true, Snapshot: true},
		{Name: "webhookUrl", Kind: KindString, Writable: true, Snapshot: true},
		{Name: "webhookListenHost", Kind: KindString, Default: "127.0.0.1", Writable: true, Snapshot: true},
		{Name: "webhookListenPort", Kind: KindInt, Default: 8081, Writable: true, Snapshot: true},
		{Name: "webhookPath", Kind: KindString, Default: "/telegram", Writable: true, Snapshot: true},
		{Name: "webhookSecretToken", Kind: KindSecret, Writable: true, Snapshot: true},
		{Name: "webhookMaxConnections", Kind: KindInt, Default: 4, Writable: true, Snapshot: true},
	},
	Required:           []SetupRequirement{{Alternatives: [][]string{{"token"}}}},
	OfficialURL:        "https://t.me/BotFather",
	VerifiesConnection: true,
	HasValidator:       true,
}

// LookupSetupSpec is channel_setup_spec(name) for this package's registry.
// It returns nil for any channel this package does not own.
func LookupSetupSpec(name string) *SetupSpec {
	if name == ChannelName {
		return SETUP_SPEC
	}
	return nil
}

// Field returns the spec for one field name, or nil.
func (s *SetupSpec) Field(name string) *SetupField {
	for i := range s.Fields {
		if s.Fields[i].Name == name {
			return &s.Fields[i]
		}
	}
	return nil
}

// SimpleRequiredFields is ChannelSetupSpec.simple_required_fields.
func (s *SetupSpec) SimpleRequiredFields() []string {
	out := []string{}
	for _, requirement := range s.Required {
		if field := requirement.SimpleField(); field != "" {
			out = append(out, field)
		}
	}
	return out
}

// Secrets is ChannelSetupSpec.secrets: the names of the secret-kind fields, in
// field order.
func (s *SetupSpec) Secrets() []string {
	out := []string{}
	for _, field := range s.Fields {
		if field.Kind == KindSecret {
			out = append(out, field.Name)
		}
	}
	return out
}

// ToPublicDict is ChannelSetupSpec.to_public_dict: the writable setup contract
// the generic WebUI consumer reads.
//
// `key` is prefixed with "channels.<name>.", choices are sorted, `required`
// reflects only the simple required fields, and default_value is omitted for a
// nil default (NOT written as null).
func (s *SetupSpec) ToPublicDict(channelName string) map[string]any {
	simpleRequired := map[string]bool{}
	for _, name := range s.SimpleRequiredFields() {
		simpleRequired[name] = true
	}
	fields := []any{}
	for _, field := range s.Fields {
		if !field.Writable {
			continue
		}
		choices := append([]string{}, field.Choices...)
		sort.Strings(choices)
		public := map[string]any{
			"key":      "channels." + channelName + "." + field.Name,
			"field":    field.Name,
			"kind":     string(field.Kind),
			"choices":  choices,
			"required": simpleRequired[field.Name],
		}
		if field.Default != nil {
			public["default_value"] = stringifyChannelValue(field.Default)
		}
		fields = append(fields, public)
	}
	requirements := []any{}
	for _, requirement := range s.Required {
		alternatives := []any{}
		for _, group := range requirement.Alternatives {
			names := []string{}
			for _, name := range group {
				names = append(names, "channels."+channelName+"."+name)
			}
			alternatives = append(alternatives, names)
		}
		requirements = append(requirements, map[string]any{"alternatives": alternatives})
	}
	payload := map[string]any{"fields": fields, "requirements": requirements}
	if s.OfficialURL != "" {
		payload["official_url"] = s.OfficialURL
	}
	if s.VerifiesConnection {
		payload["verifies_connection"] = true
	}
	return payload
}

// stringifyChannelValue is stringify_channel_value (contracts.py:583-590).
//
// Bools render lower-case, which is what an HTML form needs, and that is the
// only reason "false" is a string here rather than a JSON false.
func stringifyChannelValue(value any) string {
	switch t := value.(type) {
	case bool:
		if t {
			return "true"
		}
		return "false"
	case []any:
		out := ""
		for i, item := range t {
			if i > 0 {
				out += ", "
			}
			out += pyStr(item)
		}
		return out
	case map[string]any:
		encoded, err := json.MarshalIndent(t, "", "  ")
		if err != nil {
			return pyStr(t)
		}
		return string(encoded)
	case int:
		return strconv.Itoa(t)
	case float64:
		return pyFloatStr(t)
	}
	return pyStr(value)
}
