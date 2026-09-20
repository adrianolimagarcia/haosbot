package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/marketplace"
)

func marketplaceTestServer(t *testing.T, allowInstall bool) (*Server, *http.ServeMux, string) {
	t.Helper()
	ws := t.TempDir()
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{Workspace: ws},
		},
	}
	cfg.Tools.WebUIAllowRemotePackageInstall = allowInstall
	s := NewServer(cfg, nil, nil)
	mux := http.NewServeMux()
	s.registerMarketplace(mux)
	return s, mux, ws
}

func TestMarketplaceAPIEndpoints(t *testing.T) {
	_, mux, ws := marketplaceTestServer(t, true)

	// 1. Trending endpoint. The provider name matches no upstream on purpose:
	// the API layer is what is under test here, and a live catalogue call would
	// make the suite depend on third-party availability. Provider parsing and
	// HTTP handling are covered by stub servers in internal/marketplace.
	req := httptest.NewRequest(http.MethodGet, "/api/webui/skills/marketplace/trending?provider=none&limit=2", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("trending status = %d want 200", rec.Code)
	}
	var trendRes marketplace.TrendingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &trendRes); err != nil {
		t.Fatalf("unmarshal trending: %v", err)
	}
	if !trendRes.InstallSupported {
		t.Fatal("trending install_supported=false, want true when installs are enabled")
	}
	if trendRes.Skills == nil {
		t.Fatal("trending skills is null, want an array")
	}

	// 2. Search endpoint with too short query
	req = httptest.NewRequest(http.MethodGet, "/api/webui/skills/marketplace/search?q=a", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("search short query status = %d want 400", rec.Code)
	}

	// 3. Uninstall endpoint on a manually created skill
	skillDir := filepath.Join(ws, "skills", "api-test-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/webui/skills/marketplace/install?name=api-test-skill", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("uninstall status = %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(skillDir); !os.IsNotExist(err) {
		t.Fatal("expected skillDir to be deleted")
	}

	// 4. Method guard
	req = httptest.NewRequest(http.MethodPatch, "/api/webui/skills/marketplace/install", strings.NewReader("{}"))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("patch status = %d want 405", rec.Code)
	}
}

func TestMarketplaceInstallIsForbiddenWhenDisabled(t *testing.T) {
	_, mux, ws := marketplaceTestServer(t, false)

	req := httptest.NewRequest(http.MethodPost, "/api/webui/skills/marketplace/install",
		strings.NewReader(`{"provider":"skillhub","skill_id":"demo"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("install status = %d want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "skills", "demo")); !os.IsNotExist(err) {
		t.Fatal("skill was installed despite the install gate")
	}

	skillDir := filepath.Join(ws, "skills", "keep-me")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/webui/skills/marketplace/install?name=keep-me", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("uninstall status = %d want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(skillDir); err != nil {
		t.Fatalf("skill was removed despite the install gate: %v", err)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/webui/skills/marketplace/trending?provider=none&limit=1", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("trending status = %d want 200", rec.Code)
	}
	var trendRes marketplace.TrendingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &trendRes); err != nil {
		t.Fatal(err)
	}
	if trendRes.InstallSupported {
		t.Fatal("install_supported=true, want false when installs are disabled")
	}
}
