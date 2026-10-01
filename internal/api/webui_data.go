package api

import (
	"context"
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

	"github.com/adrianolimagarcia/nanobot-go/internal/channels/registry"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/mcp"
	"github.com/adrianolimagarcia/nanobot-go/internal/mcpruntime"
	"github.com/adrianolimagarcia/nanobot-go/internal/memoryfabric"
	"github.com/adrianolimagarcia/nanobot-go/internal/multiagent"
	"github.com/adrianolimagarcia/nanobot-go/internal/skills"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
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
	mux.HandleFunc("/api/webui/channels/catalog", s.handleWebUIChannelCatalog)
	mux.HandleFunc("/api/webui/channels/", s.handleWebUIChannelValidate)
	mux.HandleFunc("/api/webui/mcp/test", s.handleWebUIMCPTest)
	mux.HandleFunc("/api/webui/state", s.handleWebUIState)
	mux.HandleFunc("/api/webui/session", s.handleWebUISession)
	mux.HandleFunc("/api/webui/session/action", s.handleWebUISessionAction)
	mux.HandleFunc("/api/webui/search", s.handleWebUISearch)
	mux.HandleFunc("/api/webui/memory", s.handleWebUIMemory)
	mux.HandleFunc("/api/webui/memory/admin", s.handleWebUIMemoryAdmin)
	mux.HandleFunc("/api/webui/skill", s.handleWebUISkill)
	mux.HandleFunc("/api/webui/file-preview", s.handleWebUIFilePreview)
	mux.HandleFunc("/api/webui/attachment", s.handleWebUIAttachment)
	mux.HandleFunc("/api/webui/session/context", s.handleWebUISessionContext)
	mux.HandleFunc("/api/webui/temporary", s.handleWebUITemporary)
	mux.HandleFunc("/api/webui/automations", s.handleWebUIAutomations)
	mux.HandleFunc("/api/webui/automation", s.handleWebUIAutomation)
	mux.HandleFunc("/api/webui/automation/run", s.handleWebUIAutomationRun)
	mux.HandleFunc("/api/webui/automation/from-session", s.handleWebUIAutomationFromSession)
	mux.HandleFunc("/api/webui/automation/history", s.handleWebUIAutomationHistory)
	mux.HandleFunc("/api/webui/triggers", s.handleWebUITriggers)
	mux.HandleFunc("/api/webui/trigger", s.handleWebUITrigger)
	mux.HandleFunc("/api/webui/trigger/fire", s.handleWebUITriggerFire)
	mux.HandleFunc("/api/webui/agents", s.handleWebUIAgents)
	mux.HandleFunc("/api/webui/agent-tasks", s.handleWebUIAgentTasks)
	mux.HandleFunc("/api/webui/agent-task", s.handleWebUIAgentTask)
}

func (s *Server) handleWebUIAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	manager := s.multiAgents.Load()
	if manager == nil {
		writeWebUIJSON(w, map[string]any{"enabled": false, "agents": []any{}, "limits": nil})
		return
	}
	writeWebUIJSON(w, map[string]any{
		"enabled": true,
		"agents": manager.Profiles(),
		"limits": manager.Limits(),
	})
}

func (s *Server) handleWebUIAgentTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	manager := s.multiAgents.Load()
	if manager == nil {
		writeWebUIJSON(w, map[string]any{"tasks": []any{}})
		return
	}
	writeWebUIJSON(w, map[string]any{"tasks": manager.List(200)})
}

func (s *Server) handleWebUIAgentTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	manager := s.multiAgents.Load()
	if manager == nil {
		http.Error(w, "multi-agent runtime is disabled", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Action string `json:"action"`
		Agent string `json:"agent"`
		Prompt string `json:"prompt"`
		TaskID string `json:"task_id"`
		TimeoutSeconds int `json:"timeout_seconds"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 256<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "delegate":
		task, err := manager.Delegate(r.Context(), multiagent.DelegateRequest{
			AgentID: req.Agent, Prompt: req.Prompt, Wait: false, Detach: true,
			Timeout: time.Duration(req.TimeoutSeconds) * time.Second,
			RequestedBy: "webui",
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeWebUIJSON(w, map[string]any{"task": task})
	case "cancel":
		task, err := manager.Cancel(strings.TrimSpace(req.TaskID))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeWebUIJSON(w, map[string]any{"task": task})
	default:
		http.Error(w, "unsupported action", http.StatusBadRequest)
	}
}

func (s *Server) handleWebUIMCPTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	cfg := s.cfg
	if saved, err := config.Load(configTargetPath(s.cfg)); err == nil {
		cfg = saved
	}
	server, ok := cfg.Tools.MCPServers[name]
	if name == "" || !ok {
		http.Error(w, "MCP server not found", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	registry := tools.NewRegistry()
	kind := ""
	if server.Type != nil {
		kind = strings.ToLower(strings.TrimSpace(*server.Type))
	}
	if kind == "stdio" || (kind == "" && strings.TrimSpace(server.URL) == "") {
		manager, err := mcp.RegisterConfigured(ctx, registry, map[string]config.MCPServerConfig{name: server})
		if manager != nil {
			defer manager.Close()
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	} else {
		manager := mcpruntime.NewManager(mcpruntime.Options{SSRFWhitelist: cfg.Tools.SSRFWhitelist})
		defer manager.Close()
		if err := manager.LoadAndRegister(ctx, registry, map[string]config.MCPServerConfig{name: server}); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	writeWebUIJSON(w, map[string]any{"name": name, "status": "connected", "tools": registry.Names()})
}

func (s *Server) handleWebUIChannelValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	const prefix = "/api/webui/channels/"
	path := strings.TrimPrefix(r.URL.Path, prefix)
	name, suffix, ok := strings.Cut(path, "/")
	if !ok || suffix != "validate" || strings.TrimSpace(name) == "" || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	if _, exists := registry.Lookup(name); !exists {
		http.NotFound(w, r)
		return
	}
	cfg := s.cfg
	if saved, err := config.Load(configTargetPath(s.cfg)); err == nil {
		cfg = saved
	}
	values, _ := cfg.Channels.Extra[name].(map[string]any)
	if values == nil {
		values = map[string]any{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeWebUIJSON(w, registry.Validate(name, values, registry.ValidationContext{
		AllowLocalServiceAccess: cfg.Tools.WebUIAllowLocalServiceAccess,
	}))
}

// The catalog describes runtime support and form fields from the transport's
// own setup contract. Unsupported entries remain visible but cannot be saved.
func (s *Server) handleWebUIChannelCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	names := []struct{ ID, Name, Description string }{
		{"telegram", "Telegram", "Bot API · polling ou webhook"},
		{"discord", "Discord", "Mensagens e comunidades"},
		{"slack", "Slack", "Mensagens de equipes"},
		{"whatsapp", "WhatsApp Cloud API", "Webhook oficial e mensagens 1:1"},
		{"weixin", "WeChat / Weixin", "Mensageria WeChat"},
		{"feishu", "Feishu / Lark", "Mensagens de equipes"},
		{"dingtalk", "DingTalk", "Colaboração corporativa"},
		{"email", "Email", "Caixa de entrada e envio"},
		{"matrix", "Matrix", "Mensageria federada"},
		{"qq", "QQ via OneBot", "Mensagens QQ por gateway OneBot 11"},
		{"napcat", "Napcat", "Gateway compatível com QQ"},
		{"wecom", "WeCom", "Mensagens corporativas"},
		{"teams", "Microsoft Teams", "Colaboração corporativa"},
		{"mattermost", "Mattermost", "Mensagens de equipes"},
		{"mochat", "Mochat", "Mensagens multiusuário"},
		{"signal", "Signal", "Mensagens privadas"},
		{"linear", "Linear", "Eventos de projetos"},
		{"websocket", "WebSocket", "Integração customizada"},
	}
	entries := make([]map[string]any, 0, len(names))
	for _, n := range names {
		entry := map[string]any{"id": n.ID, "name": n.Name, "description": n.Description, "available": false}
		if manifest, ok := registry.Lookup(n.ID); ok {
			entry["available"] = true
			setup := make(map[string]any, len(manifest.Setup)+1)
			for key, value := range manifest.Setup {
				setup[key] = value
			}
			setup["verifies_connection"] = true
			entry["setup"] = setup
			entry["probe"] = manifest.Probe
			entry["capabilities"] = manifest.Capabilities
		}
		entries = append(entries, entry)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeWebUIJSON(w, map[string]any{"channels": entries})
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
			"scheduler": s.scheduler.Load() != nil,
			"local_triggers": s.triggers.Load() != nil,
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
			matchStart, matchEnd, found := caseInsensitiveSpan(text, query)
			if !found { continue }
			start := snapRuneStart(text, matchStart-80)
			end := snapRuneEnd(text, matchEnd+160)
			results = append(results, map[string]any{
				"key": key, "title": title, "snippet": strings.TrimSpace(text[start:end]),
			})
			break
		}
		if len(results) >= maxWebUISearchResults { break }
	}
	writeWebUIJSON(w, map[string]any{"results": results})
}

func (s *Server) handleWebUIMemoryAdmin(w http.ResponseWriter, r *http.Request) {
	admin := s.memoryAdmin
	if admin == nil {
		http.Error(w, "memory admin unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodGet {
		snapshot, err := admin.Snapshot(r.Context(), 50)
		if err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return }
		writeWebUIJSON(w, snapshot)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Action     string `json:"action"`
		Projection string `json:"projection"`
		JobID      string `json:"job_id"`
		BeforeDays int    `json:"before_days"`
		Scope      string `json:"scope"`
		Owner      string `json:"owner"`
		MaxRecords int    `json:"max_records"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 32<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil { http.Error(w, "invalid request", http.StatusBadRequest); return }
	req.Action = strings.ToLower(strings.TrimSpace(req.Action))
	var result map[string]any
	switch req.Action {
	case "retry_dead":
		if err := admin.RetryDead(r.Context(), strings.TrimSpace(req.Projection), strings.TrimSpace(req.JobID)); err != nil {
			http.Error(w, err.Error(), http.StatusConflict); return
		}
		result = map[string]any{"ok": true, "action": req.Action}
	case "rebuild":
		projection := strings.ToLower(strings.TrimSpace(req.Projection))
		if projection == "" { projection = "all" }
		if err := admin.Rebuild(r.Context(), projection); err != nil {
			http.Error(w, err.Error(), http.StatusConflict); return
		}
		result = map[string]any{"ok": true, "action": req.Action, "projection": projection}
	case "prune":
		if req.BeforeDays <= 0 { req.BeforeDays = 30 }
		if req.BeforeDays > 36500 { http.Error(w, "before_days too large", http.StatusBadRequest); return }
		if req.MaxRecords <= 0 { req.MaxRecords = 1000 }
		var namespace *memoryfabric.Namespace
		if strings.TrimSpace(req.Scope) != "" {
			ns := memoryfabric.Namespace{Scope: req.Scope, Owner: req.Owner}
			if strings.EqualFold(strings.TrimSpace(req.Scope), memoryfabric.ScopeGlobal) && strings.TrimSpace(req.Owner) == "" {
				ns.Owner = memoryfabric.ScopeGlobal
			}
			if _, err := memoryfabric.NormalizeNamespace(ns); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest); return
			}
			namespace = &ns
		}
		deleted, err := admin.Prune(r.Context(), time.Now().Add(-time.Duration(req.BeforeDays)*24*time.Hour), namespace, req.MaxRecords)
		if err != nil { http.Error(w, err.Error(), http.StatusConflict); return }
		result = map[string]any{"ok": true, "action": req.Action, "deleted": deleted}
	case "vacuum":
		if err := admin.Vacuum(r.Context()); err != nil { http.Error(w, err.Error(), http.StatusConflict); return }
		result = map[string]any{"ok": true, "action": req.Action}
	default:
		http.Error(w, "unsupported memory action", http.StatusBadRequest); return
	}
	snapshot, err := admin.Snapshot(r.Context(), 50)
	if err == nil { result["snapshot"] = snapshot }
	writeWebUIJSON(w, result)
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
	if truncated {
		raw = raw[:maxPreview]
		for i := 0; i < 4 && len(raw) > 0 && !utf8.Valid(raw); i++ {
			raw = raw[:len(raw)-1]
		}
	}
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

// caseInsensitiveSpan returns the byte offsets of the first case-insensitive
// match of query inside text.
//
// It compares rune by rune rather than searching a lowercased copy: strings
// .ToLower can change a string's byte length ('İ' U+0130 expands to two runes,
// 'K' U+212A contracts to one), so an offset taken from the lowercased copy
// would point into the middle of a different character in text.
func caseInsensitiveSpan(text, query string) (int, int, bool) {
	if query == "" { return 0, 0, false }
	runes := []rune(text)
	q := []rune(query)
	if len(q) == 0 || len(q) > len(runes) { return 0, 0, false }
	byteAt := make([]int, len(runes)+1)
	offset := 0
	for i, r := range runes {
		byteAt[i] = offset
		offset += utf8.RuneLen(r)
	}
	byteAt[len(runes)] = offset
	for i := 0; i+len(q) <= len(runes); i++ {
		if strings.EqualFold(string(runes[i:i+len(q)]), query) {
			return byteAt[i], byteAt[i+len(q)], true
		}
	}
	return 0, 0, false
}

// snapRuneStart moves offset back to the start of the rune containing it, so a
// slice taken from that offset never begins mid-character.
func snapRuneStart(text string, offset int) int {
	if offset <= 0 { return 0 }
	if offset >= len(text) { return len(text) }
	for offset > 0 && !utf8.RuneStart(text[offset]) { offset-- }
	return offset
}

// snapRuneEnd moves offset forward to just past the rune containing it.
func snapRuneEnd(text string, offset int) int {
	if offset <= 0 { return 0 }
	if offset >= len(text) { return len(text) }
	for offset < len(text) && !utf8.RuneStart(text[offset]) { offset++ }
	return offset
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
