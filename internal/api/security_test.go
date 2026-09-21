package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
)

func TestValidateBindAddrWithoutAPIKey(t *testing.T) {
	s := NewServer(config.DefaultConfig(), nil, nil)
	s.cfg.API.APIKey = ""

	for _, addr := range []string{"127.0.0.1:8900", "[::1]:8900", "localhost:8900"} {
		if err := s.validateBindAddr(addr); err != nil {
			t.Fatalf("validateBindAddr(%q) unexpected error: %v", addr, err)
		}
	}

	for _, addr := range []string{"0.0.0.0:8900", ":8900", "192.168.1.10:8900"} {
		if err := s.validateBindAddr(addr); err == nil {
			t.Fatalf("validateBindAddr(%q) expected rejection", addr)
		}
	}
}

func TestValidateBindAddrWithAPIKeyAllowsRemoteListener(t *testing.T) {
	s := NewServer(config.DefaultConfig(), nil, nil)
	s.cfg.API.APIKey = "secret"

	if err := s.validateBindAddr("0.0.0.0:8900"); err != nil {
		t.Fatalf("configured auth should allow remote listener: %v", err)
	}
}

func TestSecurityMiddlewareRequiresBearerWhenConfigured(t *testing.T) {
	s := NewServer(config.DefaultConfig(), nil, nil)
	s.cfg.API.APIKey = "secret"

	called := false
	h := s.securityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, "http://example.test/api/restart", nil)
	req.RemoteAddr = "203.0.113.10:12345"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized || called {
		t.Fatalf("missing token: status=%d called=%v", rr.Code, called)
	}

	called = false
	req = httptest.NewRequest(http.MethodPost, "http://example.test/api/restart", nil)
	req.RemoteAddr = "203.0.113.10:12345"
	req.Header.Set("Authorization", "Bearer secret")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent || !called {
		t.Fatalf("valid token: status=%d called=%v", rr.Code, called)
	}
}

func TestSecurityMiddlewareNoKeyIsLoopbackOnly(t *testing.T) {
	s := NewServer(config.DefaultConfig(), nil, nil)
	s.cfg.API.APIKey = ""

	called := false
	h := s.securityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	remote := httptest.NewRequest(http.MethodPost, "http://example.test/v1/chat/completions", nil)
	remote.RemoteAddr = "203.0.113.10:12345"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, remote)
	if rr.Code != http.StatusUnauthorized || called {
		t.Fatalf("remote no-key request: status=%d called=%v", rr.Code, called)
	}

	called = false
	local := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/chat/completions", nil)
	local.RemoteAddr = "127.0.0.1:12345"
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, local)
	if rr.Code != http.StatusNoContent || !called {
		t.Fatalf("loopback no-key request: status=%d called=%v", rr.Code, called)
	}
}

func TestSecurityMiddlewareLeavesHealthPublic(t *testing.T) {
	s := NewServer(config.DefaultConfig(), nil, nil)
	s.cfg.API.APIKey = "secret"

	called := false
	h := s.securityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "http://example.test/health", nil)
	req.RemoteAddr = "203.0.113.10:12345"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !called {
		t.Fatalf("health should remain public: status=%d called=%v", rr.Code, called)
	}
}

// Regression guard for the trailing-slash A2A route: the JSON-RPC endpoint also
// answers on /a2a/ (the official TCK always requests "<interface-url>/"), so
// registering that path without listing it in protectedPath would expose the
// full agent — shell execution included — with no bearer token at all.
func TestSecurityMiddlewareProtectsTheA2ATrailingSlashForm(t *testing.T) {
	s := NewServer(config.DefaultConfig(), nil, nil)
	s.cfg.API.APIKey = "secret"

	called := false
	h := s.securityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, path := range []string{"/a2a", "/a2a/"} {
		t.Run(path, func(t *testing.T) {
			called = false
			req := httptest.NewRequest(http.MethodPost, "http://example.test"+path, nil)
			req.RemoteAddr = "203.0.113.10:12345"
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized || called {
				t.Fatalf("%s without a token: status=%d reached-handler=%v, want 401 and not reached", path, rr.Code, called)
			}

			called = false
			req = httptest.NewRequest(http.MethodPost, "http://example.test"+path, nil)
			req.RemoteAddr = "203.0.113.10:12345"
			req.Header.Set("Authorization", "Bearer secret")
			rr = httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent || !called {
				t.Fatalf("%s with a valid token: status=%d reached-handler=%v", path, rr.Code, called)
			}
		})
	}
}
