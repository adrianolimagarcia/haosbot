package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
)

const maxWebUIAttachmentBytes = 20 << 20

func (s *Server) handleWebUIAttachment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWebUIAttachmentBytes+(1<<20))
	if err := r.ParseMultipartForm(maxWebUIAttachmentBytes); err != nil {
		http.Error(w, "invalid attachment upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	sessionID := strings.TrimSpace(r.FormValue("sessionId"))
	if sessionID == "" { sessionID = strings.TrimSpace(r.Header.Get("X-HAOS-Session-ID")) }
	if !webSessionIDPattern.MatchString(sessionID) {
		http.Error(w, "invalid sessionId", http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil { http.Error(w, "file is required", http.StatusBadRequest); return }
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxWebUIAttachmentBytes+1))
	if err != nil { http.Error(w, err.Error(), http.StatusBadRequest); return }
	if len(data) == 0 || len(data) > maxWebUIAttachmentBytes {
		http.Error(w, "attachment size is invalid", http.StatusRequestEntityTooLarge)
		return
	}
	mime := http.DetectContentType(data[:min(len(data), 512)])
	if !webUIAttachmentMIMEAllowed(mime) {
		http.Error(w, "unsupported attachment type: "+mime, http.StatusUnsupportedMediaType)
		return
	}
	dir := filepath.Join(s.webUIMediaRoot(), sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil { http.Error(w, err.Error(), 500); return }
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil { http.Error(w, err.Error(), 500); return }
	ext := safeAttachmentExtension(header.Filename, mime)
	path := filepath.Join(dir, hex.EncodeToString(random[:])+ext)
	if err := os.WriteFile(path, data, 0o600); err != nil { http.Error(w, err.Error(), 500); return }
	writeWebUIJSON(w, map[string]any{"path": path, "name": filepath.Base(header.Filename), "mime": mime, "size": len(data)})
}

func webUIAttachmentMIMEAllowed(mime string) bool {
	if strings.HasPrefix(mime, "text/") { return true }
	switch mime {
	case "image/png","image/jpeg","image/gif","image/webp","application/json","application/xml","application/pdf","application/zip","application/octet-stream":
		return true
	}
	return false
}

func safeAttachmentExtension(name, mime string) string {
	ext := strings.ToLower(filepath.Ext(filepath.Base(name)))
	if len(ext) > 12 { ext = "" }
	for _, r := range ext {
		if !(r == '.' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') { ext = ""; break }
	}
	if ext != "" { return ext }
	switch mime { case "image/png": return ".png"; case "image/jpeg": return ".jpg"; case "image/gif": return ".gif"; case "image/webp": return ".webp"; case "application/pdf": return ".pdf"; case "application/json": return ".json" }
	return ".bin"
}

func (s *Server) webUIMediaRoot() string {
	dir := config.DefaultDataDir()
	if p := s.dataDir.Load(); p != nil && strings.TrimSpace(*p) != "" { dir = *p }
	return filepath.Join(dir, "webui-media")
}

func (s *Server) handleWebUISessionContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet { w.Header().Set("Allow", http.MethodGet); http.Error(w, "Method not allowed", 405); return }
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" { http.Error(w, "key is required", 400); return }
	store := s.sessionStore.Load(); if store == nil { http.Error(w, "session store unavailable", 503); return }
	sess, err := store.Open(key); if err != nil { http.Error(w, err.Error(), 500); return }
	messages := sess.Messages()
	chars := 0
	toolEvents := make([]map[string]any,0)
	for _,m := range messages {
		if m.Content.IsText() { chars += len([]rune(m.Content.Text)) } else { for _,b := range m.Content.Blocks { chars += len([]rune(b.Text)) } }
		for _,call := range m.ToolCalls { toolEvents = append(toolEvents,map[string]any{"type":"tool_call","id":call.ID,"name":call.Name,"timestamp":m.Timestamp}) }
		if m.Role == core.RoleTool { toolEvents = append(toolEvents,map[string]any{"type":"tool_result","id":m.ToolCallID,"name":m.Name,"timestamp":m.Timestamp}) }
	}
	meta := sess.Metadata()
	summary, _ := meta["_last_summary"].(map[string]any)
	writeWebUIJSON(w,map[string]any{
		"key":key,"messages":len(messages),"approx_chars":chars,"approx_tokens":(chars+3)/4,
		"context_window_tokens":s.cfg.Agents.Defaults.ContextWindowTokens,
		"last_archived":sess.LastArchived(),"summary":summary,"activity":toolEvents,
	})
}

func cleanupWebUIMedia(root, sessionID string) error {
	if !webSessionIDPattern.MatchString(sessionID) { return errors.New("invalid session id") }
	return os.RemoveAll(filepath.Join(root,sessionID))
}

var _ = json.Valid
