package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/marketplace"
)

func (s *Server) registerMarketplace(mux *http.ServeMux) {
	mux.HandleFunc("/api/webui/skills/marketplace/trending", s.handleMarketplaceTrending)
	mux.HandleFunc("/api/webui/skills/marketplace/search", s.handleMarketplaceSearch)
	mux.HandleFunc("/api/webui/skills/marketplace/install", s.handleMarketplaceInstall)
}

func (s *Server) marketplaceService() *marketplace.Service {
	workspace := webUIWorkspace(s.cfg)
	return marketplace.NewService(workspace)
}

func (s *Server) handleMarketplaceTrending(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	limit := 12
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	svc := s.marketplaceService()
	res, err := svc.Trending(r.Context(), provider, limit)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "server_error", "marketplace_unavailable", err.Error())
		return
	}

	writeWebUIJSON(w, res)
}

func (s *Server) handleMarketplaceSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 {
		http.Error(w, "query must have at least 2 characters", http.StatusBadRequest)
		return
	}

	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	svc := s.marketplaceService()
	res, err := svc.Search(r.Context(), q, provider, limit)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "server_error", "marketplace_search_failed", err.Error())
		return
	}

	writeWebUIJSON(w, res)
}

func (s *Server) handleMarketplaceInstall(w http.ResponseWriter, r *http.Request) {
	svc := s.marketplaceService()

	switch r.Method {
	case http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
			return
		}

		var req marketplace.InstallRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}

		res, err := svc.Install(r.Context(), req)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "install_failed", "marketplace_install_failed", err.Error())
			return
		}

		w.WriteHeader(http.StatusOK)
		writeWebUIJSON(w, res)

	case http.MethodDelete:
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			http.Error(w, "skill name is required", http.StatusBadRequest)
			return
		}

		provider := strings.TrimSpace(r.URL.Query().Get("provider"))
		if err := svc.Uninstall(r.Context(), name); err != nil {
			writeAPIError(w, http.StatusBadRequest, "uninstall_failed", "marketplace_uninstall_failed", err.Error())
			return
		}

		_ = provider
		w.WriteHeader(http.StatusOK)
		writeWebUIJSON(w, map[string]any{"ok": true, "uninstalled": name})

	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
