package api

import (
	"context"
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

func TestMarketplaceAPIEndpoints(t *testing.T) {
	ws := t.TempDir()
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace: ws,
			},
		},
	}
	s := NewServer(cfg, nil, nil)
	mux := http.NewServeMux()
	s.registerMarketplace(mux)

	// 1. Trending endpoint
	req := httptest.NewRequest(http.MethodGet, "/api/webui/skills/marketplace/trending?provider=skillhub&limit=2", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("trending status = %d want 200", rec.Code)
	}
	var trendRes marketplace.TrendingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &trendRes); err != nil {
		t.Fatalf("unmarshal trending: %v", err)
	}

	// 2. Search endpoint with too short query
	req = httptest.NewRequest(http.MethodGet, "/api/webui/skills/marketplace/search?q=a", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("search short query status = %d want 400", rec.Code)
	}

	// 3. Mock installation
	// Create manual skill in workspace
	skillDir := filepath.Join(ws, "skills", "api-test-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 4. Uninstall endpoint
	req = httptest.NewRequest(http.MethodDelete, "/api/webui/skills/marketplace/install?name=api-test-skill", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("uninstall status = %d want 200", rec.Code)
	}

	// Verify uninstalled
	if _, err := os.Stat(skillDir); !os.IsNotExist(err) {
		t.Fatalf("expected skillDir to be deleted")
	}
	_ = context.Background
	_ = strings.Clone
}
