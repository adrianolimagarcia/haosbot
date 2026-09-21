package config

// Tests for api.webuiAuth, the port-only switch that lets the browser shell's
// /api/ routes run without a bearer token.
//
// Like api.publicBaseUrl this key is a deliberate divergence: the reference has
// no WebUI auth model of its own, so no value here can be pinned against the
// differential corpus. What is pinned is the shape that makes the setting safe —
// a tri-state where "absent" is NOT the same as "false".

import (
	"testing"
)

func TestWebUIAuthIsUnsetByDefault(t *testing.T) {
	t.Setenv("TZ", "UTC")

	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty document", `{}`},
		{"api section without the key", `{"api":{"host":"127.0.0.1","port":8900}}`},
		{"explicit null", `{"api":{"webuiAuth":null}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustLoad(t, tc.body)
			if cfg.API.WebUIAuth != nil {
				t.Errorf("api.webuiAuth = %v, want nil (token still required)", *cfg.API.WebUIAuth)
			}
		})
	}
}

func TestWebUIAuthExplicitValues(t *testing.T) {
	t.Setenv("TZ", "UTC")

	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"disabled", `{"api":{"webuiAuth":false}}`, false},
		{"enabled", `{"api":{"webuiAuth":true}}`, true},
		{"snake case alias", `{"api":{"webui_auth":false}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustLoad(t, tc.body)
			if cfg.API.WebUIAuth == nil {
				t.Fatal("api.webuiAuth = nil, want the explicit value")
			}
			if *cfg.API.WebUIAuth != tc.want {
				t.Errorf("api.webuiAuth = %v, want %v", *cfg.API.WebUIAuth, tc.want)
			}
		})
	}
}

// TestWebUIAuthCoercionMatchesTheOtherBooleans pins the two halves of the type
// check. The strings "true"/"false" ARE accepted and coerced, because that is
// pydantic's lax bool validation and every other boolean in this config behaves
// the same way; diverging here would make one key stricter than its neighbours
// and would show up in the differential corpus. A value pydantic cannot
// interpret is still rejected, and the error location names the key.
func TestWebUIAuthCoercionMatchesTheOtherBooleans(t *testing.T) {
	t.Setenv("TZ", "UTC")

	coerced := mustLoad(t, `{"api":{"webuiAuth":"false"}}`)
	if coerced.API.WebUIAuth == nil || *coerced.API.WebUIAuth {
		t.Errorf(`api.webuiAuth="false" = %v, want the pydantic coercion to false`, coerced.API.WebUIAuth)
	}

	if _, err := loadFromJSON(t, `{"api":{"webuiAuth":"maybe"}}`); err == nil {
		t.Fatal(`api.webuiAuth="maybe" was accepted; it is not a valid boolean`)
	}
}

// TestWebUIAuthIsNestedNotRoot keeps the key inside `api`, the only section the
// reference tolerates an unknown key in. At the root it would raise
// extra_forbidden and make the config unreadable by the reference.
func TestWebUIAuthIsNestedNotRoot(t *testing.T) {
	t.Setenv("TZ", "UTC")

	if _, err := loadFromJSON(t, `{"webuiAuth":false}`); err == nil {
		t.Fatal("a root-level webuiAuth key was accepted; the reference forbids extra root keys")
	}
}
