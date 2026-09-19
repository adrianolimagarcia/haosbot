package api

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"net/http"
)

// The WebUI is fully self-hosted: every script and stylesheet is embedded in the
// binary and served from this server, so the page needs no CDN (no remote code
// execution surface in the operator's browser, and the UI works offline).
//
//go:embed webui/index.html
var indexHTML []byte

//go:embed webui/hardening.js
var hardeningJS []byte

//go:embed webui/app.js
var appJS []byte

//go:embed webui/control.js
var controlJS []byte

//go:embed webui/skills_marketplace.js
var skillsMarketplaceJS []byte

//go:embed webui/enhancements.js
var enhancementsJS []byte

//go:embed webui/app.css
var appCSS []byte

//go:embed webui/tailwind.css
var tailwindCSS []byte

//go:embed webui/haosbot_mark.png
var haosbotMarkPNG []byte

//go:embed webui/manifest.webmanifest
var manifestWebmanifest []byte

//go:embed webui/sw.js
var serviceWorkerJS []byte

// webUIContentSecurityPolicy is the policy sent with the browser shell.
//
// It is deliberately strict: no 'unsafe-inline', no 'unsafe-eval' and no remote
// origins, so injected markup cannot run script or load anything from a third
// party. The page satisfies it because index.html has no inline <script>, no
// inline <style> and no inline event handlers (they are served from
// /webui/app.js, /webui/tailwind.css and /webui/app.css, and every interaction
// goes through data-action attributes), and because it only talks to this
// origin. img-src allows same-origin and data: images only, which keeps
// attacker-influenced content from beaconing out through an <img>.
const webUIContentSecurityPolicy = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"manifest-src 'self'; " +
	"worker-src 'self'; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'; " +
	"object-src 'none'"

// maxRenderTextBytes bounds the input of the Markdown renderer. The renderer is
// linear in the input size, but a hard cap keeps a single request cheap.
const maxRenderTextBytes = 128 << 10

func (s *Server) registerWebUI(mux *http.ServeMux) {
	// Stateful browser traffic has an explicit endpoint instead of sharing the
	// OpenAI-compatible route's historical fixed webui_session.
	s.registerAgentTurn(mux)
	s.registerAgentTurnStream(mux)
	s.registerWebUIRender(mux)
	s.registerWebUIData(mux)

	serveAsset := func(path, contentType string, body []byte) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			_, _ = w.Write(body)
		})
	}
	serveAsset("/webui-hardening.js", "application/javascript; charset=utf-8", hardeningJS)
	serveAsset("/webui/app.js", "application/javascript; charset=utf-8", appJS)
	serveAsset("/webui/control.js", "application/javascript; charset=utf-8", controlJS)
	serveAsset("/webui/skills-marketplace.js", "application/javascript; charset=utf-8", skillsMarketplaceJS)
	serveAsset("/webui/enhancements.js", "application/javascript; charset=utf-8", enhancementsJS)
	serveAsset("/webui/app.css", "text/css; charset=utf-8", appCSS)
	serveAsset("/webui/tailwind.css", "text/css; charset=utf-8", tailwindCSS)
	serveAsset("/brand/haosbot_mark.png", "image/png", haosbotMarkPNG)
	serveAsset("/manifest.webmanifest", "application/manifest+json; charset=utf-8", manifestWebmanifest)
	serveAsset("/sw.js", "application/javascript; charset=utf-8", serviceWorkerJS)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", webUIContentSecurityPolicy)
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")

		// Keep the legacy UI source intact for now, but load the security layer
		// after it so vulnerable global functions are replaced before user input.
		page := bytes.Replace(indexHTML, []byte("</body>"), []byte("<script src=\"/webui-hardening.js\"></script>\n<script src=\"/webui/enhancements.js\"></script>\n<script src=\"/webui/skills-marketplace.js\"></script>\n</body>"), 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(page)
	})
}

// registerWebUIRender exposes renderSafeMarkdown to the browser shell.
//
// Rendering model output is a security boundary, so it happens on the trusted
// side: the client never parses untrusted text into markup itself. The route
// lives under /api/ and therefore inherits the control-plane boundary in
// securityMiddleware (bearer token, or loopback when no token is configured).
func (s *Server) registerWebUIRender(mux *http.ServeMux) {
	mux.HandleFunc("/api/webui/render", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Text string `json:"text"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "Invalid JSON request", http.StatusBadRequest)
			return
		}
		if len(req.Text) > maxRenderTextBytes {
			http.Error(w, "text exceeds the render limit", http.StatusRequestEntityTooLarge)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"html": renderSafeMarkdown(req.Text),
		})
	})
}
