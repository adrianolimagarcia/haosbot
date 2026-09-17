package config

// Tests for api.publicBaseUrl, the port-only public base URL the A2A agent card
// is built from.
//
// The reference has no A2A endpoint and therefore no such setting, so this key
// is a deliberate divergence and none of the values below can be pinned against
// the differential corpus. What IS pinned against the reference is the
// constraint that made this the only possible home for the key: the root of
// Config is a BaseSettings with extra="forbid" (verified by executing upstream
// 1bb712d3: a root-level key raises extra_forbidden), while an unknown key
// inside `api` is accepted and ignored. TestPublicBaseURLIsNestedNotRoot below
// holds that line, so a future move of the key to the root cannot pass quietly.

import (
	"encoding/json"
	"strings"
	"testing"
)

// quoteJSON renders s as a JSON string literal.
func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestPublicBaseURLIsUnsetByDefault pins the no-regression half of the feature:
// with the key absent the loaded config carries no public base URL at all, so
// the agent card falls back to the effective bind address exactly as it did
// before the key existed.
func TestPublicBaseURLIsUnsetByDefault(t *testing.T) {
	t.Setenv("TZ", "UTC")

	cases := []struct {
		name string
		body string
	}{
		{"empty document", `{}`},
		{"api section without the key", `{"api":{"host":"127.0.0.1","port":8900}}`},
		{"explicit null", `{"api":{"publicBaseUrl":null}}`},
		{"explicit empty string", `{"api":{"publicBaseUrl":""}}`},
		{"whitespace only", `{"api":{"publicBaseUrl":"   "}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustLoad(t, tc.body)
			if cfg.API.PublicBaseURL != "" {
				t.Errorf("api.publicBaseUrl = %q, want empty", cfg.API.PublicBaseURL)
			}
			// The fallback the card uses must be untouched by any of these.
			if cfg.API.Host != "127.0.0.1" || cfg.API.Port != 8900 {
				t.Errorf("bind address = %s:%d, want 127.0.0.1:8900", cfg.API.Host, cfg.API.Port)
			}
		})
	}
}

// TestPublicBaseURLAcceptedForms pins the values that must load, including the
// three the joining rule in internal/a2a/handler.go has to get right: a bare
// host, a trailing slash, and a path component.
func TestPublicBaseURLAcceptedForms(t *testing.T) {
	t.Setenv("TZ", "UTC")

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare host", "https://example.com", "https://example.com"},
		{"trailing slash", "https://example.com/", "https://example.com/"},
		{"path component", "https://example.com/base", "https://example.com/base"},
		{"path with trailing slash", "https://example.com/base/", "https://example.com/base/"},
		{"http", "http://example.com", "http://example.com"},
		{"explicit port", "http://127.0.0.1:8900", "http://127.0.0.1:8900"},
		{"https on a non-default port", "https://example.com:8443/base", "https://example.com:8443/base"},
		{"uppercase scheme and host", "HTTPS://EXAMPLE.COM", "HTTPS://EXAMPLE.COM"},
		{"surrounding whitespace is stripped", "  https://example.com  ", "https://example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"api":{"publicBaseUrl":` + quoteJSON(tc.in) + `}}`
			cfg := mustLoad(t, body)
			if cfg.API.PublicBaseURL != tc.want {
				t.Errorf("api.publicBaseUrl = %q, want %q", cfg.API.PublicBaseURL, tc.want)
			}
		})
	}
}

// TestPublicBaseURLRejectedForms pins the validation. Each case asserts the
// issue LOCATION and the message the operator actually sees, because
// errors.go FriendlyMessage rewrites a custom validator's `value_error` text
// into a generic sentence — a `value_error` here would leave the operator with
// "Value does not satisfy this setting's requirements." and no idea which rule
// was broken.
func TestPublicBaseURLRejectedForms(t *testing.T) {
	t.Setenv("TZ", "UTC")

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"relative path", "/agent", "Must be a valid URL, relative URL without a base."},
		{"no scheme", "example.com", "Must be a valid URL, relative URL without a base."},
		{"wrong scheme", "ftp://example.com", "URL scheme should be 'http' or 'https'."},
		{"ws scheme", "ws://example.com", "URL scheme should be 'http' or 'https'."},
		{"empty host", "http://", "Must be a valid URL, empty host."},
		{"empty host with port", "https://:80", "Must be a valid URL, empty host."},
		{"query string", "https://example.com?x=1", "Must be a valid URL, it must not contain a query string."},
		{"empty query delimiter", "https://example.com?", "Must be a valid URL, it must not contain a query string."},
		{"fragment", "https://example.com#f", "Must be a valid URL, it must not contain a fragment."},
		{"empty fragment delimiter", "https://example.com#", "Must be a valid URL, it must not contain a fragment."},
		{"query after a path", "https://example.com/a?b", "Must be a valid URL, it must not contain a query string."},
		{"unparseable", "http://exa mple.com", "Must be a valid URL."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"api":{"publicBaseUrl":` + quoteJSON(tc.in) + `}}`
			cfg, err := loadFromJSON(t, body)
			if err == nil {
				t.Fatalf("api.publicBaseUrl=%q was accepted (loaded as %q); want a validation error",
					tc.in, cfg.API.PublicBaseURL)
			}
			le := loadError(t, err)
			locs := issueLocations(le)
			if len(locs) != 1 || locs[0] != "api.publicBaseUrl" {
				t.Fatalf("issue locations = %v, want [api.publicBaseUrl]", locs)
			}
			if got := le.Issues[0].Message; got != tc.want {
				t.Errorf("message = %q, want %q", got, tc.want)
			}
			if cfg != nil {
				t.Errorf("expected a nil config on validation failure, got %+v", cfg)
			}
		})
	}
}

// TestPublicBaseURLRejectsEmptyQueryAndFragmentDelimiters is the regression test
// for a defect found while writing this feature, not a hypothetical.
//
// "https://example.com?" and "https://example.com#" look harmless, and Go's
// url.Parse reports them as RawQuery == "" and Fragment == "", so a validator
// that tests those two VALUES accepts them. Both then swallow the path the card
// appends: the advertised URL becomes "https://example.com?/a2a" — whose path is
// "" and whose query is "/a2a" — or "https://example.com#/a2a", which is a
// fragment and never reaches the server as a path at all. A peer following
// either URL would POST to "/" instead of "/a2a".
//
// The percent-encoded spelling is the control: "%3F" is a literal '?' inside a
// path, not a delimiter, and must stay accepted.
func TestPublicBaseURLRejectsEmptyQueryAndFragmentDelimiters(t *testing.T) {
	t.Setenv("TZ", "UTC")

	for _, in := range []string{"https://example.com?", "https://example.com#"} {
		t.Run(in, func(t *testing.T) {
			if _, err := loadFromJSON(t, `{"api":{"publicBaseUrl":`+quoteJSON(in)+`}}`); err == nil {
				t.Errorf("api.publicBaseUrl=%q was accepted; the appended \"/a2a\" would not be a path", in)
			}
		})
	}

	// Control: a percent-encoded delimiter is path content, not a delimiter.
	cfg := mustLoad(t, `{"api":{"publicBaseUrl":"https://example.com/a%3Fb"}}`)
	if cfg.API.PublicBaseURL != "https://example.com/a%3Fb" {
		t.Errorf("percent-encoded '?' must stay accepted, got %q", cfg.API.PublicBaseURL)
	}
}

// TestPublicBaseURLRejectsNonString pins that pydantic's `str` rule applies:
// numbers, booleans and objects are not coerced.
func TestPublicBaseURLRejectsNonString(t *testing.T) {
	t.Setenv("TZ", "UTC")

	for _, in := range []string{`123`, `true`, `{"a":1}`, `["https://example.com"]`} {
		t.Run(in, func(t *testing.T) {
			if _, err := loadFromJSON(t, `{"api":{"publicBaseUrl":`+in+`}}`); err == nil {
				t.Errorf("api.publicBaseUrl=%s was accepted; want a string_type error", in)
			}
		})
	}
}

// TestPublicBaseURLAliasSpellings pins both accepted spellings. to_camel of
// public_base_url is publicBaseUrl, and populate_by_name adds the raw snake_case
// name, so both must bind to the same field.
func TestPublicBaseURLAliasSpellings(t *testing.T) {
	t.Setenv("TZ", "UTC")

	const want = "https://example.com/base"
	for _, key := range []string{"publicBaseUrl", "public_base_url"} {
		t.Run(key, func(t *testing.T) {
			cfg := mustLoad(t, `{"api":{`+quoteJSON(key)+`:`+quoteJSON(want)+`}}`)
			if cfg.API.PublicBaseURL != want {
				t.Errorf("%s: PublicBaseURL = %q, want %q", key, cfg.API.PublicBaseURL, want)
			}
		})
	}
}

// TestPublicBaseURLIsNestedNotRoot holds the interop constraint that decided
// where the key lives.
//
// The reference's root Config is a BaseSettings with extra="forbid", so a
// ROOT-LEVEL key makes a config file written by this port unreadable by the
// reference. A key inside `api` is tolerated because ApiConfig descends from
// `Base`, whose pydantic default is extra="ignore". Both halves are asserted
// here: the nested form loads, the root form is an error.
func TestPublicBaseURLIsNestedNotRoot(t *testing.T) {
	t.Setenv("TZ", "UTC")

	if cfg := mustLoad(t, `{"api":{"publicBaseUrl":"https://example.com"}}`); cfg.API.PublicBaseURL != "https://example.com" {
		t.Fatalf("nested api.publicBaseUrl did not bind: %q", cfg.API.PublicBaseURL)
	}

	_, err := loadFromJSON(t, `{"publicBaseUrl":"https://example.com"}`)
	if err == nil {
		t.Fatal("a ROOT-LEVEL publicBaseUrl must be rejected: the reference's root is extra=\"forbid\", " +
			"so this spelling would make the config file unreadable by the reference")
	}
	le := loadError(t, err)
	if locs := issueLocations(le); len(locs) != 1 || locs[0] != "publicBaseUrl" {
		t.Fatalf("issue locations = %v, want [publicBaseUrl]", locs)
	}
	if got := le.Issues[0].Message; got != "Unknown setting." {
		t.Errorf("message = %q, want %q", got, "Unknown setting.")
	}
}

// TestPublicBaseURLSurvivesSaveAndReload pins the round trip: the key is
// serialized under its camelCase alias and read back unchanged.
func TestPublicBaseURLSurvivesSaveAndReload(t *testing.T) {
	t.Setenv("TZ", "UTC")

	const want = "https://example.com/base"
	cfg := DefaultConfig()
	cfg.API.PublicBaseURL = want

	saved := readSaved(t, cfg)
	if !strings.Contains(saved, `"publicBaseUrl": "`+want+`"`) {
		t.Errorf("saved config does not carry api.publicBaseUrl under its camelCase alias:\n%s", saved)
	}

	target := writeConfig(t, saved)
	reloaded, err := Load(target)
	if err != nil {
		t.Fatalf("reload saved config: %v", err)
	}
	if reloaded.API.PublicBaseURL != want {
		t.Errorf("after save+reload PublicBaseURL = %q, want %q", reloaded.API.PublicBaseURL, want)
	}
}

// TestPublicBaseURLDoesNotDisturbTheBindAddress is the guard against the two
// features fighting: api.publicBaseUrl is the address PEERS use, while
// api.host/api.port are the address the process binds and that
// cmd/haosbot/runtime.go rewrites to the resolved --host/--port. Setting the
// public URL must not change either of them.
func TestPublicBaseURLDoesNotDisturbTheBindAddress(t *testing.T) {
	t.Setenv("TZ", "UTC")

	cfg := mustLoad(t, `{"api":{"host":"127.0.0.1","port":8900,"publicBaseUrl":"https://example.com/agent"}}`)
	if cfg.API.Host != "127.0.0.1" || cfg.API.Port != 8900 {
		t.Fatalf("bind address = %s:%d, want 127.0.0.1:8900 — the public URL must not overwrite it",
			cfg.API.Host, cfg.API.Port)
	}
	if cfg.API.PublicBaseURL != "https://example.com/agent" {
		t.Fatalf("PublicBaseURL = %q", cfg.API.PublicBaseURL)
	}

	// Reproduce what cmdGateway does to the SAME config just before the server
	// starts (cmd/haosbot/runtime.go: the resolved --host/--port are written
	// back into cfg.API). The public URL must survive that write untouched.
	cfg.API.Host = "127.0.0.1"
	cfg.API.Port = 41877
	if cfg.API.PublicBaseURL != "https://example.com/agent" {
		t.Errorf("PublicBaseURL = %q after the effective-bind write-back, want it unchanged",
			cfg.API.PublicBaseURL)
	}
}
