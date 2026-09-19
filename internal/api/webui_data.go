package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/skills"
)

const (
	maxWebUIMemoryBytes    = 512 << 10
	maxWebUISearchSessions = 100
	maxWebUISearchResults  = 50
	maxWebUISkillBytes     = 256 << 10
)

var webUISkillNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

type webUISessionPrefs struct {
	Title    string `json:"title,omitempty"`
	Pinned   bool   `json:"pinned,omitempty"`
	Archived bool   `json:"archived,omitempty"`
}

type webUIPrefs struct {
	Sessions map[string]webUISessionPrefs `json:"sessions"`
}

type webUISessionSummary struct {
	Key       string `json:"key"`
	SessionID string `json:"session_id,omitempty"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updated_at"`
	Messages  int    `json:"messages"`
	Pinned    bool   `json:"pinned"`
	Archived  bool   `json:"archived"`
	Selectable bool  `json:"selectable"`
}

func (s *Server) registerWebUIData(mux *http.ServeMux) {
	mux.HandleFunc("/api/webui/state", s.handleWebUIState)
	mux.HandleFunc("/api/webui/session", s.handleWebUISession)
	mux.HandleFunc("/api/webui/session/action", s.handleWebUISessionAction)
	mux.HandleFunc("/api/webui/search", s.handleWebUISearch)
	mux.HandleFunc("/api/webui/memory", s.handleWebUIMemory)
	mux.HandleFunc("/api/webui/skill", s.handleWebUISkill)
	mux.HandleFunc("/api/webui/file-preview", s.handleWebUIFilePreview)
}

func (s *Server) handleWebUIState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store := s.sessionStore.Load()
	if store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "sessions_unavailable", "Session store is not configured")
		return
	}
	prefs, _ := s.loadWebUIPrefs()
	keys, err := store.List()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "sessions_list_failed", err.Error())
		return
	}
	summaries := make([]webUISessionSummary, 0, len(keys))
	for _, key := range keys {
		sess, err := store.Open(key)
		if err != nil { continue }
		messages := sess.Messages()
		p := prefs.Sessions[key]
		title := strings.TrimSpace(p.Title)
		if title == "" { title = titleFromMessages(messages) }
		sessionID := ""
		selectable := strings.HasPrefix(key, "webui:")
		if selectable { sessionID = strings.TrimPrefix(key, "webui:") }
		summaries = append(summaries, webUISessionSummary{
			Key: key, SessionID: sessionID, Title: title,
			UpdatedAt: sess.UpdatedAt().Format(time.RFC3339),
			Messages: len(messages), Pinned: p.Pinned, Archived: p.Archived,
			Selectable: selectable,
		})
	}
	sort.SliceStable(summaries, func(i, j int) bool {
		if summaries[i].Pinned != summaries[j].Pinned { return summaries[i].Pinned }
		return summaries[i].UpdatedAt > summaries[j].UpdatedAt
	})

	workspace := webUIWorkspace(s.cfg)
	loader := skills.New(workspace)
	skillRows := make([]map[string]any, 0)
	for _, skill := range loader.ListSkills(false) {
		available, reason := loader.GetSkillAvailability(skill.Name)
		req := loader.GetSkillRequirements(skill.Name)
		skillRows = append(skillRows, map[string]any{
			"name": skill.Name, "source": skill.Source, "path": skill.Path,
			"description": loader.GetSkillDescription(skill.Name),
			"available": available, "unavailable_reason": reason,
			"requirements": req,
		})
	}

	redacted, _ := redactedConfig(s.cfg)
	var metrics any
	if registry := s.metrics.Load(); registry != nil { metrics = registry.Snapshot() }
	memoryPath := filepath.Join(workspace, "memory", "MEMORY.md")
	var memoryBytes int64
	if info, err := os.Stat(memoryPath); err == nil { memoryBytes = info.Size() }

	writeWebUIJSON(w, map[string]any{
		"sessions": summaries,
		"skills": skillRows,
		"config": redacted,
		"metrics": metrics,
		"workspace": workspace,
		"memory": map[string]any{"path": memoryPath, "bytes": memoryBytes},
		"capabilities": map[string]any{
			"files": s.cfg.Tools.File.Enable,
			"exec": s.cfg.Tools.Exec.Enable,
			"web": s.cfg.Tools.Web.Enable,
			"memory_search": true,
			"graph_async": true,
			"streaming": true,
		},
	})
}

func (s *Server) handleWebUISession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" { http.Error(w, "key is required", http.StatusBadRequest); return }
	store := s.sessionStore.Load()
	if store == nil { http.Error(w, "session store unavailable", http.StatusServiceUnavailable); return }
	sess, err := store.Open(key)
	if err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return }
	prefs, _ := s.loadWebUIPrefs()
	writeWebUIJSON(w, map[string]any{
		"key": key,
		"session_id": strings.TrimPrefix(key, "webui:"),
		"messages": sess.Messages(),
		"updated_at": sess.UpdatedAt().Format(time.RFC3339),
		"preferences": prefs.Sessions[key],
	})
}

func (s *Server) handleWebUISessionAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Action string `json:"action"`
		Key string `json:"key"`
		Value any `json:"value"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil { http.Error(w, "invalid request", http.StatusBadRequest); return }
	req.Action, req.Key = strings.TrimSpace(req.Action), strings.TrimSpace(req.Key)
	if req.Key == "" { http.Error(w, "key is required", http.StatusBadRequest); return }

	store := s.sessionStore.Load()
	if store == nil { http.Error(w, "session store unavailable", http.StatusServiceUnavailable); return }
	if req.Action == "delete" {
		if err := store.Delete(req.Key); err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return }
		s.webuiMu.Lock()
		prefs, _ := s.loadWebUIPrefsUnlocked()
		delete(prefs.Sessions, req.Key)
		err := s.saveWebUIPrefsUnlocked(prefs)
		s.webuiMu.Unlock()
		if err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return }
		writeWebUIJSON(w, map[string]any{"ok": true})
		return
	}

	s.webuiMu.Lock()
	prefs, _ := s.loadWebUIPrefsUnlocked()
	p := prefs.Sessions[req.Key]
	switch req.Action {
	case "rename":
		title, ok := req.Value.(string)
		if !ok { s.webuiMu.Unlock(); http.Error(w, "value must be a string", http.StatusBadRequest); return }
		p.Title = strings.TrimSpace(title)
	case "pin":
		value, ok := req.Value.(bool)
		if !ok { s.webuiMu.Unlock(); http.Error(w, "value must be boolean", http.StatusBadRequest); return }
		p.Pinned = value
	case "archive":
		value, ok := req.Value.(bool)
		if !ok { s.webuiMu.Unlock(); http.Error(w, "value must be boolean", http.StatusBadRequest); return }
		p.Archived = value
	default:
		s.webuiMu.Unlock()
		http.Error(w, "unsupported action", http.StatusBadRequest)
		return
	}
	prefs.Sessions[req.Key] = p
	err := s.saveWebUIPrefsUnlocked(prefs)
	s.webuiMu.Unlock()
	if err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return }
	writeWebUIJSON(w, map[string]any{"ok": true, "preferences": p})
}

func (s *Server) handleWebUISearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet { http.Error(w, "Method not allowed", http.StatusMethodNotAllowed); return }
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if query == "" { writeWebUIJSON(w, map[string]any{"results": []any{}}); return }
	store := s.sessionStore.Load()
	if store == nil { http.Error(w, "session store unavailable", http.StatusServiceUnavailable); return }
	keys, err := store.List()
	if err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return }
	if len(keys) > maxWebUISearchSessions { keys = keys[:maxWebUISearchSessions] }
	prefs, _ := s.loadWebUIPrefs()
	results := make([]map[string]any, 0)
	for _, key := range keys {
		sess, err := store.Open(key)
		if err != nil { continue }
		title := prefs.Sessions[key].Title
		if title == "" { title = titleFromMessages(sess.Messages()) }
		if strings.Contains(strings.ToLower(title), query) {
			results = append(results, map[string]any{"key": key, "title": title, "snippet": title})
		}
		if len(results) >= maxWebUISearchResults { break }
		for _, message := range sess.Messages() {
			if !message.Content.IsText() { continue }
			text := message.Content.Text
			pos := strings.Index(strings.ToLower(text), query)
			if pos < 0 { continue }
			start := pos - 80; if start < 0 { start = 0 }
			end := pos + len(query) + 160; if end > len(text) { end = len(text) }
			results = append(results, map[string]any{
				"key": key, "title": title, "snippet": strings.TrimSpace(text[start:end]),
			})
			break
		}
		if len(results) >= maxWebUISearchResults { break }
	}
	writeWebUIJSON(w, map[string]any{"results": results})
}

func (s *Server) handleWebUIMemory(w http.ResponseWriter, r *http.Request) {
	workspace := webUIWorkspace(s.cfg)
	path := filepath.Join(workspace, "memory", "MEMORY.md")
	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) { http.Error(w, err.Error(), http.StatusInternalServerError); return }
		writeWebUIJSON(w, map[string]any{"content": string(data), "path": path})
	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebUIMemoryBytes+1))
		if err != nil { http.Error(w, err.Error(), http.StatusBadRequest); return }
		if len(body) > maxWebUIMemoryBytes { http.Error(w, "memory document too large", http.StatusRequestEntityTooLarge); return }
		var req struct { Content string `json:"content"` }
		if err := json.Unmarshal(body, &req); err != nil { http.Error(w, "invalid JSON", http.StatusBadRequest); return }
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { http.Error(w, err.Error(), 500); return }
		if err := writeWebUIAtomic(path, []byte(req.Content), 0o600); err != nil { http.Error(w, err.Error(), 500); return }
		writeWebUIJSON(w, map[string]any{"ok": true, "bytes": len(req.Content)})
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleWebUISkill(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" || !webUISkillNamePattern.MatchString(name) {
		http.Error(w, "valid skill name is required", http.StatusBadRequest)
		return
	}
	workspace := webUIWorkspace(s.cfg)
	loader := skills.New(workspace)
	workspaceDir, pathErr := resolveWebUIWorkspacePath(workspace, filepath.Join("skills", name))
	if pathErr != nil {
		http.Error(w, pathErr.Error(), http.StatusForbidden)
		return
	}
	workspaceFile := filepath.Join(workspaceDir, "SKILL.md")

	switch r.Method {
	case http.MethodGet:
		content, found, err := loader.LoadSkillStrict(name)
		if err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return }
		if !found { http.NotFound(w, r); return }
		source := ""
		path := ""
		for _, item := range loader.ListSkills(false) {
			if item.Name == name { source, path = item.Source, item.Path; break }
		}
		writeWebUIJSON(w, map[string]any{
			"name": name, "content": content, "source": source, "path": path,
			"description": loader.GetSkillDescription(name),
			"requirements": loader.GetSkillRequirements(name),
		})
	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebUISkillBytes+1))
		if err != nil { http.Error(w, err.Error(), http.StatusBadRequest); return }
		if len(body) > maxWebUISkillBytes { http.Error(w, "skill document too large", http.StatusRequestEntityTooLarge); return }
		var req struct { Content string `json:"content"` }
		if err := json.Unmarshal(body, &req); err != nil { http.Error(w, "invalid JSON", http.StatusBadRequest); return }
		if strings.TrimSpace(req.Content) == "" { http.Error(w, "skill content is required", http.StatusBadRequest); return }
		if err := os.MkdirAll(workspaceDir, 0o700); err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return }
		if err := writeWebUIAtomic(workspaceFile, []byte(req.Content), 0o600); err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return }
		writeWebUIJSON(w, map[string]any{"ok": true, "name": name, "source": skills.SourceWorkspace})
	case http.MethodDelete:
		source := ""
		for _, item := range loader.ListSkills(false) {
			if item.Name == name { source = item.Source; break }
		}
		if source != skills.SourceWorkspace {
			http.Error(w, "only workspace skills can be deleted", http.StatusConflict)
			return
		}
		if err := os.RemoveAll(workspaceDir); err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return }
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func resolveWebUIWorkspacePath(workspace, rel string) (string, error) {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	within := func(path string) bool {
		relative, err := filepath.Rel(root, path)
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
	}

	logical, err := filepath.Abs(filepath.Join(root, filepath.Clean(rel)))
	if err != nil {
		return "", err
	}
	if !within(logical) {
		return "", errors.New("path escapes workspace")
	}

	// EvalSymlinks requires the whole path to exist. Walk up until an existing
	// ancestor is found, resolve that ancestor, then append the missing suffix.
	probe := logical
	var suffix []string
	for {
		if _, err := os.Lstat(probe); err == nil {
			resolved, err := filepath.EvalSymlinks(probe)
			if err != nil {
				return "", err
			}
			if !within(resolved) {
				return "", errors.New("symlink escapes workspace")
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			if !within(resolved) {
				return "", errors.New("path escapes workspace")
			}
			return resolved, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}

		parent := filepath.Dir(probe)
		if parent == probe {
			return "", errors.New("workspace path has no existing ancestor")
		}
		suffix = append(suffix, filepath.Base(probe))
		probe = parent
	}
}

func (s *Server) handleWebUIFilePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	if rel == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	workspace := webUIWorkspace(s.cfg)
	target, err := resolveWebUIWorkspacePath(workspace, rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) { http.NotFound(w, r); return }
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		entries, err := os.ReadDir(target)
		if err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return }
		rows := make([]map[string]any, 0, len(entries))
		for _, entry := range entries {
			rows = append(rows, map[string]any{"name": entry.Name(), "dir": entry.IsDir()})
			if len(rows) >= 500 { break }
		}
		writeWebUIJSON(w, map[string]any{"path": rel, "directory": true, "entries": rows})
		return
	}
	const maxPreview = 256 << 10
	f, err := os.Open(target)
	if err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return }
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxPreview+1))
	if err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return }
	truncated := len(raw) > maxPreview
	if truncated { raw = raw[:maxPreview] }
	if !utf8.Valid(raw) {
		writeWebUIJSON(w, map[string]any{
			"path": rel, "directory": false, "binary": true,
			"bytes": info.Size(), "truncated": truncated,
		})
		return
	}
	writeWebUIJSON(w, map[string]any{
		"path": rel, "directory": false, "binary": false,
		"bytes": info.Size(), "truncated": truncated, "content": string(raw),
	})
}

func (s *Server) loadWebUIPrefs() (webUIPrefs, error) {
	s.webuiMu.Lock()
	defer s.webuiMu.Unlock()
	return s.loadWebUIPrefsUnlocked()
}

func (s *Server) loadWebUIPrefsUnlocked() (webUIPrefs, error) {
	out := webUIPrefs{Sessions: map[string]webUISessionPrefs{}}
	data, err := os.ReadFile(s.webUIPrefsPath())
	if errors.Is(err, os.ErrNotExist) { return out, nil }
	if err != nil { return out, err }
	if err := json.Unmarshal(data, &out); err != nil { return webUIPrefs{Sessions: map[string]webUISessionPrefs{}}, nil }
	if out.Sessions == nil { out.Sessions = map[string]webUISessionPrefs{} }
	return out, nil
}

func (s *Server) saveWebUIPrefsUnlocked(prefs webUIPrefs) error {
	data, err := json.MarshalIndent(prefs, "", "  ")
	if err != nil { return err }
	return writeWebUIAtomic(s.webUIPrefsPath(), append(data, '\n'), 0o600)
}

func (s *Server) webUIPrefsPath() string {
	dir := config.DefaultDataDir()
	if p := s.dataDir.Load(); p != nil && strings.TrimSpace(*p) != "" { dir = *p }
	return filepath.Join(dir, "webui-state.json")
}

func webUIWorkspace(cfg *config.Config) string {
	path := strings.TrimSpace(cfg.Agents.Defaults.Workspace)
	if path == "" { path = config.DefaultWorkspace() }
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" { return home }
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func titleFromMessages(messages []core.Message) string {
	for _, message := range messages {
		if message.Role != core.RoleUser || !message.Content.IsText() { continue }
		text := strings.Join(strings.Fields(message.Content.Text), " ")
		if text == "" { continue }
		runes := []rune(text)
		if len(runes) > 52 { return string(runes[:52]) + "…" }
		return text
	}
	return "Novo chat"
}

func writeWebUIAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil { return err }
	f, err := os.CreateTemp(dir, ".webui-*.tmp")
	if err != nil { return err }
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok { _ = os.Remove(tmp) }
	}()
	if err := f.Chmod(perm); err != nil { return err }
	if _, err := f.Write(data); err != nil { return err }
	if err := f.Close(); err != nil { return err }
	if err := os.Rename(tmp, path); err != nil { return err }
	ok = true
	return nil
}

func writeWebUIJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}
