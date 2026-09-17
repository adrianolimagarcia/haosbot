package api

import (
	"bytes"
	_ "embed"
	"net/http"
)

//go:embed webui/index.html
var indexHTML []byte

//go:embed webui/hardening.js
var hardeningJS []byte

func (s *Server) registerWebUI(mux *http.ServeMux) {
	// Stateful browser traffic has an explicit endpoint instead of sharing the
	// OpenAI-compatible route's historical fixed webui_session.
	s.registerAgentTurn(mux)

	mux.HandleFunc("/webui-hardening.js", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(hardeningJS)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		// Keep the legacy UI source intact for now, but load the security layer
		// after it so vulnerable global functions are replaced before user input.
		page := bytes.Replace(indexHTML, []byte("</body>"), []byte("<script src=\"/webui-hardening.js\"></script>\n</body>"), 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(page)
	})
}
