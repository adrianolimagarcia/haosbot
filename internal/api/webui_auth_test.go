package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
)

// boolPtr is a helper for the tri-state setting: nil is not the same as false.
func boolPtr(v bool) *bool { return &v }

// newAuthTestHandler builds the middleware with a configured API key and returns
// it along with the "reached the handler" flag.
func newAuthTestHandler(t *testing.T, webUIAuth *bool) (http.Handler, *bool) {
	t.Helper()

	s := NewServer(config.DefaultConfig(), nil, nil)
	s.cfg.API.APIKey = "secret"
	s.cfg.API.WebUIAuth = webUIAuth

	called := false
	h := s.securityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	return h, &called
}

// serveRemote drives one request from a non-loopback peer, which is the case the
// setting actually changes: without a token, loopback is already allowed when no
// API key is configured.
func serveRemote(h http.Handler, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://example.test"+path, strings.NewReader("{}"))
	req.RemoteAddr = "203.0.113.10:12345"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestWebUIAuthIsRequiredByDefault pins the no-regression half of the setting:
// with the key absent, the browser shell keeps demanding the token exactly as it
// did before the setting existed.
func TestWebUIAuthIsRequiredByDefault(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value *bool
	}{
		{"unset", nil},
		{"explicitly enabled", boolPtr(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, called := newAuthTestHandler(t, tc.value)

			if rr := serveRemote(h, "/api/restart", ""); rr.Code != http.StatusUnauthorized || *called {
				t.Fatalf("/api/restart without a token: status=%d reached-handler=%v, want 401 and not reached", rr.Code, *called)
			}
			if rr := serveRemote(h, "/api/restart", "secret"); rr.Code != http.StatusNoContent || !*called {
				t.Fatalf("/api/restart with a token: status=%d reached-handler=%v, want 204", rr.Code, *called)
			}
		})
	}
}

// TestWebUIAuthCanBeDisabled pins the feature itself: with api.webuiAuth false a
// remote browser reaches the shell's routes with no token at all.
func TestWebUIAuthCanBeDisabled(t *testing.T) {
	h, called := newAuthTestHandler(t, boolPtr(false))

	for _, path := range []string{"/api/restart", "/api/webui/state", "/api/agent/turn"} {
		t.Run(path, func(t *testing.T) {
			*called = false
			if rr := serveRemote(h, path, ""); rr.Code != http.StatusNoContent || !*called {
				t.Fatalf("%s without a token: status=%d reached-handler=%v, want it to reach the handler", path, rr.Code, *called)
			}
		})
	}
}

// TestDisablingWebUIAuthLeavesTheMachineFacingAPIsProtected is the guard that
// matters most. Turning the browser shell's token off must not quietly open the
// A2A endpoint or the OpenAI-compatible API: those are called by peers and
// programs, not by the page, and /a2a in particular runs the full agent.
func TestDisablingWebUIAuthLeavesTheMachineFacingAPIsProtected(t *testing.T) {
	h, called := newAuthTestHandler(t, boolPtr(false))

	for _, path := range []string{
		"/a2a",
		"/a2a/",
		"/v1/models",
		"/v1/chat/completions",
	} {
		t.Run(path, func(t *testing.T) {
			*called = false
			rr := serveRemote(h, path, "")
			if rr.Code != http.StatusUnauthorized || *called {
				t.Fatalf("%s without a token while webuiAuth is false: status=%d reached-handler=%v, want 401 and not reached",
					path, rr.Code, *called)
			}

			*called = false
			if rr := serveRemote(h, path, "secret"); rr.Code != http.StatusNoContent || !*called {
				t.Fatalf("%s with a token: status=%d reached-handler=%v, want 204", path, rr.Code, *called)
			}
		})
	}
}

// TestDisablingWebUIAuthKeepsTheBodyLimit makes sure the exemption skips the
// token check only: the request size cap is a separate protection and must
// survive the change.
func TestDisablingWebUIAuthKeepsTheBodyLimit(t *testing.T) {
	s := NewServer(config.DefaultConfig(), nil, nil)
	s.cfg.API.APIKey = "secret"
	s.cfg.API.WebUIAuth = boolPtr(false)

	var readErr error
	h := s.securityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// MaxBytesReader only reports the overflow once a read crosses the cap,
		// so the whole body has to be consumed for the limit to be observable.
		_, readErr = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))

	oversized := strings.Repeat("x", int(maxProtectedRequestBodyBytes)+1)
	req := httptest.NewRequest(http.MethodPost, "http://example.test/api/webui/session", strings.NewReader(oversized))
	req.RemoteAddr = "203.0.113.10:12345"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if readErr == nil {
		t.Fatal("the exempted route accepted a body larger than the cap")
	}
}

// TestWebUIPathClassification pins the split the exemption relies on, so a later
// route added under a different prefix cannot silently change which APIs the
// setting affects.
func TestWebUIPathClassification(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/api/restart", true},
		{"/api/webui/state", true},
		{"/api/", true},
		{"/a2a", false},
		{"/a2a/", false},
		{"/v1/models", false},
		{"/health", false},
		{"/apix/thing", false},
	} {
		if got := webUIPath(tc.path); got != tc.want {
			t.Errorf("webUIPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
