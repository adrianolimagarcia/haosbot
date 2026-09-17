package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// writeConfig writes body to a temp config.json and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

// loadFromJSON loads an inline JSON document through the real Load path.
func loadFromJSON(t *testing.T, body string) (*Config, error) {
	t.Helper()
	return Load(writeConfig(t, body))
}

// mustLoad fails the test when Load returns an error.
func mustLoad(t *testing.T, body string) *Config {
	t.Helper()
	cfg, err := loadFromJSON(t, body)
	if err != nil {
		t.Fatalf("Load(%s) returned error: %v", body, err)
	}
	return cfg
}

// normalizedJSON renders a config with all aliases applied, for structural
// comparison between two input spellings.
func normalizedJSON(t *testing.T, cfg *Config) string {
	t.Helper()
	b, err := marshalNoEscape(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// loadError extracts the structured error from a failed Load.
func loadError(t *testing.T, err error) *LoadError {
	t.Helper()
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("expected *LoadError, got %T: %v", err, err)
	}
	return le
}

// issueLocations renders the locations of a load error, sorted for comparison.
func issueLocations(le *LoadError) []string {
	out := make([]string, 0, len(le.Issues))
	for _, i := range le.Issues {
		out = append(out, i.Location())
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// 1. snake_case and camelCase produce identical normalized values
// ---------------------------------------------------------------------------

// TestSnakeAndCamelAgree loads the same logical configuration written twice — once
// entirely in snake_case, once entirely in camelCase — and requires the
// normalized result to be byte-identical.
func TestSnakeAndCamelAgree(t *testing.T) {
	t.Setenv("TZ", "UTC")

	snake := `{
	  "agents": {"defaults": {
	    "workspace": "/var/tmp/nanobot-test-ws",
	    "model_preset": "fast",
	    "model": "openai/gpt-4o",
	    "provider": "openai",
	    "max_tokens": 4096,
	    "context_window_tokens": 128000,
	    "temperature": 0.25,
	    "fallback_models": ["slow", {"model": "m2", "provider": "p2", "max_tokens": 10}],
	    "max_tool_iterations": 50,
	    "max_concurrent_subagents": 8,
	    "max_tool_result_chars": 9000,
	    "provider_retry_mode": "persistent",
	    "tool_hint_max_length": 60,
	    "reasoning_effort": "high",
	    "timezone": "Asia/Tokyo",
	    "timezone_mode": "manual",
	    "bot_name": "tux",
	    "bot_icon": "X",
	    "unified_session": true,
	    "disabled_skills": ["summarize"],
	    "session_ttl_minutes": 42,
	    "idle_compact_check_interval_seconds": 30,
	    "dream": {"enabled": false, "interval_h": 5, "cron": "0 3 * * *", "model_override": "fast"}
	  }},
	  "channels": {"send_progress": false, "send_tool_hints": false, "show_reasoning": false,
	               "extract_document_text": false, "send_max_retries": 7,
	               "transcription_provider": "whisper", "transcription_language": "pt",
	               "telegram": {"token": "abc"}},
	  "transcription": {"enabled": false, "provider": "groq", "model": "whisper-1",
	                    "language": "en", "max_duration_sec": 300, "max_upload_mb": 50},
	  "providers": {"openai": {"api_key": "k", "api_base": "http://x", "api_type": "responses",
	                           "extra_headers": {"A": "b"}, "extra_query": {"q": "1"},
	                           "proxy": "http://p", "thinking_style": "enable_thinking"},
	                "deepseek": {"api_key": "d"},
	                "bedrock": {"region": "us-east-1", "profile": "prof"},
	                "myproxy": {"api_key": "c", "display_name": "My Proxy"}},
	  "api": {"host": "127.0.0.1", "port": 9000, "timeout": 30.5, "api_key": "secret"},
	  "gateway": {"host": "127.0.0.1", "port": 18000, "restart_mode": "exec",
	              "heartbeat": {"enabled": false, "interval_s": 60}},
	  "tools": {"web": {"enable": false, "proxy": "http://w", "user_agent": "ua",
	                    "search": {"provider": "brave", "api_key": "bk", "base_url": "http://b",
	                               "max_results": 9, "timeout": 11},
	                    "fetch": {"use_jina_reader": false}},
	            "exec": {"enable": false, "timeout": 0, "path_prepend": "/a", "path_append": "/b",
	                     "sandbox": "bwrap", "sandbox_ro_binds": ["/r"], "sandbox_rw_binds": ["/w"],
	                     "allowed_env_keys": ["PATH"], "allow_patterns": ["^ls"], "deny_patterns": ["rm"]},
	            "file": {"enable": false},
	            "cli_apps": {"enable": false, "install_timeout": 100, "run_timeout": 20,
	                         "catalog_ttl_seconds": 120},
	            "my": {"enable": false, "allow_set": true},
	            "image_generation": {"enabled": true, "provider": "openai", "model": "img",
	                                 "default_aspect_ratio": "16:9", "default_image_size": "2K",
	                                 "max_images_per_turn": 8, "save_dir": "gen"},
	            "max_session_messages_per_minute": 12,
	            "restrict_to_workspace": true,
	            "webui_allow_local_service_access": false,
	            "webui_allow_remote_package_install": true,
	            "mcp_servers": {"srv": {"type": "stdio", "auth": "oauth", "command": "npx",
	                                    "args": ["-y"], "env": {"E": "1"}, "cwd": "/c",
	                                    "url": "http://u", "headers": {"H": "1"},
	                                    "tool_timeout": 45, "enabled_tools": ["a"]}},
	            "ssrf_whitelist": ["100.64.0.0/10"]},
	  "model_presets": {"fast": {"model": "openai/gpt-4o-mini", "provider": "openai",
	                             "max_tokens": 100, "context_window_tokens": 1000,
	                             "temperature": 0.7, "reasoning_effort": "low"},
	                    "slow": {"model": "openai/gpt-3.5-turbo", "provider": "openai"}}
	}`

	camel := `{
	  "agents": {"defaults": {
	    "workspace": "/var/tmp/nanobot-test-ws",
	    "modelPreset": "fast",
	    "model": "openai/gpt-4o",
	    "provider": "openai",
	    "maxTokens": 4096,
	    "contextWindowTokens": 128000,
	    "temperature": 0.25,
	    "fallbackModels": ["slow", {"model": "m2", "provider": "p2", "maxTokens": 10}],
	    "maxToolIterations": 50,
	    "maxConcurrentSubagents": 8,
	    "maxToolResultChars": 9000,
	    "providerRetryMode": "persistent",
	    "toolHintMaxLength": 60,
	    "reasoningEffort": "high",
	    "timezone": "Asia/Tokyo",
	    "timezoneMode": "manual",
	    "botName": "tux",
	    "botIcon": "X",
	    "unifiedSession": true,
	    "disabledSkills": ["summarize"],
	    "idleCompactAfterMinutes": 42,
	    "idleCompactCheckIntervalSeconds": 30,
	    "dream": {"enabled": false, "intervalH": 5, "cron": "0 3 * * *", "modelOverride": "fast"}
	  }},
	  "channels": {"sendProgress": false, "sendToolHints": false, "showReasoning": false,
	               "extractDocumentText": false, "sendMaxRetries": 7,
	               "transcriptionProvider": "whisper", "transcriptionLanguage": "pt",
	               "telegram": {"token": "abc"}},
	  "transcription": {"enabled": false, "provider": "groq", "model": "whisper-1",
	                    "language": "en", "maxDurationSec": 300, "maxUploadMb": 50},
	  "providers": {"openai": {"apiKey": "k", "apiBase": "http://x", "apiType": "responses",
	                           "extraHeaders": {"A": "b"}, "extraQuery": {"q": "1"},
	                           "proxy": "http://p", "thinkingStyle": "enable_thinking"},
	                "deepseek": {"apiKey": "d"},
	                "bedrock": {"region": "us-east-1", "profile": "prof"},
	                "myproxy": {"apiKey": "c", "displayName": "My Proxy"}},
	  "api": {"host": "127.0.0.1", "port": 9000, "timeout": 30.5, "apiKey": "secret"},
	  "gateway": {"host": "127.0.0.1", "port": 18000, "restartMode": "exec",
	              "heartbeat": {"enabled": false, "intervalS": 60}},
	  "tools": {"web": {"enable": false, "proxy": "http://w", "userAgent": "ua",
	                    "search": {"provider": "brave", "apiKey": "bk", "baseUrl": "http://b",
	                               "maxResults": 9, "timeout": 11},
	                    "fetch": {"useJinaReader": false}},
	            "exec": {"enable": false, "timeout": 0, "pathPrepend": "/a", "pathAppend": "/b",
	                     "sandbox": "bwrap", "sandboxRoBinds": ["/r"], "sandboxRwBinds": ["/w"],
	                     "allowedEnvKeys": ["PATH"], "allowPatterns": ["^ls"], "denyPatterns": ["rm"]},
	            "file": {"enable": false},
	            "cliApps": {"enable": false, "installTimeout": 100, "runTimeout": 20,
	                        "catalogTtlSeconds": 120},
	            "my": {"enable": false, "allowSet": true},
	            "imageGeneration": {"enabled": true, "provider": "openai", "model": "img",
	                                "defaultAspectRatio": "16:9", "defaultImageSize": "2K",
	                                "maxImagesPerTurn": 8, "saveDir": "gen"},
	            "maxSessionMessagesPerMinute": 12,
	            "restrictToWorkspace": true,
	            "webuiAllowLocalServiceAccess": false,
	            "webuiAllowRemotePackageInstall": true,
	            "mcpServers": {"srv": {"type": "stdio", "auth": "oauth", "command": "npx",
	                                   "args": ["-y"], "env": {"E": "1"}, "cwd": "/c",
	                                   "url": "http://u", "headers": {"H": "1"},
	                                   "toolTimeout": 45, "enabledTools": ["a"]}},
	            "ssrfWhitelist": ["100.64.0.0/10"]},
	  "modelPresets": {"fast": {"model": "openai/gpt-4o-mini", "provider": "openai",
	                            "maxTokens": 100, "contextWindowTokens": 1000,
	                            "temperature": 0.7, "reasoningEffort": "low"},
	                   "slow": {"model": "openai/gpt-3.5-turbo", "provider": "openai"}}
	}`

	fromSnake := normalizedJSON(t, mustLoad(t, snake))
	fromCamel := normalizedJSON(t, mustLoad(t, camel))

	if fromSnake != fromCamel {
		t.Fatalf("snake_case and camelCase configs differ after normalization\nsnake: %s\ncamel: %s",
			fromSnake, fromCamel)
	}
	// Guard against the test silently comparing two default configs.
	if !strings.Contains(fromSnake, `"idleCompactAfterMinutes":42`) {
		t.Fatalf("expected the alias input to take effect, got: %s", fromSnake)
	}
}

// ---------------------------------------------------------------------------
// 2. every declared default
// ---------------------------------------------------------------------------

// TestAgentDefaultsEveryDefault asserts each default value named in the task
// brief, straight from schema.py:119-157.
func TestAgentDefaultsEveryDefault(t *testing.T) {
	t.Setenv("TZ", "UTC")
	d := DefaultConfig().Agents.Defaults

	cases := []struct {
		field string
		got   any
		want  any
	}{
		{"workspace", d.Workspace, "~/.nanobot/workspace"},
		{"model", d.Model, "anthropic/claude-opus-4-5"},
		{"provider", d.Provider, "auto"},
		{"max_tokens", d.MaxTokens, 8192},
		{"context_window_tokens", d.ContextWindowTokens, 200000},
		{"temperature", float64(d.Temperature), 0.1},
		{"max_tool_iterations", d.MaxToolIterations, 200},
		{"max_concurrent_subagents", d.MaxConcurrentSubagent, 4},
		{"max_tool_result_chars", d.MaxToolResultChars, 16000},
		{"provider_retry_mode", d.ProviderRetryMode, "standard"},
		{"tool_hint_max_length", d.ToolHintMaxLength, 40},
		{"timezone_mode", d.TimezoneMode, "auto"},
		{"bot_name", d.BotName, "nanobot"},
		{"bot_icon", d.BotIcon, "🐈"},
		{"unified_session", d.UnifiedSession, false},
		{"session_ttl_minutes", d.SessionTTLMinutes, 15},
		{"idle_compact_check_interval_seconds", d.IdleCompactCheckIntervalSecs, 60},
		{"model_preset", d.ModelPreset, (*string)(nil)},
		{"reasoning_effort", d.ReasoningEffort, (*string)(nil)},
		{"dream.enabled", d.Dream.Enabled, true},
		{"dream.interval_h", d.Dream.IntervalH, 2},
		{"dream.cron", d.Dream.Cron, (*string)(nil)},
		{"dream.model_override", d.Dream.ModelOverride, (*string)(nil)},
		{"fallback_models.len", len(d.FallbackModels), 0},
		{"disabled_skills.len", len(d.DisabledSkills), 0},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("AgentDefaults.%s = %#v, want %#v", tc.field, tc.got, tc.want)
		}
	}

	// timezone: the DECLARED default is "UTC" (schema.py:141) but
	// resolve_timezone (schema.py:159-173) replaces it with the detected system
	// zone whenever timezone_mode resolves to "auto", which is the default. With
	// TZ=UTC the detected value is normalized to "UTC" via _UTC_ALIASES.
	if d.Timezone != "UTC" {
		t.Errorf("timezone with TZ=UTC = %q, want %q", d.Timezone, "UTC")
	}
}

// TestOtherSectionDefaults covers the remaining sections so a regression in any
// of them is caught.
func TestOtherSectionDefaults(t *testing.T) {
	c := DefaultConfig()

	if !c.Channels.SendProgress || !c.Channels.SendToolHints || !c.Channels.ShowReasoning ||
		!c.Channels.ExtractDocumentText {
		t.Error("channels booleans should default to true")
	}
	if c.Channels.SendMaxRetries != 3 || c.Channels.TranscriptionProvider != "groq" ||
		c.Channels.TranscriptionLanguage != nil {
		t.Errorf("channels defaults wrong: %+v", c.Channels)
	}
	if !c.Transcription.Enabled || c.Transcription.MaxDurationSec != 120 || c.Transcription.MaxUploadMB != 25 {
		t.Errorf("transcription defaults wrong: %+v", c.Transcription)
	}
	if c.API.Host != "127.0.0.1" || c.API.Port != 8900 || c.API.Timeout != 120.0 || c.API.APIKey != "" {
		t.Errorf("api defaults wrong: %+v", c.API)
	}
	if c.Gateway.Host != "127.0.0.1" || c.Gateway.Port != 18790 || c.Gateway.RestartMode != "auto" ||
		!c.Gateway.Heartbeat.Enabled || c.Gateway.Heartbeat.IntervalS != 1800 {
		t.Errorf("gateway defaults wrong: %+v", c.Gateway)
	}
	if !c.Tools.Web.Enable || c.Tools.Web.Search.Provider != "duckduckgo" ||
		c.Tools.Web.Search.MaxResults != 5 || c.Tools.Web.Search.Timeout != 30 ||
		!c.Tools.Web.Fetch.UseJinaReader {
		t.Errorf("tools.web defaults wrong: %+v", c.Tools.Web)
	}
	if !c.Tools.Exec.Enable || c.Tools.Exec.Timeout != 60 {
		t.Errorf("tools.exec defaults wrong: %+v", c.Tools.Exec)
	}
	if !c.Tools.File.Enable || !c.Tools.CliApps.Enable || c.Tools.CliApps.InstallTimeout != 300 ||
		c.Tools.CliApps.RunTimeout != 60 || c.Tools.CliApps.CatalogTTLSeconds != 3600 {
		t.Errorf("tools file/cliApps defaults wrong: %+v %+v", c.Tools.File, c.Tools.CliApps)
	}
	if !c.Tools.My.Enable || c.Tools.My.AllowSet {
		t.Errorf("tools.my defaults wrong: %+v", c.Tools.My)
	}
	if c.Tools.ImageGeneration.Enabled || c.Tools.ImageGeneration.Provider != "openrouter" ||
		c.Tools.ImageGeneration.Model != "openai/gpt-5.4-image-2" ||
		c.Tools.ImageGeneration.MaxImagesPerTurn != 4 || c.Tools.ImageGeneration.SaveDir != "generated" {
		t.Errorf("tools.imageGeneration defaults wrong: %+v", c.Tools.ImageGeneration)
	}
	if c.Tools.MaxSessionMessagesPerMinute != 6 || c.Tools.RestrictToWorkspace ||
		!c.Tools.WebUIAllowLocalServiceAccess || c.Tools.WebUIAllowRemotePackageInstall {
		t.Errorf("tools scalar defaults wrong: %+v", c.Tools)
	}
	if len(c.Tools.MCPServers) != 0 || len(c.Tools.SSRFWhitelist) != 0 || len(c.ModelPresets) != 0 {
		t.Error("map/slice defaults should be empty")
	}
	if c.Providers.OpenAI.APIType != "auto" || c.Providers.Anthropic.APIKey != nil {
		t.Errorf("provider defaults wrong: %+v", c.Providers.OpenAI)
	}

	// MCP server field defaults (schema.py:365-374).
	m := DefaultMCPServerConfig()
	if m.ToolTimeout != 30 || len(m.EnabledTools) != 1 || m.EnabledTools[0] != "*" ||
		m.Command != "" || m.CWD != "" || m.URL != "" || m.Type != nil || m.Auth != nil {
		t.Errorf("mcp defaults wrong: %+v", m)
	}
}

// ---------------------------------------------------------------------------
// 3. serialization aliases
// ---------------------------------------------------------------------------

// TestSerializedAgentDefaultsKeySet pins the exact serialized key set measured
// from the reference (compat/python/dump_reference.py -> serialized_defaults).
// In particular session_ttl_minutes MUST serialize as idleCompactAfterMinutes,
// and dream.cron MUST be absent because it is exclude_if None.
func TestSerializedAgentDefaultsKeySet(t *testing.T) {
	t.Setenv("TZ", "UTC")

	raw, err := marshalNoEscape(DefaultConfig().Agents.Defaults)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := []string{
		"botIcon", "botName", "contextWindowTokens", "disabledSkills", "dream",
		"fallbackModels", "idleCompactAfterMinutes", "idleCompactCheckIntervalSeconds",
		"maxConcurrentSubagents", "maxTokens", "maxToolIterations", "maxToolResultChars",
		"model", "modelPreset", "provider", "providerRetryMode", "reasoningEffort",
		"temperature", "timezone", "timezoneMode", "toolHintMaxLength", "unifiedSession",
		"workspace",
	}
	gotKeys := make([]string, 0, len(got))
	for k := range got {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	if strings.Join(gotKeys, ",") != strings.Join(want, ",") {
		t.Errorf("serialized AgentDefaults keys:\n got %v\nwant %v", gotKeys, want)
	}

	// Nulls are emitted, not omitted (save_config does not pass exclude_none).
	for _, k := range []string{"modelPreset", "reasoningEffort"} {
		if string(got[k]) != "null" {
			t.Errorf("%s = %s, want null", k, got[k])
		}
	}
	if string(got["idleCompactAfterMinutes"]) != "15" {
		t.Errorf("idleCompactAfterMinutes = %s, want 15", got["idleCompactAfterMinutes"])
	}
	if string(got["botIcon"]) != `"🐈"` {
		t.Errorf("botIcon = %s, want the raw emoji", got["botIcon"])
	}

	// dream.cron is exclude_if None -> absent; modelOverride is present as null.
	var dream map[string]json.RawMessage
	if err := json.Unmarshal(got["dream"], &dream); err != nil {
		t.Fatalf("unmarshal dream: %v", err)
	}
	if _, present := dream["cron"]; present {
		t.Error("dream.cron must be omitted when nil (exclude_if None)")
	}
	if string(dream["modelOverride"]) != "null" {
		t.Errorf("dream.modelOverride = %s, want null", dream["modelOverride"])
	}

	// openai_codex / xai_grok / github_copilot carry exclude=True.
	praw, err := marshalNoEscape(DefaultConfig().Providers)
	if err != nil {
		t.Fatalf("marshal providers: %v", err)
	}
	var provs map[string]json.RawMessage
	if err := json.Unmarshal(praw, &provs); err != nil {
		t.Fatalf("unmarshal providers: %v", err)
	}
	for _, excluded := range []string{"openaiCodex", "xaiGrok", "githubCopilot"} {
		if _, present := provs[excluded]; present {
			t.Errorf("providers.%s must be excluded from serialization", excluded)
		}
	}
	// displayName is exclude_if None.
	if strings.Contains(string(provs["custom"]), "displayName") {
		t.Error("providers.custom.displayName must be omitted when nil")
	}
}

// TestSaveSerializesWithAliases checks the on-disk artifact, including the
// OAuth re-additions that save_config performs (loader.py:158-171).
func TestSaveSerializesWithAliases(t *testing.T) {
	t.Setenv("TZ", "UTC")
	cfg := DefaultConfig()
	proxy := "http://proxy"
	cfg.Providers.OpenAICodex.Proxy = &proxy
	cfg.Providers.XAIGrok.ExtraBody = map[string]any{"a": 1}
	cfg.Providers.OpenAICodex.APIKey = strPtr("must-not-be-persisted")

	target := filepath.Join(t.TempDir(), "nested", "config.json")
	if err := cfg.Save(target); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("saved file is not valid JSON: %v", err)
	}
	providers := doc["providers"].(map[string]any)
	codex, ok := providers["openaiCodex"].(map[string]any)
	if !ok {
		t.Fatal("save must re-add providers.openaiCodex")
	}
	if codex["proxy"] != "http://proxy" {
		t.Errorf("openaiCodex.proxy = %v", codex["proxy"])
	}
	if _, leaked := codex["apiKey"]; leaked {
		t.Error("openaiCodex.apiKey must NOT be persisted (OAuth credentials live elsewhere)")
	}
	grok, ok := providers["xaiGrok"].(map[string]any)
	if !ok {
		t.Fatal("save must re-add providers.xaiGrok")
	}
	if _, has := grok["extraBody"]; !has {
		t.Errorf("xaiGrok.extraBody missing: %v", grok)
	}
	// 2-space indentation, no trailing newline, raw emoji.
	if !strings.Contains(string(data), "\n  \"agents\": {") {
		t.Error("expected two-space indented output")
	}
	if strings.HasSuffix(string(data), "\n") {
		t.Error("json.dumps does not emit a trailing newline")
	}
	if !strings.Contains(string(data), "🐈") {
		t.Error("non-ASCII must be emitted raw (ensure_ascii=False)")
	}
}

// ---------------------------------------------------------------------------
// 4. dual-alias fields
// ---------------------------------------------------------------------------

// TestDualAliasFields covers every field whose INPUT alias differs from its
// SERIALIZATION alias, plus the AliasChoices spellings.
func TestDualAliasFields(t *testing.T) {
	t.Setenv("TZ", "UTC")

	t.Run("session_ttl_minutes", func(t *testing.T) {
		for _, key := range []string{"idleCompactAfterMinutes", "sessionTtlMinutes", "session_ttl_minutes"} {
			cfg := mustLoad(t, `{"agents":{"defaults":{"`+key+`":42}}}`)
			if cfg.Agents.Defaults.SessionTTLMinutes != 42 {
				t.Errorf("%s did not bind to session_ttl_minutes (got %d)",
					key, cfg.Agents.Defaults.SessionTTLMinutes)
			}
		}
		// AliasChoices order decides when both are present: idleCompactAfterMinutes wins.
		cfg := mustLoad(t, `{"agents":{"defaults":{"idleCompactAfterMinutes":7,"sessionTtlMinutes":9}}}`)
		if cfg.Agents.Defaults.SessionTTLMinutes != 7 {
			t.Errorf("first AliasChoice must win, got %d", cfg.Agents.Defaults.SessionTTLMinutes)
		}
		// The snake_case spelling is NOT a valid serialization alias.
		b, _ := marshalNoEscape(cfg.Agents.Defaults)
		if strings.Contains(string(b), "sessionTtlMinutes") || !strings.Contains(string(b), "idleCompactAfterMinutes") {
			t.Errorf("session_ttl_minutes must serialize as idleCompactAfterMinutes: %s", b)
		}
	})

	t.Run("tool_hint_max_length", func(t *testing.T) {
		for _, key := range []string{"toolHintMaxLength", "tool_hint_max_length"} {
			cfg := mustLoad(t, `{"agents":{"defaults":{"`+key+`":99}}}`)
			if cfg.Agents.Defaults.ToolHintMaxLength != 99 {
				t.Errorf("%s did not bind (got %d)", key, cfg.Agents.Defaults.ToolHintMaxLength)
			}
		}
		// Wrong case is NOT accepted (nested models are case-sensitive).
		cfg := mustLoad(t, `{"agents":{"defaults":{"toolhintmaxlength":99}}}`)
		if cfg.Agents.Defaults.ToolHintMaxLength != 40 {
			t.Errorf("case-insensitive matching must not apply to nested models, got %d",
				cfg.Agents.Defaults.ToolHintMaxLength)
		}
	})

	t.Run("webui_allow_local_service_access", func(t *testing.T) {
		for _, key := range []string{
			"webuiAllowLocalServiceAccess", "webui_allow_local_service_access",
			"allowLocalPreviewAccess", "allow_local_preview_access",
		} {
			cfg := mustLoad(t, `{"tools":{"`+key+`":false}}`)
			if cfg.Tools.WebUIAllowLocalServiceAccess {
				t.Errorf("%s did not bind", key)
			}
		}
	})

	t.Run("webui_allow_remote_package_install", func(t *testing.T) {
		for _, key := range []string{"webuiAllowRemotePackageInstall", "webui_allow_remote_package_install"} {
			cfg := mustLoad(t, `{"tools":{"`+key+`":true}}`)
			if !cfg.Tools.WebUIAllowRemotePackageInstall {
				t.Errorf("%s did not bind", key)
			}
		}
	})

	t.Run("dream_model_override", func(t *testing.T) {
		base := `"modelPresets":{"p1":{"model":"m"}},`
		for _, key := range []string{"modelOverride", "model", "model_override"} {
			cfg := mustLoad(t, `{`+base+`"agents":{"defaults":{"dream":{"`+key+`":"p1"}}}}`)
			if cfg.Agents.Defaults.Dream.ModelOverride == nil || *cfg.Agents.Defaults.Dream.ModelOverride != "p1" {
				t.Errorf("dream.%s did not bind", key)
			}
		}
		// Serializes as modelOverride regardless of which alias was used.
		cfg := mustLoad(t, `{`+base+`"agents":{"defaults":{"dream":{"model":"p1"}}}}`)
		b, _ := marshalNoEscape(cfg.Agents.Defaults.Dream)
		if !strings.Contains(string(b), `"modelOverride":"p1"`) {
			t.Errorf("dream must serialize as modelOverride: %s", b)
		}
	})

	t.Run("model_presets", func(t *testing.T) {
		for _, key := range []string{"modelPresets", "model_presets"} {
			cfg := mustLoad(t, `{"`+key+`":{"p":{"model":"m"}}}`)
			if _, ok := cfg.ModelPresets["p"]; !ok {
				t.Errorf("%s did not bind", key)
			}
		}
		b, _ := marshalNoEscape(DefaultConfig())
		if !strings.Contains(string(b), `"modelPresets":{}`) {
			t.Errorf("model_presets must serialize as modelPresets: %s", b)
		}
	})
}

// ---------------------------------------------------------------------------
// 5. bound violations
// ---------------------------------------------------------------------------

// TestBoundViolations requires every ge/le constraint to raise an error rather
// than clamp the value.
func TestBoundViolations(t *testing.T) {
	t.Setenv("TZ", "UTC")

	cases := []struct {
		name    string
		body    string
		loc     string
		value   any
		checker func(*Config) any
	}{
		{"max_concurrent_subagents below min", `{"agents":{"defaults":{"maxConcurrentSubagents":0}}}`,
			"agents.defaults.maxConcurrentSubagents", 4, func(c *Config) any { return c.Agents.Defaults.MaxConcurrentSubagent }},
		{"tool_hint_max_length below min", `{"agents":{"defaults":{"toolHintMaxLength":19}}}`,
			"agents.defaults.toolHintMaxLength", 40, func(c *Config) any { return c.Agents.Defaults.ToolHintMaxLength }},
		{"tool_hint_max_length above max", `{"agents":{"defaults":{"toolHintMaxLength":501}}}`,
			"agents.defaults.toolHintMaxLength", 40, func(c *Config) any { return c.Agents.Defaults.ToolHintMaxLength }},
		{"session_ttl_minutes below min", `{"agents":{"defaults":{"sessionTtlMinutes":-1}}}`,
			"agents.defaults.sessionTtlMinutes", 15, func(c *Config) any { return c.Agents.Defaults.SessionTTLMinutes }},
		{"idle_compact_check_interval below min", `{"agents":{"defaults":{"idleCompactCheckIntervalSeconds":-1}}}`,
			"agents.defaults.idleCompactCheckIntervalSeconds", 60, func(c *Config) any { return c.Agents.Defaults.IdleCompactCheckIntervalSecs }},
		{"dream interval_h below min", `{"agents":{"defaults":{"dream":{"intervalH":0}}}}`,
			"agents.defaults.dream.intervalH", 2, func(c *Config) any { return c.Agents.Defaults.Dream.IntervalH }},
		{"send_max_retries above max", `{"channels":{"sendMaxRetries":11}}`,
			"channels.sendMaxRetries", 3, func(c *Config) any { return c.Channels.SendMaxRetries }},
		{"send_max_retries below min", `{"channels":{"sendMaxRetries":-1}}`,
			"channels.sendMaxRetries", 3, func(c *Config) any { return c.Channels.SendMaxRetries }},
		{"transcription max_duration_sec above max", `{"transcription":{"maxDurationSec":601}}`,
			"transcription.maxDurationSec", 120, func(c *Config) any { return c.Transcription.MaxDurationSec }},
		{"transcription max_upload_mb above max", `{"transcription":{"maxUploadMb":101}}`,
			"transcription.maxUploadMb", 25, func(c *Config) any { return c.Transcription.MaxUploadMB }},
		{"exec timeout below min", `{"tools":{"exec":{"timeout":-1}}}`,
			"tools.exec.timeout", 60, func(c *Config) any { return c.Tools.Exec.Timeout }},
		{"max_session_messages_per_minute below min", `{"tools":{"maxSessionMessagesPerMinute":0}}`,
			"tools.maxSessionMessagesPerMinute", 6, func(c *Config) any { return c.Tools.MaxSessionMessagesPerMinute }},
		{"cli install_timeout above max", `{"tools":{"cliApps":{"installTimeout":3601}}}`,
			"tools.cliApps.installTimeout", 300, func(c *Config) any { return c.Tools.CliApps.InstallTimeout }},
		{"cli catalog_ttl_seconds below min", `{"tools":{"cliApps":{"catalogTtlSeconds":59}}}`,
			"tools.cliApps.catalogTtlSeconds", 3600, func(c *Config) any { return c.Tools.CliApps.CatalogTTLSeconds }},
		{"image max_images_per_turn above max", `{"tools":{"imageGeneration":{"maxImagesPerTurn":9}}}`,
			"tools.imageGeneration.maxImagesPerTurn", 4, func(c *Config) any { return c.Tools.ImageGeneration.MaxImagesPerTurn }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadFromJSON(t, tc.body)
			if err == nil {
				t.Fatalf("expected a bound violation error, got config with value %v", tc.checker(cfg))
			}
			le := loadError(t, err)
			if le.Kind != KindInvalidSchema {
				t.Errorf("Kind = %q, want %q", le.Kind, KindInvalidSchema)
			}
			locs := issueLocations(le)
			if len(locs) != 1 || locs[0] != tc.loc {
				t.Errorf("issue locations = %v, want [%s]", locs, tc.loc)
			}
			// The value must never be silently clamped into a loaded config.
			if cfg != nil {
				t.Errorf("expected a nil config on validation failure, got %+v", cfg)
			}
		})
	}
}

// TestValidationRules covers the non-numeric validation rules.
func TestValidationRules(t *testing.T) {
	t.Setenv("TZ", "UTC")

	cases := []struct {
		name string
		body string
		loc  string
	}{
		{"unknown root key is forbidden", `{"bogus":1}`, "bogus"},
		{"unknown nested key is ignored", `{"tools":{"bogus":1}}`, ""},
		{"providerRetryMode literal", `{"agents":{"defaults":{"providerRetryMode":"bogus"}}}`,
			"agents.defaults.providerRetryMode"},
		{"timezoneMode literal", `{"agents":{"defaults":{"timezoneMode":"bogus"}}}`,
			"agents.defaults.timezoneMode"},
		{"restartMode literal", `{"gateway":{"restartMode":"bogus"}}`, "gateway.restartMode"},
		{"apiType literal", `{"providers":{"openai":{"apiType":"bogus"}}}`, "providers.openai.apiType"},
		{"mcp type literal", `{"tools":{"mcpServers":{"a":{"type":"bogus"}}}}`, "tools.mcpServers.a.type"},
		{"thinkingStyle value", `{"providers":{"custom":{"thinkingStyle":"bogus"}}}`,
			"providers.custom.thinkingStyle"},
		{"api_type scope", `{"providers":{"deepseek":{"apiType":"responses"}}}`, "providers"},
		{"api wildcard host needs key", `{"api":{"host":"0.0.0.0"}}`, "api"},
		{"unknown timezone", `{"agents":{"defaults":{"timezone":"Not/AZone","timezoneMode":"manual"}}}`,
			"agents.defaults.timezone"},
		{"language pattern", `{"transcription":{"language":"english"}}`, "transcription.language"},
		{"reserved preset name", `{"modelPresets":{"default":{"model":"m"}}}`, "<root>"},
		{"missing preset ref", `{"agents":{"defaults":{"modelPreset":"nope"}}}`, "<root>"},
		{"fallback preset ref", `{"agents":{"defaults":{"fallbackModels":["nope"]}}}`, "<root>"},
		{"string not coerced from number", `{"agents":{"defaults":{"model":1}}}`,
			"agents.defaults.model"},
		{"non-integral float into int", `{"agents":{"defaults":{"maxTokens":8192.5}}}`,
			"agents.defaults.maxTokens"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadFromJSON(t, tc.body)
			if tc.loc == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a validation error")
			}
			le := loadError(t, err)
			locs := issueLocations(le)
			if len(locs) == 0 || locs[0] != tc.loc {
				t.Errorf("issue locations = %v, want first = %s", locs, tc.loc)
			}
		})
	}
}

// TestPydanticLaxCoercion pins the coercions verified against pydantic 2.13.5.
func TestPydanticLaxCoercion(t *testing.T) {
	t.Setenv("TZ", "UTC")
	cases := []struct {
		body  string
		check func(*Config) any
		want  any
	}{
		{`{"agents":{"defaults":{"maxTokens":"8192"}}}`, func(c *Config) any { return c.Agents.Defaults.MaxTokens }, 8192},
		{`{"agents":{"defaults":{"maxTokens":8192.0}}}`, func(c *Config) any { return c.Agents.Defaults.MaxTokens }, 8192},
		{`{"agents":{"defaults":{"maxTokens":true}}}`, func(c *Config) any { return c.Agents.Defaults.MaxTokens }, 1},
		{`{"agents":{"defaults":{"unifiedSession":"false"}}}`, func(c *Config) any { return c.Agents.Defaults.UnifiedSession }, false},
		{`{"agents":{"defaults":{"unifiedSession":"yes"}}}`, func(c *Config) any { return c.Agents.Defaults.UnifiedSession }, true},
		{`{"agents":{"defaults":{"unifiedSession":1}}}`, func(c *Config) any { return c.Agents.Defaults.UnifiedSession }, true},
		{`{"agents":{"defaults":{"temperature":"0.5"}}}`, func(c *Config) any { return float64(c.Agents.Defaults.Temperature) }, 0.5},
		{`{"agents":{"defaults":{"temperature":1}}}`, func(c *Config) any { return float64(c.Agents.Defaults.Temperature) }, 1.0},
		{`{"tools":{"mcpServers":{"a":{"toolTimeout":"30"}}}}`,
			func(c *Config) any { return c.Tools.MCPServers["a"].ToolTimeout }, 30},
	}
	for _, tc := range cases {
		cfg, err := loadFromJSON(t, tc.body)
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.body, err)
			continue
		}
		if got := tc.check(cfg); got != tc.want {
			t.Errorf("%s: got %#v, want %#v", tc.body, got, tc.want)
		}
	}

	// Values that must be rejected.
	for _, body := range []string{
		`{"agents":{"defaults":{"unifiedSession":"maybe"}}}`,
		`{"agents":{"defaults":{"unifiedSession":2}}}`,
		`{"agents":{"defaults":{"disabledSkills":"a"}}}`,
		`{"agents":{"defaults":{"disabledSkills":[1]}}}`,
		`{"providers":{"custom":{"extraHeaders":{"a":1}}}}`,
		`{"agents":{"defaults":null}}`,
	} {
		if _, err := loadFromJSON(t, body); err == nil {
			t.Errorf("%s: expected an error", body)
		}
	}
}

// ---------------------------------------------------------------------------
// 6. missing file
// ---------------------------------------------------------------------------

// TestMissingFileYieldsDefaults is the explicit requirement that an absent
// config.json is not an error (loader.py:59-80).
func TestMissingFileYieldsDefaults(t *testing.T) {
	t.Setenv("TZ", "UTC")
	t.Setenv("HOME", t.TempDir())

	missing := filepath.Join(t.TempDir(), "does", "not", "exist.json")
	cfg, err := Load(missing)
	if err != nil {
		t.Fatalf("Load(missing) must not fail, got %v", err)
	}
	if cfg.Agents.Defaults.Model != "anthropic/claude-opus-4-5" {
		t.Errorf("defaults not applied: %q", cfg.Agents.Defaults.Model)
	}
	if cfg.SourcePath() != resolvePath(missing) {
		t.Errorf("SourcePath = %q, want %q", cfg.SourcePath(), resolvePath(missing))
	}
	if got, want := cfg.RuntimeDataDir(), filepath.Dir(resolvePath(missing)); got != want {
		t.Errorf("RuntimeDataDir = %q, want %q", got, want)
	}

	// Empty path resolves to the default location, which does not exist here.
	def, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault must not fail, got %v", err)
	}
	if def.SourcePath() != DefaultConfigPath() {
		t.Errorf("LoadDefault SourcePath = %q, want %q", def.SourcePath(), DefaultConfigPath())
	}
}

// TestEmptyObjectEqualsDefaults checks that an empty JSON object yields exactly
// the default configuration.
func TestEmptyObjectEqualsDefaults(t *testing.T) {
	t.Setenv("TZ", "UTC")
	fromFile := normalizedJSON(t, mustLoad(t, `{}`))
	fromDefault := normalizedJSON(t, DefaultConfig())
	if fromFile != fromDefault {
		t.Errorf("{} and DefaultConfig() differ:\n%s\n%s", fromFile, fromDefault)
	}
}

// ---------------------------------------------------------------------------
// 7. round trip
// ---------------------------------------------------------------------------

// TestLoadSaveLoadRoundTrip requires Load -> Save -> Load to preserve values.
func TestLoadSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("TZ", "UTC")

	original := `{
	  "agents": {"defaults": {"model": "deepseek/deepseek-chat", "provider": "deepseek",
	    "maxTokens": 4096, "temperature": 0.4, "idleCompactAfterMinutes": 33,
	    "toolHintMaxLength": 77, "timezone": "Europe/Lisbon", "timezoneMode": "manual",
	    "botName": "round", "dream": {"intervalH": 9, "modelOverride": "p1"}}},
	  "channels": {"sendProgress": false, "telegram": {"token": "tok", "enabled": true}},
	  "providers": {"deepseek": {"apiKey": "sk-test"}, "myproxy": {"apiBase": "http://p", "displayName": "My"}},
	  "tools": {"mcpServers": {"srv": {"command": "npx", "args": ["-y", "pkg"], "enabledTools": ["*"]}},
	            "ssrfWhitelist": ["100.64.0.0/10"], "allowLocalPreviewAccess": false},
	  "modelPresets": {"p1": {"model": "m", "temperature": 0.9}}
	}`

	first := mustLoad(t, original)
	target := filepath.Join(t.TempDir(), "round", "config.json")
	if err := first.Save(target); err != nil {
		t.Fatalf("Save: %v", err)
	}
	second, err := Load(target)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	a := normalizedJSON(t, first)
	b := normalizedJSON(t, second)
	if a != b {
		t.Fatalf("round trip changed the configuration\nfirst:  %s\nsecond: %s", a, b)
	}

	// Spot-check that the interesting values really survived.
	d := second.Agents.Defaults
	if d.Model != "deepseek/deepseek-chat" || d.MaxTokens != 4096 || d.Temperature != 0.4 ||
		d.SessionTTLMinutes != 33 || d.ToolHintMaxLength != 77 || d.BotName != "round" ||
		d.Timezone != "Europe/Lisbon" || d.TimezoneMode != "manual" || d.Dream.IntervalH != 9 {
		t.Errorf("values lost in round trip: %+v", d)
	}
	if second.Channels.SendProgress {
		t.Error("channels.sendProgress lost")
	}
	if second.Channels.Extra["telegram"] == nil {
		t.Error("channel extra field lost")
	}
	if second.Providers.DeepSeek.APIKey == nil || *second.Providers.DeepSeek.APIKey != "sk-test" {
		t.Error("provider apiKey lost")
	}
	if second.Providers.ExtraProviders["myproxy"].APIBase == nil {
		t.Error("custom provider lost")
	}
	if got := second.Tools.MCPServers["srv"].Args; len(got) != 2 || got[1] != "pkg" {
		t.Errorf("mcp args lost: %v", got)
	}
	if second.Tools.WebUIAllowLocalServiceAccess {
		t.Error("legacy allowLocalPreviewAccess alias lost on round trip")
	}
	if len(second.Tools.SSRFWhitelist) != 1 || second.Tools.SSRFWhitelist[0] != "100.64.0.0/10" {
		t.Errorf("ssrfWhitelist lost: %v", second.Tools.SSRFWhitelist)
	}
	if second.ModelPresets["p1"].Temperature != 0.9 {
		t.Error("model preset lost")
	}
}

// TestSaveIsAtomicAndPreservesMode checks the temp+rename write path.
func TestSaveIsAtomicAndPreservesMode(t *testing.T) {
	t.Setenv("TZ", "UTC")
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := DefaultConfig().Save(target); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640 preserved from the existing file", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// ---------------------------------------------------------------------------
// 8. timezone resolution
// ---------------------------------------------------------------------------

// TestTimezoneResolution pins the resolve_timezone rules (schema.py:159-173).
func TestTimezoneResolution(t *testing.T) {
	cases := []struct {
		name     string
		tzEnv    string
		body     string
		wantTZ   string
		wantMode string
	}{
		{"no keys -> auto + system zone", "Asia/Tokyo", `{}`, "Asia/Tokyo", "auto"},
		{"explicit timezone implies manual", "Asia/Tokyo",
			`{"agents":{"defaults":{"timezone":"Europe/Paris"}}}`, "Europe/Paris", "manual"},
		{"mode auto overrides explicit timezone", "Asia/Tokyo",
			`{"agents":{"defaults":{"timezone":"Europe/Paris","timezoneMode":"auto"}}}`, "Asia/Tokyo", "auto"},
		{"mode manual preserves explicit timezone", "Asia/Tokyo",
			`{"agents":{"defaults":{"timezone":"Europe/Paris","timezoneMode":"manual"}}}`, "Europe/Paris", "manual"},
		{"snake_case timezone_mode also triggers auto", "Asia/Tokyo",
			`{"agents":{"defaults":{"timezone":"Europe/Paris","timezone_mode":"auto"}}}`, "Asia/Tokyo", "auto"},
		{"UTC alias normalizes to UTC", "UTC", `{}`, "UTC", "auto"},
		{"Etc/UTC alias normalizes to UTC", "Etc/UTC", `{}`, "UTC", "auto"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TZ", tc.tzEnv)
			cfg := mustLoad(t, tc.body)
			if got := cfg.Agents.Defaults.Timezone; got != tc.wantTZ {
				t.Errorf("timezone = %q, want %q", got, tc.wantTZ)
			}
			if got := cfg.Agents.Defaults.TimezoneMode; got != tc.wantMode {
				t.Errorf("timezone_mode = %q, want %q", got, tc.wantMode)
			}
		})
	}
}

// TestTimezoneModeNullIsTreatedAsAbsent mirrors `data.get("timezoneMode", ...)`
// returning None for an explicit null.
func TestTimezoneModeNullIsTreatedAsAbsent(t *testing.T) {
	t.Setenv("TZ", "Asia/Tokyo")
	cfg := mustLoad(t, `{"agents":{"defaults":{"timezoneMode":null}}}`)
	if cfg.Agents.Defaults.Timezone != "Asia/Tokyo" || cfg.Agents.Defaults.TimezoneMode != "auto" {
		t.Errorf("null timezoneMode: got %q/%q, want Asia/Tokyo/auto",
			cfg.Agents.Defaults.Timezone, cfg.Agents.Defaults.TimezoneMode)
	}
	cfg = mustLoad(t, `{"agents":{"defaults":{"timezoneMode":null,"timezone":"Europe/Paris"}}}`)
	if cfg.Agents.Defaults.Timezone != "Europe/Paris" || cfg.Agents.Defaults.TimezoneMode != "manual" {
		t.Errorf("null timezoneMode + explicit tz: got %q/%q, want Europe/Paris/manual",
			cfg.Agents.Defaults.Timezone, cfg.Agents.Defaults.TimezoneMode)
	}
}

// ---------------------------------------------------------------------------
// 9. case sensitivity / extra policy
// ---------------------------------------------------------------------------

// TestRootKeysAreCaseInsensitiveButNestedKeysAreNot pins a real asymmetry: the
// root Config descends from pydantic-settings' BaseSettings (case_sensitive=False
// via SettingsConfigDict) while nested models descend from Base, whose keys match
// case-sensitively.
func TestRootKeysAreCaseInsensitiveButNestedKeysAreNot(t *testing.T) {
	t.Setenv("TZ", "UTC")

	for _, key := range []string{"agents", "Agents", "AGENTS", "aGeNtS"} {
		cfg := mustLoad(t, `{"`+key+`":{"defaults":{"maxTokens":7}}}`)
		if cfg.Agents.Defaults.MaxTokens != 7 {
			t.Errorf("root key %q was not matched case-insensitively", key)
		}
	}
	for _, key := range []string{"maxTokens", "maxtokens", "MAXTOKENS", "MaxTokens"} {
		cfg := mustLoad(t, `{"agents":{"defaults":{"`+key+`":7}}}`)
		want := 8192
		if key == "maxTokens" {
			want = 7
		}
		if cfg.Agents.Defaults.MaxTokens != want {
			t.Errorf("nested key %q: got %d, want %d", key, cfg.Agents.Defaults.MaxTokens, want)
		}
	}
}

// TestExtraKeyPolicy pins all four extra policies.
func TestExtraKeyPolicy(t *testing.T) {
	t.Setenv("TZ", "UTC")

	// Root: extra="forbid" (inherited from BaseSettings).
	_, err := loadFromJSON(t, `{"bogus":1}`)
	if err == nil {
		t.Fatal("unknown root key must be rejected")
	}
	le := loadError(t, err)
	if len(le.Issues) != 1 || le.Issues[0].Message != "Unknown setting." {
		t.Errorf("issues = %+v, want one 'Unknown setting.'", le.Issues)
	}

	// Nested Base models: extra="ignore".
	cfg := mustLoad(t, `{"agents":{"zzz":1},"tools":{"bogus":2},"api":{"nope":3},"gateway":{"x":4}}`)
	if cfg.Agents.Defaults.MaxTokens != 8192 || cfg.Tools.RestrictToWorkspace || cfg.API.Host != "127.0.0.1" {
		t.Error("nested unknown keys must be ignored without disturbing defaults")
	}

	// ChannelsConfig: extra="allow".
	cfg = mustLoad(t, `{"channels":{"telegram":{"token":"t"},"discord":{"token":"d"}}}`)
	if len(cfg.Channels.Extra) != 2 {
		t.Fatalf("channels extras = %v, want 2 entries", cfg.Channels.Extra)
	}
	if cfg.Channels.Extra["telegram"].(map[string]any)["token"] != "t" {
		t.Errorf("channel extra not preserved: %v", cfg.Channels.Extra["telegram"])
	}

	// ProvidersConfig: extra="allow" -> custom providers.
	cfg = mustLoad(t, `{"providers":{"myproxy":{"apiKey":"k"}}}`)
	p, ok := cfg.Providers.ExtraProviders["myproxy"]
	if !ok {
		t.Fatal("custom provider not captured")
	}
	if p.APIKey == nil || *p.APIKey != "k" {
		t.Errorf("custom provider fields not decoded: %+v", p)
	}

	// A custom provider whose name collides with a built-in registry entry is
	// rejected (schema.py:296-310), including kebab-case spellings that normalize
	// onto a built-in name.
	for _, name := range []string{"azure-openai", "openai-codex", "CUSTOM"} {
		if _, err := loadFromJSON(t, `{"providers":{"`+name+`":{"apiKey":"k"}}}`); err == nil {
			t.Errorf("provider name %q must conflict with a built-in provider", name)
		}
	}
	if _, err := loadFromJSON(t, `{"providers":{"my-proxy":{"apiKey":"k"}}}`); err != nil {
		t.Errorf("non-conflicting custom provider rejected: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 10. error diagnostics
// ---------------------------------------------------------------------------

// TestErrorDiagnostics checks the structured error kinds and the redaction rule.
func TestErrorDiagnostics(t *testing.T) {
	t.Setenv("TZ", "UTC")

	t.Run("invalid json", func(t *testing.T) {
		_, err := loadFromJSON(t, `{"agents":`)
		if err == nil {
			t.Fatal("expected an error")
		}
		le := loadError(t, err)
		if le.Kind != KindInvalidJSON {
			t.Errorf("Kind = %q, want %q", le.Kind, KindInvalidJSON)
		}
		if !strings.Contains(le.Summary, "JSON syntax error at line") {
			t.Errorf("Summary = %q", le.Summary)
		}
	})

	t.Run("trailing data", func(t *testing.T) {
		_, err := loadFromJSON(t, `{} {}`)
		if err == nil {
			t.Fatal("expected an error for trailing content")
		}
		if le := loadError(t, err); le.Kind != KindInvalidJSON {
			t.Errorf("Kind = %q, want %q", le.Kind, KindInvalidJSON)
		}
	})

	t.Run("non-object root", func(t *testing.T) {
		_, err := loadFromJSON(t, `[1,2]`)
		if err == nil {
			t.Fatal("expected an error")
		}
		le := loadError(t, err)
		if le.Kind != KindInvalidRoot {
			t.Errorf("Kind = %q, want %q", le.Kind, KindInvalidRoot)
		}
		if len(le.Issues) != 1 || le.Issues[0].Location() != "<root>" {
			t.Errorf("Issues = %+v", le.Issues)
		}
	})

	t.Run("invalid utf-8", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte{'{', 0xff, 0xfe, '}'}, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected an error")
		}
		le := loadError(t, err)
		if le.Kind != KindIOError || le.Summary != "The file is not valid UTF-8." {
			t.Errorf("got kind=%q summary=%q", le.Kind, le.Summary)
		}
	})

	t.Run("directory is an io error", func(t *testing.T) {
		_, err := Load(t.TempDir())
		if err == nil {
			t.Fatal("expected an error")
		}
		if le := loadError(t, err); le.Kind != KindIOError {
			t.Errorf("Kind = %q, want %q", le.Kind, KindIOError)
		}
	})

	t.Run("multiple issues are collected", func(t *testing.T) {
		_, err := loadFromJSON(t, `{"agents":{"defaults":{"maxConcurrentSubagents":0,"toolHintMaxLength":1}},"api":{"port":"x"}}`)
		le := loadError(t, err)
		if len(le.Issues) != 3 {
			t.Fatalf("expected 3 issues, got %d: %+v", len(le.Issues), le.Issues)
		}
		got := issueLocations(le)
		want := []string{"agents.defaults.maxConcurrentSubagents", "agents.defaults.toolHintMaxLength", "api.port"}
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("locations = %v, want %v", got, want)
		}
		if !strings.Contains(le.Error(), "Found 3 invalid setting(s).") {
			t.Errorf("error text = %q", le.Error())
		}
	})

	t.Run("free-form keys are redacted", func(t *testing.T) {
		_, err := loadFromJSON(t, `{"tools":{"mcpServers":{"https://user:pw@host/x":{"toolTimeout":"bad"}}}}`)
		le := loadError(t, err)
		if len(le.Issues) != 1 {
			t.Fatalf("issues = %+v", le.Issues)
		}
		loc := le.Issues[0].Location()
		if strings.Contains(loc, "user") || strings.Contains(loc, "pw") {
			t.Errorf("credential-bearing key leaked into the location: %q", loc)
		}
		if loc != "tools.mcpServers.<redacted>.toolTimeout" {
			t.Errorf("location = %q", loc)
		}
	})
}

// ---------------------------------------------------------------------------
// 11. migration
// ---------------------------------------------------------------------------

// TestLegacyKeyMigration ports loader.py:347-382.
func TestLegacyKeyMigration(t *testing.T) {
	t.Setenv("TZ", "UTC")

	t.Run("my tool keys", func(t *testing.T) {
		cfg := mustLoad(t, `{"tools":{"myEnabled":false,"mySet":true}}`)
		if cfg.Tools.My.Enable || !cfg.Tools.My.AllowSet {
			t.Errorf("legacy my keys not migrated: %+v", cfg.Tools.My)
		}
		// And they are rewritten on save.
		target := filepath.Join(t.TempDir(), "config.json")
		if err := cfg.Save(target); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(target)
		var doc map[string]any
		_ = json.Unmarshal(data, &doc)
		tools := doc["tools"].(map[string]any)
		if _, has := tools["myEnabled"]; has {
			t.Error("myEnabled must not survive a save")
		}
		if _, has := tools["mySet"]; has {
			t.Error("mySet must not survive a save")
		}
	})

	t.Run("new keys win over legacy", func(t *testing.T) {
		cfg := mustLoad(t, `{"tools":{"myEnabled":false,"mySet":false,"my":{"enable":true,"allowSet":true}}}`)
		if !cfg.Tools.My.Enable || !cfg.Tools.My.AllowSet {
			t.Errorf("new keys must take precedence: %+v", cfg.Tools.My)
		}
	})

	t.Run("exec restrictToWorkspace is hoisted", func(t *testing.T) {
		cfg := mustLoad(t, `{"tools":{"exec":{"restrictToWorkspace":true}}}`)
		if !cfg.Tools.RestrictToWorkspace {
			t.Error("tools.exec.restrictToWorkspace must migrate to tools.restrictToWorkspace")
		}
	})

	t.Run("existing tools key wins", func(t *testing.T) {
		cfg := mustLoad(t, `{"tools":{"restrictToWorkspace":false,"exec":{"restrictToWorkspace":true}}}`)
		if cfg.Tools.RestrictToWorkspace {
			t.Error("the already-present tools.restrictToWorkspace must win")
		}
	})
}

// ---------------------------------------------------------------------------
// 12. path helpers
// ---------------------------------------------------------------------------

// TestPathHelpers checks path resolution against a controlled HOME.
//
// The branded helpers prefer ~/.haosbot and fall back to the legacy ~/.nanobot
// installation, which is the compatibility contract this port advertises: a
// machine that already has ~/.nanobot keeps using it, a fresh machine gets
// ~/.haosbot, and a machine with both prefers ~/.haosbot. The previous version of
// this test asserted ~/.nanobot for every helper and so failed against the
// documented behaviour — and covered none of the fallback branches.
func TestPathHelpers(t *testing.T) {
	t.Run("fresh install prefers ~/.haosbot", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		cases := []struct {
			name string
			got  string
			want string
		}{
			{"DefaultConfigPath", DefaultConfigPath(), filepath.Join(home, ".haosbot", "config.json")},
			{"DefaultDataDir", DefaultDataDir(), filepath.Join(home, ".haosbot")},
			{"DefaultWorkspace", DefaultWorkspace(), filepath.Join(home, ".haosbot", "workspace")},
			{"CronDir", CronDir(), filepath.Join(home, ".haosbot", "cron")},
			{"LogsDir", LogsDir(), filepath.Join(home, ".haosbot", "logs")},
			{"WebUIDir", WebUIDir(), filepath.Join(home, ".haosbot", "webui")},
			{"MediaDir(empty)", MediaDir(""), filepath.Join(home, ".haosbot", "media")},
			{"MediaDir(telegram)", MediaDir("telegram"), filepath.Join(home, ".haosbot", "media", "telegram")},
			// These two are ports of the reference's LEGACY-global helpers
			// (paths.py get_legacy_sessions_dir / get_cli_history_path), so they
			// keep the ~/.nanobot spelling by design.
			{"SessionsDir", SessionsDir(), filepath.Join(home, ".nanobot", "sessions")},
			{"CLIHistoryPath", CLIHistoryPath(), filepath.Join(home, ".nanobot", "history", "cli_history")},
		}
		for _, tc := range cases {
			if tc.got != tc.want {
				t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
			}
			if !filepath.IsAbs(tc.got) {
				t.Errorf("%s must be absolute, got %q", tc.name, tc.got)
			}
		}

		// The helpers must be pure: no directory may be created as a side effect.
		for _, dir := range []string{DefaultDataDir(), DefaultWorkspace(), SessionsDir(), CronDir(), LogsDir(), WebUIDir(), MediaDir("x")} {
			if _, err := os.Stat(dir); err == nil {
				t.Errorf("path helper created %q as a side effect", dir)
			}
		}
	})

	t.Run("legacy install falls back to ~/.nanobot", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		legacy := filepath.Join(home, ".nanobot")
		if err := os.MkdirAll(filepath.Join(legacy, "workspace"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacy, "config.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}

		if got, want := DefaultConfigPath(), filepath.Join(legacy, "config.json"); got != want {
			t.Errorf("DefaultConfigPath = %q, want the legacy path %q", got, want)
		}
		if got, want := DefaultDataDir(), legacy; got != want {
			t.Errorf("DefaultDataDir = %q, want the legacy dir %q", got, want)
		}
		if got, want := DefaultWorkspace(), filepath.Join(legacy, "workspace"); got != want {
			t.Errorf("DefaultWorkspace = %q, want the legacy workspace %q", got, want)
		}
		if got, want := CronDir(), filepath.Join(legacy, "cron"); got != want {
			t.Errorf("CronDir = %q, want %q", got, want)
		}
	})

	// A legacy install that never saved a config file but HAS runtime data must
	// still be found. Keying the fallback on config.json alone sent the port to an
	// empty ~/.haosbot and hid cli-apps/, plugins/, sessions/ and media/ — the
	// compat skills fixture is exactly this shape, and it silently disabled the
	// CLI-app skill aliases.
	t.Run("legacy data dir without config.json is still found", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		legacy := filepath.Join(home, ".nanobot")
		if err := os.MkdirAll(filepath.Join(legacy, "cli-apps"), 0o755); err != nil {
			t.Fatal(err)
		}

		if got, want := DefaultDataDir(), legacy; got != want {
			t.Errorf("DefaultDataDir = %q, want the legacy data dir %q", got, want)
		}
		if got, want := DefaultConfigPath(), filepath.Join(legacy, "config.json"); got != want {
			t.Errorf("DefaultConfigPath = %q, want %q", got, want)
		}
	})

	t.Run("both present prefers ~/.haosbot", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		for _, dir := range []string{
			filepath.Join(home, ".nanobot", "workspace"),
			filepath.Join(home, ".haosbot", "workspace"),
		} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(home, ".nanobot", "config.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}

		if got, want := DefaultConfigPath(), filepath.Join(home, ".haosbot", "config.json"); got != want {
			t.Errorf("DefaultConfigPath = %q, want %q", got, want)
		}
		if got, want := DefaultWorkspace(), filepath.Join(home, ".haosbot", "workspace"); got != want {
			t.Errorf("DefaultWorkspace = %q, want %q", got, want)
		}
	})
}

// TestWorkspacePathExpandsTilde checks Config.WorkspacePath and that the field
// default keeps its literal "~" spelling.
func TestWorkspacePathExpandsTilde(t *testing.T) {
	t.Setenv("TZ", "UTC")
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := DefaultConfig()
	if cfg.Agents.Defaults.Workspace != "~/.nanobot/workspace" {
		t.Errorf("field default = %q, want the literal tilde form", cfg.Agents.Defaults.Workspace)
	}
	if got, want := cfg.WorkspacePath(), filepath.Join(home, ".nanobot", "workspace"); got != want {
		t.Errorf("WorkspacePath = %q, want %q", got, want)
	}

	cfg = mustLoad(t, `{"agents":{"defaults":{"workspace":"~/custom"}}}`)
	if got, want := cfg.WorkspacePath(), filepath.Join(home, "custom"); got != want {
		t.Errorf("WorkspacePath = %q, want %q", got, want)
	}
}

// TestWorkspacePathFallsBackToDefault covers the "ghost workspace": with an
// unset (empty) configured workspace, WorkspacePath() used to return the empty
// string, so callers received a path that does not exist instead of the real
// default workspace. cmd/haosbot/runtime.go:369-371 already falls back to
// config.DefaultWorkspace() for exactly this case; WorkspacePath() must agree.
func TestWorkspacePathFallsBackToDefault(t *testing.T) {
	t.Run("empty falls back to the branded default", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		cfg := mustLoad(t, `{"agents":{"defaults":{"workspace":""}}}`)
		if cfg.Agents.Defaults.Workspace != "" {
			t.Fatalf("fixture: configured workspace = %q, want the empty value", cfg.Agents.Defaults.Workspace)
		}
		if got, want := cfg.WorkspacePath(), filepath.Join(home, ".haosbot", "workspace"); got != want {
			t.Errorf("WorkspacePath = %q, want the default workspace %q", got, want)
		}
	})

	t.Run("empty falls back to the legacy default when only that exists", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if err := os.MkdirAll(filepath.Join(home, ".nanobot", "workspace"), 0o755); err != nil {
			t.Fatal(err)
		}

		cfg := mustLoad(t, `{"agents":{"defaults":{"workspace":""}}}`)
		if got, want := cfg.WorkspacePath(), filepath.Join(home, ".nanobot", "workspace"); got != want {
			t.Errorf("WorkspacePath = %q, want the legacy default workspace %q", got, want)
		}
	})

	// A zero-value Config (constructed programmatically, not loaded) has an
	// empty workspace for the same reason.
	t.Run("zero-value config falls back too", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		cfg := &Config{}
		if got, want := cfg.WorkspacePath(), filepath.Join(home, ".haosbot", "workspace"); got != want {
			t.Errorf("WorkspacePath = %q, want the default workspace %q", got, want)
		}
	})

	t.Run("explicitly configured workspace is returned unchanged", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		// The branded default workspace exists, and must NOT win over an
		// explicitly configured path.
		if err := os.MkdirAll(filepath.Join(home, ".haosbot", "workspace"), 0o755); err != nil {
			t.Fatal(err)
		}

		for _, tc := range []struct{ configured, want string }{
			{"~/custom", filepath.Join(home, "custom")},
			{"/srv/agent-ws", "/srv/agent-ws"},
			{"relative/ws", "relative/ws"},
			{"~/.nanobot/workspace", filepath.Join(home, ".nanobot", "workspace")},
		} {
			cfg := mustLoad(t, `{"agents":{"defaults":{"workspace":"`+tc.configured+`"}}}`)
			if got := cfg.WorkspacePath(); got != tc.want {
				t.Errorf("WorkspacePath(%q) = %q, want %q", tc.configured, got, tc.want)
			}
		}
	})

	t.Run("tilde is expanded", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		cfg := mustLoad(t, `{"agents":{"defaults":{"workspace":"~"}}}`)
		if got := cfg.WorkspacePath(); got != home {
			t.Errorf("WorkspacePath(~) = %q, want %q", got, home)
		}
	})
}

// TestLoadAcceptsTildePath checks that Load and Save expand a leading "~".
func TestLoadAcceptsTildePath(t *testing.T) {
	t.Setenv("TZ", "UTC")
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := os.MkdirAll(filepath.Join(home, ".nanobot"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".nanobot", "config.json")
	if err := os.WriteFile(path, []byte(`{"agents":{"defaults":{"botName":"tilde"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("~/.nanobot/config.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agents.Defaults.BotName != "tilde" {
		t.Errorf("tilde path not expanded: %q", cfg.Agents.Defaults.BotName)
	}
	if err := cfg.Save("~/.nanobot/out.json"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".nanobot", "out.json")); err != nil {
		t.Errorf("Save did not expand the tilde: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 13. naming helpers
// ---------------------------------------------------------------------------

// TestToCamelMatchesPydantic checks the alias generator against values measured
// from pydantic 2.13.5.
func TestToCamelMatchesPydantic(t *testing.T) {
	cases := map[string]string{
		"workspace":                           "workspace",
		"max_tokens":                          "maxTokens",
		"api_key":                             "apiKey",
		"lm_studio":                           "lmStudio",
		"kimi_coding":                         "kimiCoding",
		"byteplus_coding_plan":                "byteplusCodingPlan",
		"idle_compact_check_interval_seconds": "idleCompactCheckIntervalSeconds",
		"webui_allow_local_service_access":    "webuiAllowLocalServiceAccess",
		"max_upload_mb":                       "maxUploadMb",
		"catalog_ttl_seconds":                 "catalogTtlSeconds",
		"use_jina_reader":                     "useJinaReader",
		"sandbox_ro_binds":                    "sandboxRoBinds",
		"extra_body":                          "extraBody",
		"enabled":                             "enabled",
	}
	for in, want := range cases {
		if got := toCamel(in); got != want {
			t.Errorf("toCamel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestToSnakeMatchesPydantic checks the provider-name normalizer.
func TestToSnakeMatchesPydantic(t *testing.T) {
	cases := map[string]string{
		"azureOpenai":  "azure_openai",
		"openai_codex": "openai_codex",
		"OpenAI_Codex": "open_ai_codex",
		"lmStudio":     "lm_studio",
		"CUSTOM":       "custom",
		"ovms":         "ovms",
	}
	for in, want := range cases {
		if got := toSnake(in); got != want {
			t.Errorf("toSnake(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 14. presets
// ---------------------------------------------------------------------------

// TestResolvePreset covers Config.resolve_preset (schema.py:481-488).
func TestResolvePreset(t *testing.T) {
	t.Setenv("TZ", "UTC")
	cfg := mustLoad(t, `{
	  "agents":{"defaults":{"model":"m0","provider":"p0","maxTokens":1,"temperature":0.2}},
	  "modelPresets":{"p1":{"model":"m1","provider":"p1","maxTokens":2,"temperature":0.3}}
	}`)

	def, err := cfg.ResolvePreset(nil)
	if err != nil {
		t.Fatalf("ResolvePreset(nil): %v", err)
	}
	if def.Model != "m0" || def.Provider != "p0" || def.MaxTokens != 1 || def.Temperature != 0.2 {
		t.Errorf("implicit default preset = %+v", def)
	}
	named := "p1"
	p1, err := cfg.ResolvePreset(&named)
	if err != nil {
		t.Fatalf("ResolvePreset(p1): %v", err)
	}
	if p1.Model != "m1" || p1.MaxTokens != 2 {
		t.Errorf("preset p1 = %+v", p1)
	}
	missing := "nope"
	if _, err := cfg.ResolvePreset(&missing); err == nil {
		t.Error("unknown preset must fail")
	}
	// An explicit "" is falsy in Python, so it yields the implicit default even
	// when agents.defaults.model_preset names something else.
	presetCfg := mustLoad(t, `{
	  "agents":{"defaults":{"model":"m0","provider":"p0","modelPreset":"p1"}},
	  "modelPresets":{"p1":{"model":"m1","provider":"p1"}}
	}`)
	empty := ""
	got, err := presetCfg.ResolvePreset(&empty)
	if err != nil {
		t.Fatalf("ResolvePreset(\"\"): %v", err)
	}
	if got.Model != "m0" {
		t.Errorf("explicit empty name must yield the implicit default, got %q", got.Model)
	}
	// nil follows agents.defaults.model_preset instead.
	got, err = presetCfg.ResolvePreset(nil)
	if err != nil {
		t.Fatalf("ResolvePreset(nil): %v", err)
	}
	if got.Model != "m1" {
		t.Errorf("nil name must follow model_preset, got %q", got.Model)
	}
}

func strPtr(s string) *string { return &s }
