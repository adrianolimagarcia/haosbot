package telegram

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// Config is TelegramConfig (runtime.py:413-467).
//
// The reference is a pydantic model with an alias_generator that turns every
// snake_case field into camelCase and populate_by_name=True, so model_validate
// accepts BOTH spellings and model_dump(by_alias=True) writes camelCase. The
// aliases are what the persisted configuration actually contains, because
// ChannelsConfig stores each channel section as a raw JSON object
// (config/schema.py:23-39, extra="allow"); see configAlias below.
//
// The field order is the reference's declaration order, which is also the order
// pydantic aggregates field errors in — verified by differential test on inputs
// with several simultaneous errors.
type Config struct {
	Enabled               bool
	Token                 string
	Mode                  string
	AllowFrom             []string
	Proxy                 *string
	ReplyToMessage        bool
	ReactEmoji            string
	GroupPolicy           string
	ConnectionPoolSize    int
	PoolTimeout           float64
	Streaming             bool
	InlineKeyboards       bool
	RichMessages          bool
	StreamEditInterval    float64
	WebhookURL            string
	WebhookListenHost     string
	WebhookListenPort     int
	WebhookPath           string
	WebhookSecretToken    string
	WebhookMaxConnections int
}

// DefaultConfig is TelegramConfig() with no arguments, i.e. every field default
// (runtime.py:415-444). Verified against the reference's
// model_dump(by_alias=True) by the differential test.
func DefaultConfig() Config {
	return Config{
		Enabled:               false,
		Token:                 "",
		Mode:                  "polling",
		AllowFrom:             []string{},
		Proxy:                 nil,
		ReplyToMessage:        false,
		ReactEmoji:            "👀",
		GroupPolicy:           "mention",
		ConnectionPoolSize:    32,
		PoolTimeout:           5.0,
		Streaming:             true,
		InlineKeyboards:       false,
		RichMessages:          false,
		StreamEditInterval:    StreamEditIntervalDefault,
		WebhookURL:            "",
		WebhookListenHost:     "127.0.0.1",
		WebhookListenPort:     8081,
		WebhookPath:           "/telegram",
		WebhookSecretToken:    "",
		WebhookMaxConnections: 4,
	}
}

// FieldError is one entry of pydantic's ValidationError.errors().
//
// The Type and Msg strings are pydantic-core's, reproduced verbatim so a caller
// (or the differential harness) can match on them. Loc is the ALIAS path, e.g.
// ["allowFrom", "1"] — pydantic reports the alias it validated against, not the
// Python field name.
type FieldError struct {
	// Type is pydantic's error type, e.g. "string_type", "literal_error",
	// "greater_than_equal", "value_error".
	Type string
	// Loc is the field path, alias-spelled.
	Loc []string
	// Msg is pydantic's human-readable message.
	Msg string
}

// ValidationError is the Go form of pydantic's ValidationError. Every field is
// validated even after a failure, and all failures are reported together.
type ValidationError struct {
	// Errors holds every failure, in field-declaration order.
	Errors []FieldError
}

func (e *ValidationError) Error() string {
	if len(e.Errors) == 0 {
		return "telegram: invalid configuration"
	}
	var b strings.Builder
	b.WriteString("telegram: invalid configuration:")
	for _, fe := range e.Errors {
		b.WriteString("\n  ")
		if len(fe.Loc) > 0 {
			b.WriteString(strings.Join(fe.Loc, "."))
			b.WriteString(": ")
		}
		b.WriteString(fe.Msg)
	}
	return b.String()
}

// fieldKind is the pydantic annotation of a field, restricted to the kinds
// TelegramConfig actually uses.
type fieldKind int

const (
	kindBool fieldKind = iota
	kindStr
	kindOptStr
	kindLiteral
	kindStrList
	kindInt
	kindFloat
)

// fieldSpec is one row of the model, in declaration order.
type fieldSpec struct {
	name    string // Python field name
	alias   string // camelCase alias
	kind    fieldKind
	choices []string
	ge      string // numeric lower bound as pydantic renders it, "" for none
	le      string // numeric upper bound as pydantic renders it, "" for none
}

// configFields is TelegramConfig's declaration order. It is load-bearing:
// pydantic reports errors in this order.
var configFields = []fieldSpec{
	{name: "enabled", alias: "enabled", kind: kindBool},
	{name: "token", alias: "token", kind: kindStr},
	{name: "mode", alias: "mode", kind: kindLiteral, choices: []string{"polling", "webhook"}},
	{name: "allow_from", alias: "allowFrom", kind: kindStrList},
	{name: "proxy", alias: "proxy", kind: kindOptStr},
	{name: "reply_to_message", alias: "replyToMessage", kind: kindBool},
	{name: "react_emoji", alias: "reactEmoji", kind: kindStr},
	{name: "group_policy", alias: "groupPolicy", kind: kindLiteral, choices: []string{"open", "mention"}},
	{name: "connection_pool_size", alias: "connectionPoolSize", kind: kindInt},
	{name: "pool_timeout", alias: "poolTimeout", kind: kindFloat},
	{name: "streaming", alias: "streaming", kind: kindBool},
	{name: "inline_keyboards", alias: "inlineKeyboards", kind: kindBool},
	{name: "rich_messages", alias: "richMessages", kind: kindBool},
	{name: "stream_edit_interval", alias: "streamEditInterval", kind: kindFloat, ge: "0.1"},
	{name: "webhook_url", alias: "webhookUrl", kind: kindStr},
	{name: "webhook_listen_host", alias: "webhookListenHost", kind: kindStr},
	{name: "webhook_listen_port", alias: "webhookListenPort", kind: kindInt, ge: "1", le: "65535"},
	{name: "webhook_path", alias: "webhookPath", kind: kindStr},
	{name: "webhook_secret_token", alias: "webhookSecretToken", kind: kindStr},
	{name: "webhook_max_connections", alias: "webhookMaxConnections", kind: kindInt, ge: "1", le: "100"},
}

// ParseConfig validates a decoded configuration map and returns the model.
// Port of TelegramConfig.model_validate (runtime.py:413-467).
//
// values is the raw `config.Channels.Extra["telegram"]` map, whose keys are the
// camelCase aliases the manifest writes. Both spellings are accepted, and the
// ALIAS WINS when both are present, in either key order — verified against the
// reference. Unknown keys are ignored, matching pydantic's default extra
// behaviour (the model sets no extra= option).
//
// The returned error is a *ValidationError whenever the reference would raise a
// pydantic ValidationError. There is no partial success: like the reference, a
// single failing field means no model.
func ParseConfig(values map[string]any) (Config, error) {
	cfg := DefaultConfig()
	var errs []FieldError

	for _, spec := range configFields {
		raw, present := lookupConfigValue(values, spec)
		if !present {
			continue
		}
		switch spec.kind {
		case kindBool:
			v, fe := coerceBool(raw, spec.alias)
			if fe != nil {
				errs = append(errs, *fe)
				continue
			}
			setBoolField(&cfg, spec.name, v)
		case kindStr:
			v, fe := coerceStr(raw, spec.alias)
			if fe != nil {
				errs = append(errs, *fe)
				continue
			}
			setStrField(&cfg, spec.name, v)
		case kindOptStr:
			v, fe := coerceOptStr(raw, spec.alias)
			if fe != nil {
				errs = append(errs, *fe)
				continue
			}
			cfg.Proxy = v
		case kindLiteral:
			v, fe := coerceLiteral(raw, spec)
			if fe != nil {
				errs = append(errs, *fe)
				continue
			}
			setStrField(&cfg, spec.name, v)
		case kindStrList:
			v, fes := coerceStrList(raw, spec.alias)
			if len(fes) > 0 {
				errs = append(errs, fes...)
				continue
			}
			cfg.AllowFrom = v
		case kindInt:
			v, fe := coerceInt(raw, spec)
			if fe != nil {
				errs = append(errs, *fe)
				continue
			}
			setIntField(&cfg, spec.name, v)
		case kindFloat:
			v, fe := coerceFloat(raw, spec)
			if fe != nil {
				errs = append(errs, *fe)
				continue
			}
			setFloatField(&cfg, spec.name, v)
		}
		// The only field validator in the reference is on webhook_path, and a
		// field validator runs as part of that field's validation, so its error
		// lands at this position in the aggregation order.
		if spec.name == "webhook_path" {
			cfg.WebhookPath = webhookPathMustStartWithSlash(cfg.WebhookPath)
			if !strings.HasPrefix(cfg.WebhookPath, "/") {
				errs = append(errs, FieldError{
					Type: "value_error",
					Loc:  []string{spec.alias},
					Msg:  `Value error, webhook_path must start with "/"`,
				})
			}
		}
	}

	if len(errs) > 0 {
		return cfg, &ValidationError{Errors: errs}
	}
	// The model validator only runs when every field validated.
	if err := cfg.validateWebhookConfig(); err != nil {
		return cfg, &ValidationError{Errors: []FieldError{{
			Type: "value_error",
			Loc:  nil,
			Msg:  "Value error, " + err.Error(),
		}}}
	}
	return cfg, nil
}

// lookupConfigValue resolves a field the way pydantic does with an alias
// generator and populate_by_name: the alias is tried first and, if the key is
// present at all, wins even over an explicit snake_case key.
func lookupConfigValue(values map[string]any, spec fieldSpec) (any, bool) {
	if v, ok := values[spec.alias]; ok {
		return v, true
	}
	if spec.alias != spec.name {
		if v, ok := values[spec.name]; ok {
			return v, true
		}
	}
	return nil, false
}

func setBoolField(c *Config, name string, v bool) {
	switch name {
	case "enabled":
		c.Enabled = v
	case "reply_to_message":
		c.ReplyToMessage = v
	case "streaming":
		c.Streaming = v
	case "inline_keyboards":
		c.InlineKeyboards = v
	case "rich_messages":
		c.RichMessages = v
	}
}

func setStrField(c *Config, name, v string) {
	switch name {
	case "token":
		c.Token = v
	case "mode":
		c.Mode = v
	case "react_emoji":
		c.ReactEmoji = v
	case "group_policy":
		c.GroupPolicy = v
	case "webhook_url":
		c.WebhookURL = v
	case "webhook_listen_host":
		c.WebhookListenHost = v
	case "webhook_path":
		c.WebhookPath = v
	case "webhook_secret_token":
		c.WebhookSecretToken = v
	}
}

func setIntField(c *Config, name string, v int) {
	switch name {
	case "connection_pool_size":
		c.ConnectionPoolSize = v
	case "webhook_listen_port":
		c.WebhookListenPort = v
	case "webhook_max_connections":
		c.WebhookMaxConnections = v
	}
}

func setFloatField(c *Config, name string, v float64) {
	switch name {
	case "pool_timeout":
		c.PoolTimeout = v
	case "stream_edit_interval":
		c.StreamEditInterval = v
	}
}

// ---------------------------------------------------------------------------
// pydantic lax coercion
//
// pydantic v2's default (non-strict) mode coerces between JSON-compatible
// types. Only the cases TelegramConfig can reach are implemented; each one is
// pinned by a differential case, including the error TYPE, which is not uniform
// (a non-integral float is bool_type but a non-0/1 integer is bool_parsing).
// ---------------------------------------------------------------------------

// numberKind classifies a decoded JSON number the way Python's json module
// does: a literal without '.', 'e' or 'E' is an int, anything else a float.
type numberKind int

const (
	numNotANumber numberKind = iota
	numInt
	numFloat
)

func classifyNumber(v any) (numberKind, float64, bool) {
	switch t := v.(type) {
	case int:
		return numInt, float64(t), true
	case int64:
		return numInt, float64(t), true
	case json.Number:
		s := t.String()
		if !strings.ContainsAny(s, ".eE") {
			if i, err := strconv.ParseInt(s, 10, 64); err == nil {
				return numInt, float64(i), true
			}
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return numFloat, f, true
			}
			return numNotANumber, 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return numNotANumber, 0, false
		}
		return numFloat, f, true
	case float64:
		return numFloat, t, true
	case float32:
		return numFloat, float64(t), true
	}
	return numNotANumber, 0, false
}

// boolStrings is pydantic-core's str→bool table.
var boolStrings = map[string]bool{
	"0": false, "off": false, "f": false, "false": false, "n": false, "no": false,
	"1": true, "on": true, "t": true, "true": true, "y": true, "yes": true,
}

func coerceBool(raw any, alias string) (bool, *FieldError) {
	switch t := raw.(type) {
	case bool:
		return t, nil
	case string:
		// NOTE: no trimming. "  true  " is a bool_parsing error in the
		// reference, verified by differential test.
		if v, ok := boolStrings[strings.ToLower(t)]; ok {
			return v, nil
		}
		return false, errField("bool_parsing", alias, "Input should be a valid boolean, unable to interpret input")
	}
	kind, f, ok := classifyNumber(raw)
	if !ok {
		return false, errField("bool_type", alias, "Input should be a valid boolean")
	}
	if kind == numInt {
		if f == 0 {
			return false, nil
		}
		if f == 1 {
			return true, nil
		}
		return false, errField("bool_parsing", alias, "Input should be a valid boolean, unable to interpret input")
	}
	// A float: an integral value goes down the integer path (2.0 is
	// bool_parsing), a fractional one is bool_type.
	if f != math.Trunc(f) {
		return false, errField("bool_type", alias, "Input should be a valid boolean")
	}
	if f == 0 {
		return false, nil
	}
	if f == 1 {
		return true, nil
	}
	return false, errField("bool_parsing", alias, "Input should be a valid boolean, unable to interpret input")
}

func coerceStr(raw any, alias string) (string, *FieldError) {
	if s, ok := raw.(string); ok {
		return s, nil
	}
	return "", errField("string_type", alias, "Input should be a valid string")
}

func coerceOptStr(raw any, alias string) (*string, *FieldError) {
	if raw == nil {
		return nil, nil
	}
	if s, ok := raw.(string); ok {
		return &s, nil
	}
	return nil, errField("string_type", alias, "Input should be a valid string")
}

func coerceLiteral(raw any, spec fieldSpec) (string, *FieldError) {
	if s, ok := raw.(string); ok {
		for _, choice := range spec.choices {
			if s == choice {
				return s, nil
			}
		}
	}
	return "", errField("literal_error", spec.alias, "Input should be "+literalChoicesMessage(spec.choices))
}

// literalChoicesMessage renders pydantic's choice list: 'a', 'b' or 'c'.
func literalChoicesMessage(choices []string) string {
	quoted := make([]string, len(choices))
	for i, c := range choices {
		quoted[i] = "'" + c + "'"
	}
	switch len(quoted) {
	case 0:
		return ""
	case 1:
		return quoted[0]
	case 2:
		return quoted[0] + " or " + quoted[1]
	default:
		return strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
	}
}

// coerceStrList reports EVERY bad element, not just the first: pydantic
// validates list items independently and aggregates one error per item, so
// ["a", 1, None, true] yields three errors at loc ["allowFrom", 1], [.., 2] and
// [.., 3] — and they occupy three consecutive slots in the model's error list,
// which the aggregation-order test in the corpus pins.
func coerceStrList(raw any, alias string) ([]string, []FieldError) {
	switch t := raw.(type) {
	case []string:
		return t, nil
	case []any:
		out := make([]string, 0, len(t))
		var errs []FieldError
		for i, item := range t {
			s, ok := item.(string)
			if !ok {
				errs = append(errs, FieldError{
					Type: "string_type",
					Loc:  []string{alias, strconv.Itoa(i)},
					Msg:  "Input should be a valid string",
				})
				continue
			}
			out = append(out, s)
		}
		if len(errs) > 0 {
			return nil, errs
		}
		return out, nil
	}
	return nil, []FieldError{*errField("list_type", alias, "Input should be a valid list")}
}

func coerceInt(raw any, spec fieldSpec) (int, *FieldError) {
	var value int
	switch t := raw.(type) {
	case bool:
		// bool is an int subclass in Python and pydantic accepts it: `true`
		// for webhook_listen_port becomes 1.
		if t {
			value = 1
		}
	case string:
		v, ok := intFromString(t)
		if !ok {
			return 0, errField("int_parsing", spec.alias, "Input should be a valid integer, unable to parse string as an integer")
		}
		value = int(v)
	default:
		kind, f, ok := classifyNumber(raw)
		if !ok {
			return 0, errField("int_type", spec.alias, "Input should be a valid integer")
		}
		if kind == numFloat && f != math.Trunc(f) {
			return 0, errField("int_from_float", spec.alias, "Input should be a valid integer, got a number with a fractional part")
		}
		value = int(f)
	}
	if fe := checkBounds(float64(value), spec); fe != nil {
		return 0, fe
	}
	return value, nil
}

func coerceFloat(raw any, spec fieldSpec) (float64, *FieldError) {
	var value float64
	switch t := raw.(type) {
	case bool:
		if t {
			value = 1
		}
	case string:
		v, ok := floatFromString(t)
		if !ok {
			return 0, errField("float_parsing", spec.alias, "Input should be a valid number, unable to parse string as a number")
		}
		value = v
	default:
		_, f, ok := classifyNumber(raw)
		if !ok {
			return 0, errField("float_type", spec.alias, "Input should be a valid number")
		}
		value = f
	}
	if fe := checkBounds(value, spec); fe != nil {
		return 0, fe
	}
	return value, nil
}

func checkBounds(value float64, spec fieldSpec) *FieldError {
	if spec.ge != "" {
		limit, _ := strconv.ParseFloat(spec.ge, 64)
		if !(value >= limit) {
			return errField("greater_than_equal", spec.alias, "Input should be greater than or equal to "+spec.ge)
		}
	}
	if spec.le != "" {
		limit, _ := strconv.ParseFloat(spec.le, 64)
		if !(value <= limit) {
			return errField("less_than_equal", spec.alias, "Input should be less than or equal to "+spec.le)
		}
	}
	return nil
}

func errField(typ, alias, msg string) *FieldError {
	return &FieldError{Type: typ, Loc: []string{alias}, Msg: msg}
}

// numTrim trims a numeric string the way pydantic-core's Rust parsers do.
//
// This is deliberately NOT textutil.PyStrip. pydantic-core uses Rust's
// `str::trim`, i.e. the Unicode White_Space property, which excludes
// U+001C..U+001F — whereas Python's str.strip() (used by the two model
// validators in the same class) includes them. The reference really does treat
// "\u001c8080" as an int_parsing error while "\u001c/hook" is an acceptable
// webhook_path, and the differential corpus pins both.
func numTrim(s string) string { return strings.TrimFunc(s, unicode.IsSpace) }

// intFromString is pydantic-core's lax str→int: an integer literal, else a
// float literal with no fractional part ("8080.0" is accepted, "8080.5" is
// not). Underscores are allowed between digits, as in Python's int().
func intFromString(s string) (int64, bool) {
	t := numTrim(s)
	if t == "" {
		return 0, false
	}
	if v, ok := parseIntegerLiteral(t); ok {
		return v, true
	}
	f, ok := floatFromString(t)
	if !ok || math.IsInf(f, 0) || math.IsNaN(f) || f != math.Trunc(f) {
		return 0, false
	}
	return int64(f), true
}

// parseIntegerLiteral parses [+-]?digits with underscores allowed only BETWEEN
// digits, which is Python's int() rule.
func parseIntegerLiteral(s string) (int64, bool) {
	body := s
	negative := false
	if len(body) > 0 && (body[0] == '+' || body[0] == '-') {
		negative = body[0] == '-'
		body = body[1:]
	}
	if body == "" {
		return 0, false
	}
	var digits strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c >= '0' && c <= '9':
			digits.WriteByte(c)
		case c == '_':
			// Only between digits.
			if i == 0 || i == len(body)-1 || body[i-1] == '_' || body[i+1] == '_' {
				return 0, false
			}
			if body[i-1] < '0' || body[i-1] > '9' || body[i+1] < '0' || body[i+1] > '9' {
				return 0, false
			}
		default:
			return 0, false
		}
	}
	v, err := strconv.ParseInt(digits.String(), 10, 64)
	if err != nil {
		return 0, false
	}
	if negative {
		v = -v
	}
	return v, true
}

// floatFromString is Python's float() for the forms a configuration can hold.
//
// Go's strconv.ParseFloat would also accept hexadecimal floating point
// ("0x1p-2"), which Python's float() rejects, so a "0x"/"0X" prefix is refused
// explicitly. Underscores between digits are accepted, as in Python.
func floatFromString(s string) (float64, bool) {
	t := numTrim(s)
	if t == "" {
		return 0, false
	}
	lower := strings.ToLower(t)
	sign := 1.0
	body := lower
	if strings.HasPrefix(body, "+") {
		body = body[1:]
	} else if strings.HasPrefix(body, "-") {
		sign = -1
		body = body[1:]
	}
	switch body {
	case "inf", "infinity":
		return math.Inf(int(sign)), true
	case "nan":
		return math.NaN(), true
	}
	if strings.HasPrefix(lower, "0x") || strings.Contains(lower, "0x") {
		return 0, false
	}
	if strings.ContainsRune(t, '_') {
		stripped, ok := stripNumericUnderscores(t)
		if !ok {
			return 0, false
		}
		t = stripped
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// stripNumericUnderscores removes underscores that sit between digits, which is
// the only place Python's float() tolerates them.
func stripNumericUnderscores(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '_' {
			b.WriteByte(c)
			continue
		}
		if i == 0 || i == len(s)-1 || !isASCIIDigitByte(s[i-1]) || !isASCIIDigitByte(s[i+1]) {
			return "", false
		}
	}
	return b.String(), true
}

func isASCIIDigitByte(c byte) bool { return c >= '0' && c <= '9' }

// ---------------------------------------------------------------------------
// The two model validators
// ---------------------------------------------------------------------------

// webhookPathMustStartWithSlash is the field validator at runtime.py:447-453.
//
// `value.strip() or "/telegram"` uses PYTHON's strip, so U+001C..U+001F and
// NBSP are removed while U+200B is not; the corpus covers both directions.
func webhookPathMustStartWithSlash(value string) string {
	value = textutil.PyStrip(value)
	if value == "" {
		return "/telegram"
	}
	return value
}

// validateWebhookConfig is the model validator at runtime.py:455-467.
//
// It only runs when mode is "webhook" AND every field validated. The messages
// are the reference's ValueError texts verbatim.
//
// urlparse is NOT wrapped in a try in the reference, so a ValueError from
// urllib — "Invalid IPv6 URL", the NFKC netloc guard, an invalid bracketed IP —
// escapes the model validator and pydantic turns it into a value_error carrying
// urllib's own message. url.go reproduces those messages, and the corpus pins
// each one.
func (c *Config) validateWebhookConfig() error {
	if c.Mode != "webhook" {
		return nil
	}
	url := textutil.PyStrip(c.WebhookURL)
	if url == "" {
		return errWebhookURLRequired
	}
	scheme, netloc, err := pyURLSplit(url)
	if err != nil {
		return err
	}
	if scheme != "https" || netloc == "" {
		return errWebhookURLNotHTTPS
	}
	secret := textutil.PyStrip(c.WebhookSecretToken)
	if secret == "" {
		return errWebhookSecretRequired
	}
	if utf8.RuneCountInString(secret) > 256 || !isWebhookSecretCharset(secret) {
		return errWebhookSecretCharset
	}
	return nil
}

type configError string

func (e configError) Error() string { return string(e) }

const (
	errWebhookURLRequired    = configError("webhook_url is required when Telegram mode is webhook")
	errWebhookURLNotHTTPS    = configError("webhook_url must be a public HTTPS URL")
	errWebhookSecretRequired = configError("webhook_secret_token is required when Telegram mode is webhook")
	errWebhookSecretCharset  = configError("webhook_secret_token must be 1-256 characters using only A-Z, a-z, 0-9, _ and -")
)

// isWebhookSecretCharset is `re.match(r"^[A-Za-z0-9_-]+$", secret) is not None`.
// The secret is non-empty by the time this runs, and the pattern is anchored, so
// this is a plain character-set test.
func isWebhookSecretCharset(secret string) bool {
	for _, r := range secret {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
