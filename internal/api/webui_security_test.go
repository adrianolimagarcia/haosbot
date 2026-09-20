package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
)

// webUIHandler builds the same composition Start uses for the browser shell:
// the WebUI routes wrapped in the control-plane security middleware.
func webUIHandler() http.Handler {
	s := NewServer(config.DefaultConfig(), nil, nil)
	mux := http.NewServeMux()
	s.registerWebUI(mux)
	return s.securityMiddleware(mux)
}

func doRequest(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func getWebUI(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	return doRequest(t, h, req)
}

func TestWebUIIndexIsServed(t *testing.T) {
	h := webUIHandler()
	rr := getWebUI(t, h, "/")

	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if len(body) == 0 {
		t.Fatal("GET / returned an empty body")
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(body, "</html>") {
		t.Fatal("GET / did not return a complete HTML document")
	}
}

func TestWebUIResponseCarriesStrictSecurityHeaders(t *testing.T) {
	h := webUIHandler()
	rr := getWebUI(t, h, "/")

	csp := rr.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("Content-Security-Policy header is missing")
	}

	directives := map[string]string{}
	for _, part := range strings.Split(csp, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, _ := strings.Cut(part, " ")
		directives[strings.ToLower(name)] = strings.TrimSpace(value)
	}

	want := map[string]string{
		"default-src":     "'none'",
		"script-src":      "'self'",
		"style-src":       "'self'",
		"connect-src":     "'self'",
		"manifest-src":    "'self'",
		"worker-src":      "'self'",
		"base-uri":        "'none'",
		"form-action":     "'none'",
		"frame-ancestors": "'none'",
		"object-src":      "'none'",
	}
	for name, value := range want {
		if got, ok := directives[name]; !ok {
			t.Errorf("CSP is missing directive %q (policy: %s)", name, csp)
		} else if got != value {
			t.Errorf("CSP directive %q = %q, want %q", name, got, value)
		}
	}

	for _, forbidden := range []string{"unsafe-inline", "unsafe-eval", "http://", "https://", "*"} {
		if strings.Contains(csp, forbidden) {
			t.Errorf("CSP must not contain %q: %s", forbidden, csp)
		}
	}

	headers := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for name, want := range headers {
		if got := rr.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// Every script/style the page loads must come from this server. A single remote
// <script src> is remote code execution surface in the operator's browser and
// makes the UI unusable offline.
func TestWebUIIndexLoadsNoRemoteCode(t *testing.T) {
	h := webUIHandler()
	body := getWebUI(t, h, "/").Body.String()

	if m := regexp.MustCompile(`(?i)(?:src|href)\s*=\s*"(?:https?:)?//[^"]*"`).FindAllString(body, -1); len(m) > 0 {
		t.Errorf("index.html references remote origins: %v", m)
	}
	for _, host := range []string{"cdn.tailwindcss.com", "cdnjs.cloudflare.com", "cdn.jsdelivr.net", "unpkg.com", "fonts.googleapis.com"} {
		if strings.Contains(body, host) {
			t.Errorf("index.html still references %s", host)
		}
	}
	if strings.Contains(body, "fa-solid") || strings.Contains(body, "fa-regular") {
		t.Error("index.html still uses Font Awesome classes that the removed CDN stylesheet provided")
	}
}

// Model output is attacker-influenceable. It must never be turned into markup
// by the page itself.
func TestWebUIIndexNeverRendersModelOutputAsHTML(t *testing.T) {
	h := webUIHandler()
	body := getWebUI(t, h, "/").Body.String()

	for _, forbidden := range []string{
		"marked.parse(",
		"innerHTML",
		"outerHTML",
		"insertAdjacentHTML",
		"document.write(",
		"eval(",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("index.html still contains %q, which can turn untrusted model output into live markup", forbidden)
		}
	}

	// A strict script-src 'self' policy blocks inline event handlers, so the
	// page must not rely on them.
	if m := regexp.MustCompile(`\son[a-z]+\s*=\s*"`).FindAllString(body, -1); len(m) > 0 {
		t.Errorf("index.html relies on inline event handlers blocked by the CSP: %v", m)
	}
}

// The strict CSP is only safe to ship if the page does not need anything the
// policy forbids: no inline <script>, no inline <style>, no style attributes,
// and no remote subresources. This is the "does the UI still work" half of the
// CSP change.
func TestWebUIIndexNeedsNothingTheCSPForbids(t *testing.T) {
	h := webUIHandler()
	body := getWebUI(t, h, "/").Body.String()

	for _, tag := range regexp.MustCompile(`(?i)<script[^>]*>`).FindAllString(body, -1) {
		if !strings.Contains(strings.ToLower(tag), "src=") {
			t.Errorf("index.html has an inline <script> block (%s), which script-src 'self' blocks", tag)
		}
	}
	if strings.Contains(strings.ToLower(body), "<style") {
		t.Errorf("index.html has an inline <style> block, which style-src 'self' blocks")
	}
	if m := regexp.MustCompile(`(?i)\sstyle\s*=\s*"`).FindAllString(body, -1); len(m) > 0 {
		t.Errorf("index.html has %d style attributes, which style-src 'self' blocks", len(m))
	}
	if !strings.Contains(body, `src="/webui/app.js"`) {
		t.Error("index.html does not load /webui/app.js, so its UI wiring would be missing")
	}
}

func TestWebUIAssetsAreServedByTheApplication(t *testing.T) {
	h := webUIHandler()
	body := getWebUI(t, h, "/").Body.String()

	refs := regexp.MustCompile(`(?:src|href)="(/[^"#]*)"`).FindAllStringSubmatch(body, -1)
	if len(refs) == 0 {
		t.Fatal("index.html references no local assets at all")
	}
	for _, ref := range refs {
		rr := getWebUI(t, h, ref[1])
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s referenced by index.html = %d, want 200", ref[1], rr.Code)
			continue
		}
		if rr.Body.Len() == 0 {
			t.Errorf("GET %s returned an empty body", ref[1])
		}
		if rr.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s is missing X-Content-Type-Options: nosniff", ref[1])
		}
	}
}

// None of the served browser code may turn a string into executable code, or
// reintroduce the removed client-side Markdown parser.
func TestWebUIAssetsAvoidDangerousDOMAPIs(t *testing.T) {
	h := webUIHandler()

	for _, path := range []string{"/", "/webui/app.js", "/webui/control.js", "/webui-hardening.js", "/sw.js"} {
		body := getWebUI(t, h, path).Body.String()
		for _, forbidden := range []string{
			"marked.parse(",
			"eval(",
			"new Function(",
			"document.write(",
			"insertAdjacentHTML",
			"setTimeout(\"",
			"javascript:",
		} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s contains %q", path, forbidden)
			}
		}
	}

	// The only innerHTML assignment left is the one fed by the server-side
	// renderer, and it must not be reachable from raw text directly.
	hardening := getWebUI(t, h, "/webui-hardening.js").Body.String()
	if n := strings.Count(hardening, "innerHTML"); n != 1 {
		t.Errorf("hardening.js has %d innerHTML assignments, want exactly the renderer-fed one", n)
	}
	if !strings.Contains(hardening, "assistantBody.textContent = text;") {
		t.Error("hardening.js should render untrusted text inertly before the server renderer answers")
	}
}

// allowlistedTags is exactly the markup renderSafeMarkdown may emit.
var allowlistedTags = map[string]bool{
	"p": true, "br": true, "strong": true, "em": true, "code": true, "pre": true,
	"ul": true, "ol": true, "li": true, "blockquote": true, "a": true, "hr": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
}

var tagPattern = regexp.MustCompile(`</?([a-zA-Z][a-zA-Z0-9]*)[^>]*>`)
var hrefPattern = regexp.MustCompile(`(?i)<a\s[^>]*href="([^"]*)"`)

// assertInertMarkup fails unless html contains only allowlisted tags, no raw
// angle brackets outside tags, and no dangerous URL scheme in any href.
func assertInertMarkup(t *testing.T, label, html string) {
	t.Helper()

	for _, m := range tagPattern.FindAllStringSubmatch(html, -1) {
		if !allowlistedTags[strings.ToLower(m[1])] {
			t.Errorf("%s: emitted disallowed tag <%s>: %s", label, m[1], html)
		}
	}
	if stripped := tagPattern.ReplaceAllString(html, ""); strings.ContainsAny(stripped, "<>") {
		t.Errorf("%s: unescaped angle bracket outside a tag: %s", label, html)
	}
	for _, m := range hrefPattern.FindAllStringSubmatch(html, -1) {
		scheme, _, found := strings.Cut(m[1], ":")
		if !found || strings.Contains(scheme, "/") {
			continue // no scheme, or a relative URL such as /foo:bar
		}
		switch strings.ToLower(scheme) {
		case "http", "https", "mailto":
		default:
			t.Errorf("%s: href uses disallowed scheme %q: %s", label, scheme, html)
		}
	}
}

func TestWebUIRenderEndpointNeutralisesAttackPayloads(t *testing.T) {
	h := webUIHandler()

	payloads := []string{
		`<script>alert(1)</script>`,
		`<img src=x onerror=alert(1)>`,
		`[click](javascript:alert(1))`,
		`[click](JaVaScRiPt:alert(1))`,
		"[click](java\tscript:alert(1))",
		`[click](data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==)`,
		`<iframe src="https://evil.example/"></iframe>`,
		`<svg/onload=alert(1)>`,
		`<object data="data:text/html,<script>alert(1)</script>"></object>`,
		`<embed src="https://evil.example/x.swf">`,
		`<style>body{background:url("https://evil.example/leak")}</style>`,
		`<form action="https://evil.example/"><input name=x></form>`,
		`<a href="javascript:alert(1)">click</a>`,
		"**<script>alert(1)</script>**",
		"```\n</code></pre><script>alert(1)</script>\n```",
		"`<img src=x onerror=alert(1)>`",
		`> <script>alert(1)</script>`,
		`# <script>alert(1)</script>`,
		`![x](https://evil.example/pixel.png)`,
		`"><script>alert(1)</script>`,
		`<math><mtext><table><mglyph><style><!--</style><img src=x onerror=alert(1)>`,
		`<base href="https://evil.example/">`,
	}

	for _, payload := range payloads {
		payload := payload
		t.Run(payload, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"text": payload})
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/webui/render", strings.NewReader(string(body)))
			req.RemoteAddr = "127.0.0.1:54321"
			req.Header.Set("Content-Type", "application/json")
			rr := doRequest(t, h, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("POST /api/webui/render = %d (%s), want 200", rr.Code, rr.Body.String())
			}
			var resp struct {
				HTML string `json:"html"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("invalid JSON response: %v (%s)", err, rr.Body.String())
			}
			assertInertMarkup(t, "payload", resp.HTML)

			for _, forbidden := range []string{"<script", "<iframe", "<object", "<embed", "<style", "<form", "<base", "<math", "<svg", "<mglyph", "<mtext", "<table", "<input", "<img"} {
				if strings.Contains(strings.ToLower(resp.HTML), forbidden) {
					t.Errorf("payload survived as markup (%s): %s", forbidden, resp.HTML)
				}
			}
		})
	}
}

func TestWebUIRenderEndpointStillRendersMarkdown(t *testing.T) {
	h := webUIHandler()

	cases := []struct {
		name  string
		input string
		wants []string
	}{
		{"heading", "# Title", []string{"<h1>Title</h1>"}},
		{"emphasis", "**bold** and *italic*", []string{"<strong>bold</strong>", "<em>italic</em>"}},
		{"inline code", "run `ls -la` now", []string{"<code>ls -la</code>"}},
		{"fenced code", "```go\nfmt.Println(\"hi\")\n```", []string{"<pre><code>", "fmt.Println"}},
		{"list", "- one\n- two", []string{"<ul>", "<li>one</li>", "<li>two</li>"}},
		{"ordered list", "1. one\n2. two", []string{"<ol>", "<li>one</li>"}},
		{"link", "[docs](https://example.com/a?b=1&c=2)", []string{`<a href="https://example.com/a?b=1&amp;c=2"`}},
		{"paragraph escaping", "a < b & c > d", []string{"a &lt; b &amp; c &gt; d"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"text": tc.input})
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/webui/render", strings.NewReader(string(body)))
			req.RemoteAddr = "127.0.0.1:54321"
			req.Header.Set("Content-Type", "application/json")
			rr := doRequest(t, h, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
			}
			var resp struct {
				HTML string `json:"html"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}
			for _, want := range tc.wants {
				if !strings.Contains(resp.HTML, want) {
					t.Errorf("input %q -> %s, want it to contain %q", tc.input, resp.HTML, want)
				}
			}
			assertInertMarkup(t, tc.name, resp.HTML)
		})
	}
}

// The render endpoint lives behind the control-plane boundary like the rest of
// /api/, and must reject malformed input instead of panicking.
func TestWebUIRenderEndpointIsProtectedAndValidated(t *testing.T) {
	h := webUIHandler()

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/webui/render", strings.NewReader(`{"text":"hi"}`))
	req.RemoteAddr = "203.0.113.7:5555"
	req.Header.Set("Content-Type", "application/json")
	if rr := doRequest(t, h, req); rr.Code != http.StatusUnauthorized {
		t.Errorf("remote POST /api/webui/render = %d, want 401", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/webui/render", strings.NewReader(`not json`))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "application/json")
	if rr := doRequest(t, h, req); rr.Code != http.StatusBadRequest {
		t.Errorf("malformed POST /api/webui/render = %d, want 400", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/webui/render", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	if rr := doRequest(t, h, req); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/webui/render = %d, want 405", rr.Code)
	}
}

func TestWebUITokenRotationKeepsCurrentCredentialUntilRestart(t *testing.T) {
	h := webUIHandler()
	hardening := getWebUI(t, h, "/webui-hardening.js").Body.String()
	app := getWebUI(t, h, "/webui/app.js").Body.String()

	for _, want := range []string{
		"haosbot_pending_token",
		"window.setPendingToken(gatewayKey && gatewayKey !== currentGatewayToken ? gatewayKey : '')",
		"window.getPendingToken() || window.getToken()",
	} {
		if !strings.Contains(hardening, want) {
			t.Errorf("hardening.js is missing token-rotation guard %q", want)
		}
	}
	if strings.Contains(hardening, "window.setToken(gatewayKey || window.getToken())") {
		t.Error("hardening.js still activates the new gateway token before restart")
	}
	if !strings.Contains(app, "if (!res.ok)") {
		t.Error("restartAgent does not reject non-2xx restart responses")
	}
	if !strings.Contains(app, "window.promotePendingToken()") {
		t.Error("restartAgent does not promote the pending token after acknowledgement")
	}
}


func TestWebUIFilePreviewRejectsTraversalAndSymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = workspace
	s := NewServer(cfg, nil, nil)

	for _, rel := range []string{"../secret.txt", "../../etc/passwd"} {
		req := httptest.NewRequest(http.MethodGet, "/api/webui/file-preview?path="+rel, nil)
		rr := httptest.NewRecorder()
		s.handleWebUIFilePreview(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("preview traversal %q status=%d want 403", rel, rr.Code)
		}
	}

	link := filepath.Join(workspace, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/webui/file-preview?path=escape/secret.txt", nil)
	rr := httptest.NewRecorder()
	s.handleWebUIFilePreview(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("preview symlink escape status=%d body=%s want 403", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "outside-secret") {
		t.Fatal("preview leaked content outside workspace")
	}
}

func TestWebUISkillWorkspaceCRUD(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = workspace
	s := NewServer(cfg, nil, nil)

	content := "---\nname: demo-skill\ndescription: test\n---\n\n# Demo\n"
	body, _ := json.Marshal(map[string]string{"content": content})
	req := httptest.NewRequest(http.MethodPut, "/api/webui/skill?name=demo-skill", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()
	s.handleWebUISkill(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT skill status=%d body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/webui/skill?name=demo-skill", nil)
	rr = httptest.NewRecorder()
	s.handleWebUISkill(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET skill status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "# Demo") {
		t.Fatalf("GET skill body missing content: %s", rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/webui/skill?name=demo-skill", nil)
	rr = httptest.NewRecorder()
	s.handleWebUISkill(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE skill status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(workspace, "skills", "demo-skill")); !os.IsNotExist(err) {
		t.Fatalf("workspace skill still exists after DELETE: %v", err)
	}
}

func TestWebUISkillRejectsInvalidNames(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	s := NewServer(cfg, nil, nil)
	for _, name := range []string{"../escape", "a/b", "", ".."} {
		req := httptest.NewRequest(http.MethodGet, "/api/webui/skill?name="+name, nil)
		rr := httptest.NewRecorder()
		s.handleWebUISkill(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("skill name %q status=%d want 400", name, rr.Code)
		}
	}
}


func TestWebUISkillRejectsSymlinkedParentEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "skills")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = workspace
	s := NewServer(cfg, nil, nil)

	body, _ := json.Marshal(map[string]string{
		"content": "---\nname: escaped\ndescription: no\n---\n# Escape\n",
	})
	req := httptest.NewRequest(http.MethodPut, "/api/webui/skill?name=escaped", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()
	s.handleWebUISkill(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("symlinked skills root status=%d body=%s want 403", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("skill escaped workspace through symlink, stat err=%v", err)
	}
}
