package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
)

const maxProtectedRequestBodyBytes int64 = 1 << 20 // 1 MiB

// securityMiddleware enforces the control-plane boundary in one place. The
// browser shell and the A2A discovery card remain readable, while mutation and
// model execution surfaces require either a configured bearer token or a
// loopback peer when the gateway is deliberately running without a token.
func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")

		if !protectedPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		if !s.authorizedRequest(r) {
			writeUnauthorized(w)
			return
		}

		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxProtectedRequestBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func protectedPath(path string) bool {
	// /a2a/ is listed explicitly because the A2A endpoint also answers on the
	// trailing-slash form: registering the route without protecting it would let
	// the same handler be reached with no bearer token at all.
	return path == "/a2a" || path == "/a2a/" ||
		strings.HasPrefix(path, "/api/") ||
		strings.HasPrefix(path, "/v1/")
}

func (s *Server) authorizedRequest(r *http.Request) bool {
	if s == nil || s.cfg == nil {
		return false
	}
	if s.cfg.API.APIKey != "" {
		return s.checkAuth(r)
	}
	return requestIsLoopback(r)
}

func requestIsLoopback(r *http.Request) bool {
	if r == nil {
		return false
	}
	host := r.RemoteAddr
	if splitHost, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = splitHost
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": "Invalid or missing API key",
			"type":    "authentication_error",
		},
	})
}

// validateBindAddr prevents a no-token gateway from accidentally becoming a
// remote control plane. A configured API key is required for any non-loopback
// listener, including wildcard listeners such as :8900 and 0.0.0.0:8900.
func (s *Server) validateBindAddr(addr string) error {
	if s == nil || s.cfg == nil {
		return fmt.Errorf("api: nil configuration")
	}
	if s.cfg.API.APIKey != "" {
		return nil
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("api: invalid listen address %q: %w", addr, err)
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("api: refusing unauthenticated non-loopback listener %q; configure api.apiKey or bind to 127.0.0.1/::1", addr)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
