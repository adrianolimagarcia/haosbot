package telegram

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// Port of nanobot/channels/telegram/validation.py together with the shared
// helpers it imports from nanobot/channels/validation.py and
// nanobot/config/loader.py:resolve_env_refs.
//
// The reference module is 165 lines; everything it calls is reproduced here so
// the package has no dependency on an unported settings surface. The one thing
// that is NOT reproduced is the surrounding validate_channel_config() driver in
// nanobot/channels/validation.py, which loads the persisted config, merges WebUI
// form values and dispatches to the channel validator. Its effect on THIS
// function is limited to the shape of `values`, which the caller supplies.

// ValidateTimeoutSeconds is _TIMEOUT_SECONDS (validation.py:20).
const ValidateTimeoutSeconds = 4.0

// ValidationContext is ChannelValidationContext, the second argument of
// validate().
//
// The reference names the parameter `_context` and never reads it: Telegram's
// validator does no local-service probing, so the flag has no effect here. It is
// still carried because the signature is part of the contract and because a
// future check may need it — a silently dropped parameter would be worse.
type ValidationContext struct {
	// AllowLocalServiceAccess mirrors tools.webui_allow_local_service_access.
	AllowLocalServiceAccess bool
}

// Check is one entry of the payload's `checks` list.
//
// Field order matches the reference dict literal, which is also the JSON order:
// id, label, status, then the optional message and action_url. A check with no
// message omits the key entirely rather than writing "".
type Check struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
	ActionURL string `json:"action_url,omitempty"`
}

// Check statuses.
const (
	StatusPass    = "pass"
	StatusFail    = "fail"
	StatusWarn    = "warn"
	StatusSkipped = "skipped"
)

// Setup statuses.
const (
	SetupConnected   = "connected"
	SetupConfigured  = "configured"
	SetupNeedsSetup  = "needs_setup"
	SetupInvalid     = "invalid"
	SetupUnsupported = "unsupported"
)

// Payload is the dict returned by validate(). Every key the reference always
// writes is present, including empty maps and slices, so a JSON round-trip is
// identical.
type Payload struct {
	Name            string         `json:"name"`
	Status          string         `json:"status"`
	Checks          []Check        `json:"checks"`
	Identity        map[string]any `json:"identity"`
	MissingFields   []string       `json:"missing_fields"`
	CanEnable       bool           `json:"can_enable"`
	RequiresRestart bool           `json:"requires_restart"`
	CheckedAt       string         `json:"checked_at"`
	Message         string         `json:"message"`
}

// ---------------------------------------------------------------------------
// Injectable transport
// ---------------------------------------------------------------------------

// GetMeFunc performs the getMe request the way validation.py's _get_me does.
//
// The reference reaches the network through the module-level function, which is
// why the differential harness can monkeypatch it; this is the same seam. proxy
// is "" when no proxy is configured.
type GetMeFunc func(token, proxy string) (map[string]any, error)

// HTTPStatusError mirrors httpx.HTTPStatusError: the only thing validate() reads
// is the status code.
type HTTPStatusError struct {
	StatusCode int
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("telegram: getMe returned HTTP %d", e.StatusCode)
}

// TransportError mirrors httpx.TransportError: a connection-level failure, as
// opposed to a server that answered.
type TransportError struct {
	Err error
}

func (e *TransportError) Error() string {
	if e.Err == nil {
		return "telegram: transport error"
	}
	return "telegram: transport error: " + e.Err.Error()
}

func (e *TransportError) Unwrap() error { return e.Err }

// GetMe is the production _get_me (validation.py:36-46).
//
// LIMITATION (stdlib-only constraint, reported): the reference builds an
// httpx.Client with `proxy=proxy`, and the channel's own dependency list
// installs socksio/python-socks so that socks5 and socks5h proxies work. Go's
// standard library supports only http, https and socks5 (via
// http.Transport.Proxy with a socks5 URL) — and socks5h (remote DNS) is NOT
// expressible. A socks5h proxy therefore fails here with a TransportError, which
// validate() turns into a warn check rather than an incorrect success. The
// PROXY VALIDATOR still accepts all four schemes, exactly as the reference does.
func GetMe(token, proxy string) (map[string]any, error) {
	if proxy != "" && !strings.Contains(proxy, "://") {
		proxy = "http://" + proxy
	}
	transport := &http.Transport{}
	if proxy != "" {
		// The reference builds the client with trust_env=False as soon as an
		// explicit proxy is given, so NO_PROXY and the *_PROXY variables are
		// ignored and the configured proxy wins unconditionally. ProxyURL does
		// exactly that.
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return nil, &TransportError{Err: err}
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	} else {
		// With NO explicit proxy the reference leaves httpx's trust_env at its
		// default of True, so HTTP_PROXY / HTTPS_PROXY / NO_PROXY are honoured
		// from the environment. ProxyFromEnvironment is the closest stdlib
		// equivalent. Two documented gaps remain:
		//
		//   - ALL_PROXY is read by httpx (its mount list covers "http",
		//     "https" and "all") but NOT by Go's ProxyFromEnvironment.
		//   - ProxyFromEnvironment resolves the environment ONCE per process
		//     (sync.Once), so a variable exported after the first call is not
		//     picked up. httpx re-reads it for every client.
		//
		// Note the consequence either way: the bot token travels in the URL
		// PATH, so an environment-named proxy sees it. That is the reference's
		// behaviour too, not an artefact of this port.
		transport.Proxy = http.ProxyFromEnvironment
	}
	client := &http.Client{
		Timeout:   time.Duration(ValidateTimeoutSeconds * float64(time.Second)),
		Transport: transport,
	}
	response, err := client.Get("https://api.telegram.org/bot" + token + "/getMe")
	if err != nil {
		return nil, &TransportError{Err: err}
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		// httpx's raise_for_status() fires on 4xx and 5xx.
		return nil, &HTTPStatusError{StatusCode: response.StatusCode}
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, &TransportError{Err: err}
	}
	return decodeJSONObject(body)
}

// decodeJSONObject is `data if isinstance(data, dict) else {}`: a JSON array,
// string or number decodes to an empty object rather than an error.
func decodeJSONObject(body []byte) (map[string]any, error) {
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, err
	}
	if obj, ok := decoded.(map[string]any); ok {
		return obj, nil
	}
	return map[string]any{}, nil
}

// ---------------------------------------------------------------------------
// validate
// ---------------------------------------------------------------------------

// ValidateChannel is validate() called the way the reference's caller calls it:
// with the real network transport.
//
// It exists so the settings surface does not have to know about the GetMeFunc
// seam, which exists only so the differential harness can replay recorded
// scenarios without touching api.telegram.org.
func ValidateChannel(values map[string]any, ctx ValidationContext) Payload {
	return Validate(values, ctx, nil)
}

// Validate is validate() (telegram/validation.py:49-162).
//
// getMe is the injectable seam; pass nil to use the real network client. The
// flow is a chain of early returns and every branch below is reachable in the
// differential corpus:
//
//   - a token or proxy whose ${VAR} references are unresolvable short-circuits
//     BEFORE any format or network check;
//   - an invalid proxy format short-circuits before the token format check;
//   - only a token that passes the format regex is ever sent to Telegram.
func Validate(values map[string]any, _ ValidationContext, getMe GetMeFunc) Payload {
	if getMe == nil {
		getMe = GetMe
	}
	checks, missing := requiredChecks("telegram", values)

	rawToken := stringValue(values["token"])
	rawProxy := stringValue(values["proxy"])
	token := stringValue(ResolveEnvRefs(rawToken))
	proxy := stringValue(ResolveEnvRefs(rawProxy))

	if rawToken != "" && token == "" {
		checks = append(checks, newCheck(
			"token_env", "Token environment variable", StatusFail,
			"Set every environment variable referenced by the bot token."))
	}
	if rawProxy != "" && proxy == "" {
		checks = append(checks, newCheck(
			"proxy_env", "Proxy environment variable", StatusFail,
			"Set every environment variable referenced by the network proxy."))
	}
	if (rawToken != "" && token == "") || (rawProxy != "" && proxy == "") {
		return statusFromChecks("telegram", checks, missing, nil)
	}
	if proxy != "" && !ProxyURLIsValid(proxy) {
		checks = append(checks, newCheck(
			"proxy_format", "Network proxy", StatusFail,
			"Enter a valid HTTP or SOCKS proxy."))
		return statusFromChecks("telegram", checks, missing, nil)
	}
	if token != "" {
		if !tokenFormatRe.MatchString(token) {
			checks = append(checks, newCheck(
				"token_format", "Token format", StatusFail,
				"Telegram tokens look like 123456:ABC..."))
		} else {
			checks = append(checks, newCheck(
				"token_format", "Token format", StatusPass,
				"Looks like a BotFather token."))
			data, err := getMe(token, proxy)
			switch {
			case err == nil:
				if truthyValue(data["ok"]) {
					if result, ok := data["result"].(map[string]any); ok {
						// `bot.get("id") or ""` has TWO operands: when the id is
						// missing or zero the empty string wins, so the account
						// is "" and the identity entry is dropped entirely.
						identity := map[string]any{
							"name":    firstTruthy(result["username"], result["first_name"]),
							"account": pyStr(firstTruthy(result["id"], "")),
						}
						checks = append(checks, newCheck(
							"get_me", "Bot identity", StatusPass,
							"Telegram accepted the bot token."))
						return newPayload("telegram", SetupConnected, checks,
							identity, missing, nil)
					}
				}
				checks = append(checks, newCheck(
					"get_me", "Bot identity", StatusFail,
					messageFromResponse(data, "Telegram rejected the token.")))
			case isHTTPStatusError(err):
				var statusErr *HTTPStatusError
				if !errors.As(err, &statusErr) { // unreachable: the guard matched
					checks = append(checks, newCheck(
						"get_me", "Bot identity", StatusWarn,
						"Could not verify Telegram now. Try again later."))
					break
				}
				statusCode := statusErr.StatusCode
				rejected := statusCode == 400 || statusCode == 401 || statusCode == 403 || statusCode == 404
				message := fmt.Sprintf("Telegram could not verify the token: HTTP %d.", statusCode)
				status := StatusWarn
				if rejected {
					message = fmt.Sprintf("Telegram rejected the token: HTTP %d.", statusCode)
					status = StatusFail
				}
				checks = append(checks, newCheck("get_me", "Bot identity", status, message))
			case isTransportError(err):
				id, label, message := "get_me", "Bot identity", "Could not reach Telegram now. Try again later."
				if proxy != "" {
					id, label = "proxy_connection", "Network proxy"
					message = "Could not reach Telegram through the network proxy."
				}
				checks = append(checks, newCheck(id, label, StatusWarn, message))
			default:
				checks = append(checks, newCheck(
					"get_me", "Bot identity", StatusWarn,
					"Could not verify Telegram now. Try again later."))
			}
		}
	}
	return statusFromChecks("telegram", checks, missing, nil)
}

// tokenFormatRe is `^\d+:[A-Za-z0-9_-]{20,}$` (validation.py:86).
//
// RE2 REWORK: `\d` in a Python str pattern is Unicode decimal digits, so
// "١٢٣:AAAA..." is a match for the reference. Go's `\d` is ASCII-only, so the
// class is written out as `\p{Nd}`. The corpus pins a non-ASCII digit case.
var tokenFormatRe = regexp.MustCompile(`^\p{Nd}+:[A-Za-z0-9_-]{20,}$`)

func isHTTPStatusError(err error) bool {
	var target *HTTPStatusError
	return errors.As(err, &target)
}

func isTransportError(err error) bool {
	var target *TransportError
	return errors.As(err, &target)
}

// ---------------------------------------------------------------------------
// Shared payload helpers (nanobot/channels/validation.py)
// ---------------------------------------------------------------------------

// requiredChecks is _required_checks (channels/validation.py:144-166) for
// Telegram's spec, whose only requirement is the simple field "token".
//
// The generic version also handles composite requirements
// (_composite_requirement_checks), but Telegram declares none, so that path
// cannot be reached and is not reproduced. `_get` is a plain dotted dict lookup
// — it does NOT apply the camelCase aliases, which is why a config written with
// "token" only would be reported as missing if the key were spelled differently.
func requiredChecks(name string, values map[string]any) ([]Check, []string) {
	checks := []Check{}
	missing := []string{}
	spec := LookupSetupSpec(name)
	if spec == nil {
		return checks, missing
	}
	for _, field := range spec.SimpleRequiredFields() {
		value := lookupDotted(values, field)
		if field == "consentGranted" {
			if !truthyValue(value) {
				missing = append(missing, field)
			}
			continue
		}
		if stringValue(value) != "" {
			checks = append(checks, newCheck("field:"+field, fieldLabel(field), StatusPass, "Configured."))
		} else {
			missing = append(missing, field)
			checks = append(checks, newCheck("field:"+field, fieldLabel(field), StatusFail, "Required."))
		}
	}
	return checks, missing
}

// statusFromChecks is _status_from_checks (channels/validation.py:219-232).
func statusFromChecks(name string, checks []Check, missing []string, identity map[string]any) Payload {
	if len(missing) > 0 {
		return newPayload(name, SetupNeedsSetup, checks, identity, missing, boolPtr(false))
	}
	for _, c := range checks {
		if c.Status == StatusFail {
			return newPayload(name, SetupInvalid, checks, identity, missing, boolPtr(false))
		}
	}
	for _, c := range checks {
		if c.Status == StatusWarn || c.Status == StatusSkipped {
			return newPayload(name, SetupConfigured, checks, identity, missing, nil)
		}
	}
	return newPayload(name, SetupConnected, checks, identity, missing, nil)
}

// newPayload is _payload (channels/validation.py:235-257).
//
// `can_enable` defaults to "status is not one of the three failure statuses and
// nothing is missing", and the identity map DROPS falsy values — so a bot whose
// getMe result has no username and no first_name reports an empty identity
// rather than {"name": null}.
func newPayload(name, status string, checks []Check, identity map[string]any, missing []string, canEnable *bool) Payload {
	if checks == nil {
		checks = []Check{}
	}
	if missing == nil {
		missing = []string{}
	}
	filtered := map[string]any{}
	for key, value := range identity {
		// PLAIN Python truthiness, not the `_truthy` string set: `if value` in
		// _payload keeps any non-empty string, so an account of "42" survives.
		if pyTruthy(value) {
			filtered[key] = value
		}
	}
	enable := false
	if canEnable != nil {
		enable = *canEnable
	} else {
		switch status {
		case SetupNeedsSetup, SetupInvalid, SetupUnsupported:
		default:
			enable = len(missing) == 0
		}
	}
	return Payload{
		Name:            name,
		Status:          status,
		Checks:          checks,
		Identity:        filtered,
		MissingFields:   missing,
		CanEnable:       enable,
		RequiresRestart: false,
		CheckedAt:       pyNowISO(),
		Message:         statusMessage(status),
	}
}

// newCheck is _check (channels/validation.py:260-273).
func newCheck(id, label, status, message string) Check {
	return Check{ID: id, Label: label, Status: status, Message: message}
}

// statusMessage is _status_message (channels/validation.py:329-336).
func statusMessage(status string) string {
	switch status {
	case SetupConnected:
		return "Connection verified."
	case SetupConfigured:
		return "Configuration is present, but full verification was not possible."
	case SetupNeedsSetup:
		return "Required setup is missing."
	case SetupInvalid:
		return "Configuration was checked and looks invalid."
	case SetupUnsupported:
		return "This channel is not supported by the WebUI setup checker."
	}
	return "Channel checked."
}

// messageFromResponse is _message_from_response (channels/validation.py:339-341).
//
// `or` is Python truthiness, so an empty description falls through to the next
// key and a falsy 0 or "" in `error` does too.
func messageFromResponse(data map[string]any, fallback string) string {
	for _, key := range []string{"error", "description", "message"} {
		// `or` is plain truthiness, so a falsy 0 or "" falls through to the next
		// key rather than being stringified.
		if value, ok := data[key]; ok && pyTruthy(value) {
			return pyStr(value)
		}
	}
	return fallback
}

// fieldLabelRe is `([a-z])([A-Z])` from _label (channels/validation.py:325).
var fieldLabelRe = regexp.MustCompile(`([a-z])([A-Z])`)

// fieldLabel is _label (channels/validation.py:324-326).
func fieldLabel(field string) string {
	words := fieldLabelRe.ReplaceAllString(field, "$1 $2")
	words = strings.ReplaceAll(words, ".", " ")
	words = strings.ReplaceAll(words, "_", " ")
	if words == "" {
		return words
	}
	runes := []rune(words)
	return strings.ToUpper(string(runes[0])) + string(runes[1:])
}

// lookupDotted is _get (channels/validation.py:288-294).
func lookupDotted(values map[string]any, field string) any {
	var target any = values
	for _, part := range strings.Split(field, ".") {
		obj, ok := target.(map[string]any)
		if !ok {
			return nil
		}
		target = obj[part]
	}
	return target
}

// stringValue is string_value / _str (channels/validation.py:297-302).
//
// A non-string is stringified with Python's str() and THEN stripped — the strip
// applies to the rendered form, not to the original.
func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return textutil.PyStrip(s)
	}
	return textutil.PyStrip(pyStr(value))
}

// truthyValue is _truthy (channels/validation.py:314-317) for the values a JSON
// payload can hold. A real bool short-circuits; everything else is stringified
// and compared against the accepted set.
func truthyValue(value any) bool {
	if b, ok := value.(bool); ok {
		return b
	}
	if value == nil {
		return false
	}
	switch strings.ToLower(stringValue(value)) {
	case "1", "true", "yes", "on", "granted":
		return true
	}
	return false
}

// firstTruthy is Python's `a or b or ...`: the first truthy argument, or the
// last one when none is truthy.
func firstTruthy(values ...any) any {
	for i, value := range values {
		// Python's `a or b` uses plain truthiness, so a non-empty username wins
		// over first_name even though it is not one of _truthy's tokens.
		if pyTruthy(value) {
			return value
		}
		if i == len(values)-1 {
			return value
		}
	}
	return nil
}

// pyStr is Python's str() for the value shapes a decoded JSON object can hold.
//
// DIVERGENCE (documented): Python renders containers with repr(), so a list
// becomes "[1, 2]" (space after the comma) and a dict "{'a': 1}" (single quotes,
// space after the colon); Go's fmt verbs differ. Only scalars are reachable from
// Telegram's validator — `str(bot.get("id"))` and `str(error)` — and those match
// exactly, including "True"/"False" for bools.
func pyStr(value any) string {
	switch t := value.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return pyFloatStr(t)
	case json.Number:
		return t.String()
	case []any:
		parts := make([]string, len(t))
		for i, item := range t {
			parts[i] = pyReprValue(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(t))
		for key := range t {
			keys = append(keys, key)
		}
		sortStrings(keys)
		parts := make([]string, len(keys))
		for i, key := range keys {
			parts[i] = pyReprValue(key) + ": " + pyReprValue(t[key])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(value)
}

// pyReprValue is repr() for the scalar shapes above.
func pyReprValue(value any) string {
	if s, ok := value.(string); ok {
		return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
	}
	return pyStr(value)
}

// pyFloatStr is Python's repr() for a float, which is the shortest string that
// round-trips, with ".0" appended for integral values.
func pyFloatStr(f float64) string {
	if f == float64(int64(f)) && f < 1e16 && f > -1e16 {
		return strconv.FormatInt(int64(f), 10) + ".0"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// pyNowISO is datetime.now(UTC).isoformat(): microsecond precision with a
// "+00:00" offset rather than "Z".
func pyNowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
}

// ---------------------------------------------------------------------------
// resolve_env_refs (nanobot/config/loader.py:219-241)
// ---------------------------------------------------------------------------

// envRefRe is _ENV_REF_PATTERN (loader.py:193).
var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// envLookup is the process environment, injectable so tests can install an
// exact environment without mutating the real one.
var envLookup = os.LookupEnv

// ResolveEnvRefs is resolve_env_refs (loader.py:226-241).
//
// ALL-OR-NOTHING: if ANY referenced variable is unset the whole value collapses
// to "", rather than leaving the reference in place or substituting the ones
// that do exist. That is what makes a half-configured token report
// "token_env" instead of being sent to Telegram.
func ResolveEnvRefs(value string) string {
	names := envRefRe.FindAllStringSubmatch(value, -1)
	for _, match := range names {
		if _, ok := envLookup(match[1]); !ok {
			return ""
		}
	}
	if len(names) == 0 {
		return value
	}
	return envRefRe.ReplaceAllStringFunc(value, func(match string) string {
		name := envRefRe.FindStringSubmatch(match)[1]
		resolved, _ := envLookup(name)
		return resolved
	})
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func boolPtr(v bool) *bool { return &v }
