package config

// Per-model decoders. Field tables mirror the pydantic field declarations in
// schema.py; the lookup rules live in decode.go.
//
// Extra-key policy, verified against pydantic 2.13.5 + pydantic-settings 2.15.0:
//
//	root Config        extra="forbid"  -> unknown keys are an ERROR ("Unknown setting.")
//	Base models        extra="ignore"  -> unknown keys are silently dropped
//	ChannelsConfig     extra="allow"   -> unknown keys preserved (schema.py:31)
//	ProvidersConfig    extra="allow"   -> unknown keys become custom providers (schema.py:247)
//
// Root key matching is also CASE-INSENSITIVE while nested `Base` matching is
// case-sensitive. That asymmetry is real: `Config` descends from `BaseSettings`,
// whose SettingsConfigDict sets case_sensitive=False (pydantic_settings/main.py),
// and `BaseSettings.model_validate(d)` routes through `__init__` and the settings
// sources. Verified: {"AGENTS":{...}} and {"aGeNtS":{...}} both bind to `agents`,
// while {"MAXTOKENS":7} inside agents.defaults is ignored.

import (
	"github.com/adrianolimagarcia/nanobot-go/internal/pyjson"
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Root
// ---------------------------------------------------------------------------

var configFields = []fieldDef{
	{name: "agents"},
	{name: "channels"},
	{name: "transcription"},
	{name: "providers"},
	{name: "api"},
	{name: "gateway"},
	{name: "tools"},
	{name: "model_presets", aliases: []string{"modelPresets", "model_presets"}},
}

// canonical returns the name pydantic reports in error locations, i.e. the first
// alias (or the generated camelCase name).
func (f fieldDef) canonical() string {
	if len(f.aliases) > 0 {
		return f.aliases[0]
	}
	return toCamel(f.name)
}

// ciIndex maps case-folded keys back to their original spelling, built from
// sorted keys so lookups are deterministic.
type ciIndex struct{ byFold map[string]string }

func newCIIndex(m map[string]any) ciIndex {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	idx := ciIndex{byFold: make(map[string]string, len(keys))}
	for _, k := range keys {
		fold := strings.ToLower(k)
		if _, seen := idx.byFold[fold]; !seen {
			idx.byFold[fold] = k
		}
	}
	return idx
}

// getCI resolves a root field with case-insensitive key matching.
func (f fieldDef) getCI(o *jmap, idx ciIndex) (any, string, bool) {
	names := f.aliases
	if len(names) == 0 {
		names = []string{toCamel(f.name)}
	}
	for _, a := range names {
		if v, ok := o.m[a]; ok {
			return v, a, true
		}
	}
	if v, ok := o.m[f.name]; ok {
		return v, f.name, true
	}
	for _, a := range names {
		if k, ok := idx.byFold[strings.ToLower(a)]; ok {
			return o.m[k], a, true
		}
	}
	if k, ok := idx.byFold[strings.ToLower(f.name)]; ok {
		return o.m[k], f.name, true
	}
	return nil, "", false
}

// decodeConfig parses the root document. It returns the config plus every
// collected issue (pydantic reports all field errors at once).
func decodeConfig(raw map[string]any) (*Config, []Issue) {
	c := &collector{}
	o := &jmap{m: raw}
	idx := newCIIndex(raw)
	cfg := defaultConfigSkeleton()

	// extra="forbid": any key that is not a declared field is an error.
	consumed := map[string]bool{}
	for _, f := range configFields {
		names := f.aliases
		if len(names) == 0 {
			names = []string{toCamel(f.name)}
		}
		for _, n := range names {
			consumed[strings.ToLower(n)] = true
		}
		consumed[strings.ToLower(f.name)] = true
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !consumed[strings.ToLower(k)] {
			c.add([]PathPart{Field(k)}, "extra_forbidden", "Extra inputs are not permitted")
		}
	}

	// Fields are decoded in declaration order so issue ordering matches pydantic.
	if v, key, ok := configFields[0].getCI(o, idx); ok {
		if sub, ok2 := c.asObject(v, []PathPart{Field(key)}, "AgentsConfig"); ok2 {
			cfg.Agents = decodeAgents(c, sub)
		}
	}
	if v, key, ok := configFields[1].getCI(o, idx); ok {
		if sub, ok2 := c.asObject(v, []PathPart{Field(key)}, "ChannelsConfig"); ok2 {
			cfg.Channels = decodeChannels(c, sub)
		}
	}
	if v, key, ok := configFields[2].getCI(o, idx); ok {
		if sub, ok2 := c.asObject(v, []PathPart{Field(key)}, "TranscriptionConfig"); ok2 {
			cfg.Transcription = decodeTranscription(c, sub)
		}
	}
	if v, key, ok := configFields[3].getCI(o, idx); ok {
		if sub, ok2 := c.asObject(v, []PathPart{Field(key)}, "ProvidersConfig"); ok2 {
			cfg.Providers = decodeProviders(c, sub)
		}
	}
	if v, key, ok := configFields[4].getCI(o, idx); ok {
		if sub, ok2 := c.asObject(v, []PathPart{Field(key)}, "ApiConfig"); ok2 {
			cfg.API = decodeAPI(c, sub)
		}
	}
	if v, key, ok := configFields[5].getCI(o, idx); ok {
		if sub, ok2 := c.asObject(v, []PathPart{Field(key)}, "GatewayConfig"); ok2 {
			cfg.Gateway = decodeGateway(c, sub)
		}
	}
	if v, key, ok := configFields[6].getCI(o, idx); ok {
		if sub, ok2 := c.asObject(v, []PathPart{Field(key)}, "ToolsConfig"); ok2 {
			cfg.Tools = decodeTools(c, sub)
		}
	}
	if v, key, ok := configFields[7].getCI(o, idx); ok {
		cfg.ModelPresets = decodeModelPresets(c, v, []PathPart{Field(key)})
	}

	// Config._validate_model_preset (schema.py:454-470): an "after" validator, so
	// it only runs when every field validated.
	if len(c.issues) == 0 {
		validateModelPreset(c, cfg)
	}
	return cfg, c.issues
}

// ---------------------------------------------------------------------------
// agents
// ---------------------------------------------------------------------------

var agentsFields = []fieldDef{{name: "defaults"}}

func decodeAgents(c *collector, o *jmap) AgentsConfig {
	out := AgentsConfig{Defaults: DefaultAgentDefaults()}
	if v, key, ok := agentsFields[0].get(o); ok {
		if sub, ok2 := c.asObject(v, o.child(key).path, "AgentDefaults"); ok2 {
			out.Defaults = decodeAgentDefaults(c, sub)
		}
	}
	return out
}

var agentDefaultsFields = []fieldDef{
	{name: "workspace"},
	{name: "model_preset"},
	{name: "model"},
	{name: "provider"},
	{name: "max_tokens"},
	{name: "context_window_tokens"},
	{name: "temperature"},
	{name: "fallback_models"},
	{name: "max_tool_iterations"},
	{name: "max_concurrent_subagents"},
	{name: "max_tool_result_chars"},
	{name: "provider_retry_mode"},
	{name: "tool_hint_max_length", aliases: []string{"toolHintMaxLength"}},
	{name: "reasoning_effort"},
	{name: "timezone"},
	{name: "timezone_mode"},
	{name: "bot_name"},
	{name: "bot_icon"},
	{name: "unified_session"},
	{name: "disabled_skills"},
	{name: "session_ttl_minutes", aliases: []string{"idleCompactAfterMinutes", "sessionTtlMinutes"}},
	{name: "idle_compact_check_interval_seconds"},
	{name: "dream"},
}

func decodeAgentDefaults(c *collector, o *jmap) AgentDefaults {
	// resolve_timezone model_validator(mode="before") (schema.py:159-173) runs on
	// the RAW mapping before any field is read.
	o = &jmap{m: resolveTimezone(o.m), path: o.path}

	d := DefaultAgentDefaults()
	d.Workspace = c.readString(o, agentDefaultsFields[0], d.Workspace)
	d.ModelPreset = c.readOptString(o, agentDefaultsFields[1], d.ModelPreset)
	d.Model = c.readString(o, agentDefaultsFields[2], d.Model)
	d.Provider = c.readString(o, agentDefaultsFields[3], d.Provider)
	d.MaxTokens = c.readInt(o, agentDefaultsFields[4], d.MaxTokens, intBound{}, intBound{})
	d.ContextWindowTokens = c.readInt(o, agentDefaultsFields[5], d.ContextWindowTokens, intBound{}, intBound{})
	d.Temperature = pyjson.Float(c.readFloat(o, agentDefaultsFields[6], float64(d.Temperature)))
	d.FallbackModels = decodeFallbackModels(c, o, agentDefaultsFields[7])
	d.MaxToolIterations = c.readInt(o, agentDefaultsFields[8], d.MaxToolIterations, intBound{}, intBound{})
	d.MaxConcurrentSubagent = c.readInt(o, agentDefaultsFields[9], d.MaxConcurrentSubagent, Ge(1), intBound{})
	d.MaxToolResultChars = c.readInt(o, agentDefaultsFields[10], d.MaxToolResultChars, intBound{}, intBound{})
	d.ProviderRetryMode = c.readLiteral(o, agentDefaultsFields[11], d.ProviderRetryMode, "standard", "persistent")
	d.ToolHintMaxLength = c.readInt(o, agentDefaultsFields[12], d.ToolHintMaxLength, Ge(20), Le(500))
	d.ReasoningEffort = c.readOptString(o, agentDefaultsFields[13], d.ReasoningEffort)
	d.Timezone = c.readString(o, agentDefaultsFields[14], d.Timezone)
	// validate_timezone field_validator (schema.py:175-184) rejects unknown zones.
	d.Timezone = validateTimezone(c, o, agentDefaultsFields[14], d.Timezone)
	d.TimezoneMode = c.readLiteral(o, agentDefaultsFields[15], d.TimezoneMode, "auto", "manual")
	d.BotName = c.readString(o, agentDefaultsFields[16], d.BotName)
	d.BotIcon = c.readString(o, agentDefaultsFields[17], d.BotIcon)
	d.UnifiedSession = c.readBool(o, agentDefaultsFields[18], d.UnifiedSession)
	d.DisabledSkills = c.readStringList(o, agentDefaultsFields[19], d.DisabledSkills)
	d.SessionTTLMinutes = c.readInt(o, agentDefaultsFields[20], d.SessionTTLMinutes, Ge(0), intBound{})
	d.IdleCompactCheckIntervalSecs = c.readInt(o, agentDefaultsFields[21], d.IdleCompactCheckIntervalSecs, Ge(0), intBound{})
	if v, key, ok := agentDefaultsFields[22].get(o); ok {
		if sub, ok2 := c.asObject(v, o.child(key).path, "DreamConfig"); ok2 {
			d.Dream = decodeDream(c, sub)
		}
	}
	return d
}

// resolveTimezone ports the `resolve_timezone` model_validator(mode="before")
// (schema.py:159-173).
//
//	timezone_mode = data.get("timezoneMode", data.get("timezone_mode"))
//	if timezone_mode is None:
//	    timezone_mode = "manual" if "timezone" in data else "auto"
//	    data["timezoneMode"] = timezone_mode
//	if timezone_mode == "auto":
//	    data["timezone"] = detect_system_timezone()
func resolveTimezone(raw map[string]any) map[string]any {
	data := make(map[string]any, len(raw)+2)
	for k, v := range raw {
		data[k] = v
	}
	mode, hasMode := data["timezoneMode"]
	if !hasMode {
		mode, hasMode = data["timezone_mode"]
	}
	if !hasMode || mode == nil {
		if _, hasTZ := data["timezone"]; hasTZ {
			mode = "manual"
		} else {
			mode = "auto"
		}
		data["timezoneMode"] = mode
	}
	if s, ok := mode.(string); ok && s == "auto" {
		data["timezone"] = detectSystemTimezone()
	}
	return data
}

var dreamFields = []fieldDef{
	{name: "enabled"},
	{name: "interval_h"},
	{name: "cron"},
	{name: "model_override", aliases: []string{"modelOverride", "model", "model_override"}},
}

func decodeDream(c *collector, o *jmap) DreamConfig {
	out := DreamConfig{Enabled: true, IntervalH: 2}
	out.Enabled = c.readBool(o, dreamFields[0], out.Enabled)
	out.IntervalH = c.readInt(o, dreamFields[1], out.IntervalH, Ge(1), intBound{})
	out.Cron = c.readOptString(o, dreamFields[2], out.Cron)
	out.ModelOverride = c.readOptString(o, dreamFields[3], out.ModelOverride)
	return out
}

var inlineFallbackFields = []fieldDef{
	{name: "model"},
	{name: "provider"},
	{name: "max_tokens"},
	{name: "context_window_tokens"},
	{name: "temperature"},
	{name: "reasoning_effort"},
}

// decodeFallbackModels ports `list[FallbackCandidate]` where
// `FallbackCandidate = str | InlineFallbackConfig` (schema.py:94).
//
// A pydantic union succeeds when EITHER member validates, so a well-formed inline
// object produces no error at all. Only when the inline branch also fails does
// pydantic report both members, with the `str` branch first (verified against
// pydantic 2.13.5).
func decodeFallbackModels(c *collector, o *jmap, f fieldDef) []FallbackCandidate {
	v, key, ok := f.get(o)
	if !ok {
		return []FallbackCandidate{}
	}
	path := appendPath(o.path, Field(key))
	if isJSONNull(v) {
		c.add(path, "list_type", "Input should be a valid list")
		return []FallbackCandidate{}
	}
	arr, isArr := v.([]any)
	if !isArr {
		c.add(path, "list_type", "Input should be a valid list")
		return []FallbackCandidate{}
	}
	out := make([]FallbackCandidate, 0, len(arr))
	for i, item := range arr {
		ipath := appendPath(path, Index(i))
		if s, isStr := item.(string); isStr {
			out = append(out, FallbackCandidate{Name: s})
			continue
		}

		// Parse the inline branch into a scratch collector so its issues can be
		// discarded when the branch succeeds.
		tmp := &collector{}
		var inline InlineFallbackConfig
		if m, isObj := item.(map[string]any); isObj {
			sub := &jmap{m: m, path: appendPath(ipath, Field("InlineFallbackConfig"))}
			inline.Model = tmp.readString(sub, inlineFallbackFields[0], "")
			if _, _, present := inlineFallbackFields[0].get(sub); !present {
				tmp.add(appendPath(sub.path, Field("model")), "missing", "Field required")
			}
			inline.Provider = tmp.readString(sub, inlineFallbackFields[1], "")
			if _, _, present := inlineFallbackFields[1].get(sub); !present {
				tmp.add(appendPath(sub.path, Field("provider")), "missing", "Field required")
			}
			inline.MaxTokens = readOptInt(tmp, sub, inlineFallbackFields[2])
			inline.ContextWindowTokens = readOptInt(tmp, sub, inlineFallbackFields[3])
			inline.Temperature = optPyFloat(readOptFloat(tmp, sub, inlineFallbackFields[4]))
			inline.ReasoningEffort = tmp.readOptString(sub, inlineFallbackFields[5], nil)
		} else {
			tmp.add(appendPath(ipath, Field("InlineFallbackConfig")), "model_type",
				"Input should be a valid dictionary or instance of InlineFallbackConfig")
		}

		if len(tmp.issues) == 0 {
			out = append(out, FallbackCandidate{Inline: &inline})
			continue
		}
		c.add(appendPath(ipath, Field("str")), "string_type", "Input should be a valid string")
		c.issues = append(c.issues, tmp.issues...)
	}
	return out
}

func readOptInt(c *collector, o *jmap, f fieldDef) *int {
	v, key, ok := f.get(o)
	if !ok || isJSONNull(v) {
		return nil
	}
	iv, ok2, code, msg := asInt(v)
	if !ok2 {
		c.add(c.at(o, key), code, msg)
		return nil
	}
	return &iv
}

func readOptFloat(c *collector, o *jmap, f fieldDef) *float64 {
	v, key, ok := f.get(o)
	if !ok || isJSONNull(v) {
		return nil
	}
	fv, ok2, code, msg := asFloat(v)
	if !ok2 {
		c.add(c.at(o, key), code, msg)
		return nil
	}
	return &fv
}

// ---------------------------------------------------------------------------
// channels / transcription
// ---------------------------------------------------------------------------

var channelsFields = []fieldDef{
	{name: "send_progress"},
	{name: "send_tool_hints"},
	{name: "show_reasoning"},
	{name: "extract_document_text"},
	{name: "send_max_retries"},
	{name: "transcription_provider"},
	{name: "transcription_language"},
}

func decodeChannels(c *collector, o *jmap) ChannelsConfig {
	out := DefaultChannelsConfig()
	out.SendProgress = c.readBool(o, channelsFields[0], out.SendProgress)
	out.SendToolHints = c.readBool(o, channelsFields[1], out.SendToolHints)
	out.ShowReasoning = c.readBool(o, channelsFields[2], out.ShowReasoning)
	out.ExtractDocumentText = c.readBool(o, channelsFields[3], out.ExtractDocumentText)
	out.SendMaxRetries = c.readInt(o, channelsFields[4], out.SendMaxRetries, Ge(0), Le(10))
	out.TranscriptionProvider = c.readString(o, channelsFields[5], out.TranscriptionProvider)
	out.TranscriptionLanguage = c.readPattern(o, channelsFields[6], out.TranscriptionLanguage,
		"^[a-z]{2,3}$", reLangCode)

	// extra="allow": everything not consumed by a declared field is preserved.
	for _, k := range sortedKeys(o.m) {
		if !anyFieldAccepts(channelsFields, k) {
			if out.Extra == nil {
				out.Extra = map[string]any{}
			}
			out.Extra[k] = o.m[k]
		}
	}
	return out
}

var transcriptionFields = []fieldDef{
	{name: "enabled"},
	{name: "provider"},
	{name: "model"},
	{name: "language"},
	{name: "max_duration_sec"},
	{name: "max_upload_mb"},
}

// reLangCode ports `pattern=r"^[a-z]{2,3}$"` (schema.py:39, schema.py:48).
var reLangCode = regexp.MustCompile(`^[a-z]{2,3}$`)

func decodeTranscription(c *collector, o *jmap) TranscriptionConfig {
	out := DefaultTranscriptionConfig()
	out.Enabled = c.readBool(o, transcriptionFields[0], out.Enabled)
	out.Provider = c.readOptString(o, transcriptionFields[1], out.Provider)
	out.Model = c.readOptString(o, transcriptionFields[2], out.Model)
	out.Language = c.readPattern(o, transcriptionFields[3], out.Language, "^[a-z]{2,3}$", reLangCode)
	out.MaxDurationSec = c.readInt(o, transcriptionFields[4], out.MaxDurationSec, Ge(1), Le(600))
	out.MaxUploadMB = c.readInt(o, transcriptionFields[5], out.MaxUploadMB, Ge(1), Le(100))
	return out
}

func anyFieldAccepts(fields []fieldDef, key string) bool {
	for _, f := range fields {
		if f.accepts(key) {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// providers
// ---------------------------------------------------------------------------

// providerSlot binds a ProvidersConfig field to its accessor.
type providerSlot struct {
	field   fieldDef
	get     func(*ProvidersConfig) *ProviderConfig
	bedrock bool
}

var providerSlots = []providerSlot{
	{fieldDef{name: "custom"}, func(p *ProvidersConfig) *ProviderConfig { return &p.Custom }, false},
	{fieldDef{name: "azure_openai"}, func(p *ProvidersConfig) *ProviderConfig { return &p.AzureOpenAI }, false},
	{fieldDef{name: "bedrock"}, func(p *ProvidersConfig) *ProviderConfig { return &p.Bedrock.ProviderConfig }, true},
	{fieldDef{name: "anthropic"}, func(p *ProvidersConfig) *ProviderConfig { return &p.Anthropic }, false},
	{fieldDef{name: "openai"}, func(p *ProvidersConfig) *ProviderConfig { return &p.OpenAI }, false},
	{fieldDef{name: "openrouter"}, func(p *ProvidersConfig) *ProviderConfig { return &p.OpenRouter }, false},
	{fieldDef{name: "orcarouter"}, func(p *ProvidersConfig) *ProviderConfig { return &p.OrcaRouter }, false},
	{fieldDef{name: "assemblyai"}, func(p *ProvidersConfig) *ProviderConfig { return &p.AssemblyAI }, false},
	{fieldDef{name: "huggingface"}, func(p *ProvidersConfig) *ProviderConfig { return &p.HuggingFace }, false},
	{fieldDef{name: "skywork"}, func(p *ProvidersConfig) *ProviderConfig { return &p.Skywork }, false},
	{fieldDef{name: "deepseek"}, func(p *ProvidersConfig) *ProviderConfig { return &p.DeepSeek }, false},
	{fieldDef{name: "groq"}, func(p *ProvidersConfig) *ProviderConfig { return &p.Groq }, false},
	{fieldDef{name: "zhipu"}, func(p *ProvidersConfig) *ProviderConfig { return &p.Zhipu }, false},
	{fieldDef{name: "dashscope"}, func(p *ProvidersConfig) *ProviderConfig { return &p.DashScope }, false},
	{fieldDef{name: "modelscope"}, func(p *ProvidersConfig) *ProviderConfig { return &p.ModelScope }, false},
	{fieldDef{name: "vllm"}, func(p *ProvidersConfig) *ProviderConfig { return &p.VLLM }, false},
	{fieldDef{name: "ollama"}, func(p *ProvidersConfig) *ProviderConfig { return &p.Ollama }, false},
	{fieldDef{name: "lm_studio"}, func(p *ProvidersConfig) *ProviderConfig { return &p.LMStudio }, false},
	{fieldDef{name: "atomic_chat"}, func(p *ProvidersConfig) *ProviderConfig { return &p.AtomicChat }, false},
	{fieldDef{name: "ovms"}, func(p *ProvidersConfig) *ProviderConfig { return &p.OVMS }, false},
	{fieldDef{name: "gemini"}, func(p *ProvidersConfig) *ProviderConfig { return &p.Gemini }, false},
	{fieldDef{name: "moonshot"}, func(p *ProvidersConfig) *ProviderConfig { return &p.Moonshot }, false},
	{fieldDef{name: "kimi_coding"}, func(p *ProvidersConfig) *ProviderConfig { return &p.KimiCoding }, false},
	{fieldDef{name: "minimax"}, func(p *ProvidersConfig) *ProviderConfig { return &p.MiniMax }, false},
	{fieldDef{name: "minimax_anthropic"}, func(p *ProvidersConfig) *ProviderConfig { return &p.MiniMaxAnthropic }, false},
	{fieldDef{name: "mistral"}, func(p *ProvidersConfig) *ProviderConfig { return &p.Mistral }, false},
	{fieldDef{name: "stepfun"}, func(p *ProvidersConfig) *ProviderConfig { return &p.StepFun }, false},
	{fieldDef{name: "xiaomi_mimo"}, func(p *ProvidersConfig) *ProviderConfig { return &p.XiaomiMiMo }, false},
	{fieldDef{name: "longcat"}, func(p *ProvidersConfig) *ProviderConfig { return &p.LongCat }, false},
	{fieldDef{name: "ant_ling"}, func(p *ProvidersConfig) *ProviderConfig { return &p.AntLing }, false},
	{fieldDef{name: "aihubmix"}, func(p *ProvidersConfig) *ProviderConfig { return &p.AIHubMix }, false},
	{fieldDef{name: "siliconflow"}, func(p *ProvidersConfig) *ProviderConfig { return &p.SiliconFlow }, false},
	{fieldDef{name: "edenai"}, func(p *ProvidersConfig) *ProviderConfig { return &p.EdenAI }, false},
	{fieldDef{name: "novita"}, func(p *ProvidersConfig) *ProviderConfig { return &p.Novita }, false},
	{fieldDef{name: "volcengine"}, func(p *ProvidersConfig) *ProviderConfig { return &p.VolcEngine }, false},
	{fieldDef{name: "volcengine_coding_plan"}, func(p *ProvidersConfig) *ProviderConfig { return &p.VolcEngineCodingPlan }, false},
	{fieldDef{name: "byteplus"}, func(p *ProvidersConfig) *ProviderConfig { return &p.BytePlus }, false},
	{fieldDef{name: "byteplus_coding_plan"}, func(p *ProvidersConfig) *ProviderConfig { return &p.BytePlusCodingPlan }, false},
	{fieldDef{name: "openai_codex"}, func(p *ProvidersConfig) *ProviderConfig { return &p.OpenAICodex }, false},
	{fieldDef{name: "xai_grok"}, func(p *ProvidersConfig) *ProviderConfig { return &p.XAIGrok }, false},
	{fieldDef{name: "github_copilot"}, func(p *ProvidersConfig) *ProviderConfig { return &p.GitHubCopilot }, false},
	{fieldDef{name: "qianfan"}, func(p *ProvidersConfig) *ProviderConfig { return &p.Qianfan }, false},
	{fieldDef{name: "nvidia"}, func(p *ProvidersConfig) *ProviderConfig { return &p.NVIDIA }, false},
	{fieldDef{name: "opencode"}, func(p *ProvidersConfig) *ProviderConfig { return &p.OpenCode }, false},
	{fieldDef{name: "opencode_zen"}, func(p *ProvidersConfig) *ProviderConfig { return &p.OpenCodeZen }, false},
	{fieldDef{name: "opencode_go"}, func(p *ProvidersConfig) *ProviderConfig { return &p.OpenCodeGo }, false},
}

var providerFields = []fieldDef{
	{name: "display_name"},
	{name: "api_key"},
	{name: "api_base"},
	{name: "api_type"},
	{name: "extra_headers"},
	{name: "extra_body"},
	{name: "extra_query"},
	{name: "proxy"},
	{name: "thinking_style"},
}

// validThinkingStyles mirrors ProviderConfig._VALID_THINKING_STYLES (schema.py:213-217).
var validThinkingStyles = []string{"thinking_type", "enable_thinking", "reasoning_split"}

func decodeProviders(c *collector, o *jmap) ProvidersConfig {
	out := DefaultProvidersConfig()
	for _, slot := range providerSlots {
		v, key, ok := slot.field.get(o)
		if !ok {
			continue
		}
		if sub, ok2 := c.asObject(v, o.child(key).path, "ProviderConfig"); ok2 {
			pc := decodeProvider(c, sub)
			*slot.get(&out) = pc
			if slot.bedrock {
				out.Bedrock.Region = c.readOptString(sub, fieldDef{name: "region"}, nil)
				out.Bedrock.Profile = c.readOptString(sub, fieldDef{name: "profile"}, nil)
			}
		}
	}

	// extra="allow": unknown keys become custom providers.
	for _, k := range sortedKeys(o.m) {
		if providerSlotAccepts(k) {
			continue
		}
		// convert_extra_providers (schema.py:296-310): a name that collides with a
		// built-in registry provider is rejected.
		if name, found := findBuiltinProvider(k); found {
			c.add(o.path, "value_error",
				"providers."+k+" conflicts with built-in provider '"+name+"'")
			continue
		}
		v := o.m[k]
		m, isObj := v.(map[string]any)
		if !isObj {
			// Non-dict extras are left untouched by the reference and are skipped
			// by the api_type scope check.
			if out.ExtraProviders == nil {
				out.ExtraProviders = map[string]ProviderConfig{}
			}
			out.ExtraProviders[k] = ProviderConfig{APIType: "auto"}
			continue
		}
		sub := &jmap{m: m, path: appendPath(o.path, Field(k))}
		if out.ExtraProviders == nil {
			out.ExtraProviders = map[string]ProviderConfig{}
		}
		out.ExtraProviders[k] = decodeProvider(c, sub)
	}

	// _validate_api_type_scope (schema.py:312-323): api_type is only honored for
	// providers.openai.
	if len(c.issues) == 0 {
		for _, slot := range providerSlots {
			if slot.field.name == "openai" {
				continue
			}
			if p := slot.get(&out); p != nil && p.APIType != "auto" {
				c.add(o.path, "value_error",
					"providers.<name>.api_type is only supported for providers.openai")
				break
			}
		}
		if len(c.issues) == 0 {
			for _, k := range sortedKeys2(out.ExtraProviders) {
				if out.ExtraProviders[k].APIType != "auto" {
					c.add(o.path, "value_error",
						"providers.<name>.api_type is only supported for providers.openai")
					break
				}
			}
		}
	}
	return out
}

func providerSlotAccepts(key string) bool {
	for _, s := range providerSlots {
		if s.field.accepts(key) {
			return true
		}
	}
	return false
}

func sortedKeys2[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func decodeProvider(c *collector, o *jmap) ProviderConfig {
	out := DefaultProviderConfig()
	out.DisplayName = c.readOptString(o, providerFields[0], out.DisplayName)
	out.APIKey = c.readOptString(o, providerFields[1], out.APIKey)
	out.APIBase = c.readOptString(o, providerFields[2], out.APIBase)
	out.APIType = c.readLiteral(o, providerFields[3], out.APIType, "auto", "chat_completions", "responses")
	out.ExtraHeaders = readExtraHeaders(c, o, providerFields[4])
	out.ExtraBody = c.asAnyMap(mustGet(o, providerFields[5]), o.child(providerFields[5].canonical()).path)
	out.ExtraQuery = readExtraQuery(c, o, providerFields[6])
	out.Proxy = c.readOptString(o, providerFields[7], out.Proxy)
	out.ThinkingStyle = readThinkingStyle(c, o, providerFields[8])
	return out
}

func mustGet(o *jmap, f fieldDef) any {
	v, _, ok := f.get(o)
	if !ok {
		return nil
	}
	return v
}

// readExtraHeaders ports `extra_headers: dict[str, str] | None = None`.
func readExtraHeaders(c *collector, o *jmap, f fieldDef) map[string]string {
	v, key, ok := f.get(o)
	if !ok {
		return nil
	}
	return c.asStringMap(v, c.at(o, key), true)
}

// readExtraQuery ports `extra_query: dict[str, str] | None = None`.
func readExtraQuery(c *collector, o *jmap, f fieldDef) map[string]string {
	v, key, ok := f.get(o)
	if !ok {
		return nil
	}
	return c.asStringMap(v, c.at(o, key), true)
}

// readThinkingStyle ports the `_validate_thinking_style` field_validator
// (schema.py:219-230): falsy values (None or "") are valid and pass through.
func readThinkingStyle(c *collector, o *jmap, f fieldDef) *string {
	v, key, ok := f.get(o)
	if !ok {
		return nil
	}
	if isJSONNull(v) {
		return nil
	}
	s, ok2, code, msg := asString(v)
	if !ok2 {
		c.add(c.at(o, key), code, msg)
		return nil
	}
	if s == "" {
		return &s
	}
	for _, valid := range validThinkingStyles {
		if s == valid {
			return &s
		}
	}
	c.add(c.at(o, key), "value_error",
		"Invalid thinking_style "+s+". Must be one of: 'thinking_type', 'enable_thinking', 'reasoning_split' (or empty/omitted).")
	return nil
}

// builtinProviderNames mirrors the `name=` values of PROVIDERS in
// nanobot/providers/registry.py (47 entries). Duplicated as a static list so
// this package keeps zero imports from the frozen provider package, matching
// how schema.py:210-217 duplicates _THINKING_STYLE_MAP to avoid a cycle.
var builtinProviderNames = []string{
	"custom", "azure_openai", "bedrock", "openrouter", "orcarouter", "edenai",
	"opencode", "opencode_zen", "opencode_go", "huggingface", "skywork",
	"aihubmix", "siliconflow", "novita", "volcengine", "volcengine_coding_plan",
	"byteplus", "byteplus_coding_plan", "anthropic", "openai", "openai_codex",
	"xai_grok", "github_copilot", "deepseek", "gemini", "zhipu", "dashscope",
	"modelscope", "moonshot", "kimi_coding", "minimax", "minimax_anthropic",
	"mistral", "stepfun", "xiaomi_mimo", "longcat", "ant_ling", "vllm",
	"ollama", "lm_studio", "atomic_chat", "ovms", "nvidia", "groq",
	"assemblyai", "qianfan",
}

// findBuiltinProvider ports `find_by_name(key)` (providers/registry.py:794-800):
// to_snake(name.replace("-", "_")) then an exact match against the registry.
func findBuiltinProvider(key string) (string, bool) {
	normalized := toSnake(strings.ReplaceAll(key, "-", "_"))
	for _, n := range builtinProviderNames {
		if n == normalized {
			return n, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// api / gateway
// ---------------------------------------------------------------------------

var apiFields = []fieldDef{
	{name: "host"},
	{name: "port"},
	{name: "timeout"},
	{name: "api_key"},
}

func decodeAPI(c *collector, o *jmap) ApiConfig {
	out := ApiConfig{Host: "127.0.0.1", Port: 8900, Timeout: 120.0, APIKey: ""}
	out.Host = c.readString(o, apiFields[0], out.Host)
	out.Port = c.readInt(o, apiFields[1], out.Port, intBound{}, intBound{})
	out.Timeout = pyjson.Float(c.readFloat(o, apiFields[2], float64(out.Timeout)))
	out.APIKey = c.readString(o, apiFields[3], out.APIKey)

	// ApiConfig.wildcard_host_requires_auth (schema.py:341-350).
	if len(c.issues) == 0 {
		if (out.Host == "0.0.0.0" || out.Host == "::") && strings.TrimSpace(out.APIKey) == "" {
			c.add(o.path, "value_error",
				"host is 0.0.0.0 (all interfaces) but api_key is not set - set api.api_key to prevent unauthenticated access")
		}
	}
	return out
}

var gatewayFields = []fieldDef{
	{name: "host"},
	{name: "port"},
	{name: "restart_mode"},
	{name: "heartbeat"},
}

var heartbeatFields = []fieldDef{
	{name: "enabled"},
	{name: "interval_s"},
}

func decodeGateway(c *collector, o *jmap) GatewayConfig {
	out := GatewayConfig{
		Host: "127.0.0.1", Port: 18790, RestartMode: "auto",
		Heartbeat: HeartbeatConfig{Enabled: true, IntervalS: 30 * 60},
	}
	out.Host = c.readString(o, gatewayFields[0], out.Host)
	out.Port = c.readInt(o, gatewayFields[1], out.Port, intBound{}, intBound{})
	out.RestartMode = c.readLiteral(o, gatewayFields[2], out.RestartMode, "auto", "exec", "spawn", "exit")
	if v, key, ok := gatewayFields[3].get(o); ok {
		if sub, ok2 := c.asObject(v, o.child(key).path, "HeartbeatConfig"); ok2 {
			out.Heartbeat.Enabled = c.readBool(sub, heartbeatFields[0], out.Heartbeat.Enabled)
			out.Heartbeat.IntervalS = c.readInt(sub, heartbeatFields[1], out.Heartbeat.IntervalS, intBound{}, intBound{})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// tools
// ---------------------------------------------------------------------------

var toolsFields = []fieldDef{
	{name: "web"},
	{name: "exec"},
	{name: "file"},
	{name: "cli_apps"},
	{name: "my"},
	{name: "image_generation"},
	{name: "max_session_messages_per_minute"},
	{name: "restrict_to_workspace"},
	{name: "webui_allow_local_service_access", aliases: []string{
		"webuiAllowLocalServiceAccess",
		"webui_allow_local_service_access",
		"allowLocalPreviewAccess",
		"allow_local_preview_access",
	}},
	{name: "webui_allow_remote_package_install", aliases: []string{
		"webuiAllowRemotePackageInstall",
		"webui_allow_remote_package_install",
	}},
	{name: "mcp_servers"},
	{name: "ssrf_whitelist"},
}

func decodeTools(c *collector, o *jmap) ToolsConfig {
	out := DefaultToolsConfig()
	if v, key, ok := toolsFields[0].get(o); ok {
		if sub, ok2 := c.asObject(v, o.child(key).path, "WebToolsConfig"); ok2 {
			out.Web = decodeWebTools(c, sub)
		}
	}
	if v, key, ok := toolsFields[1].get(o); ok {
		if sub, ok2 := c.asObject(v, o.child(key).path, "ExecToolConfig"); ok2 {
			out.Exec = decodeExec(c, sub)
		}
	}
	if v, key, ok := toolsFields[2].get(o); ok {
		if sub, ok2 := c.asObject(v, o.child(key).path, "FileToolsConfig"); ok2 {
			out.File.Enable = c.readBool(sub, fieldDef{name: "enable"}, out.File.Enable)
		}
	}
	if v, key, ok := toolsFields[3].get(o); ok {
		if sub, ok2 := c.asObject(v, o.child(key).path, "CliAppsToolConfig"); ok2 {
			out.CliApps = decodeCliApps(c, sub)
		}
	}
	if v, key, ok := toolsFields[4].get(o); ok {
		if sub, ok2 := c.asObject(v, o.child(key).path, "MyToolConfig"); ok2 {
			out.My.Enable = c.readBool(sub, fieldDef{name: "enable"}, out.My.Enable)
			out.My.AllowSet = c.readBool(sub, fieldDef{name: "allow_set"}, out.My.AllowSet)
		}
	}
	if v, key, ok := toolsFields[5].get(o); ok {
		if sub, ok2 := c.asObject(v, o.child(key).path, "ImageGenerationToolConfig"); ok2 {
			out.ImageGeneration = decodeImageGeneration(c, sub)
		}
	}
	out.MaxSessionMessagesPerMinute = c.readInt(o, toolsFields[6], out.MaxSessionMessagesPerMinute, Ge(1), intBound{})
	out.RestrictToWorkspace = c.readBool(o, toolsFields[7], out.RestrictToWorkspace)
	out.WebUIAllowLocalServiceAccess = c.readBool(o, toolsFields[8], out.WebUIAllowLocalServiceAccess)
	out.WebUIAllowRemotePackageInstall = c.readBool(o, toolsFields[9], out.WebUIAllowRemotePackageInstall)
	out.MCPServers = decodeMCPServers(c, o, toolsFields[10])
	out.SSRFWhitelist = c.readStringList(o, toolsFields[11], out.SSRFWhitelist)
	return out
}

func decodeMCPServers(c *collector, o *jmap, f fieldDef) map[string]MCPServerConfig {
	v, key, ok := f.get(o)
	if !ok {
		return map[string]MCPServerConfig{}
	}
	path := c.at(o, key)
	if isJSONNull(v) {
		c.add(path, "dict_type", "Input should be a valid dictionary")
		return map[string]MCPServerConfig{}
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		c.add(path, "dict_type", "Input should be a valid dictionary")
		return map[string]MCPServerConfig{}
	}
	out := make(map[string]MCPServerConfig, len(m))
	for _, k := range sortedKeys(m) {
		sub, ok2 := c.asObject(m[k], appendPath(path, Field(k)), "MCPServerConfig")
		if !ok2 {
			continue
		}
		out[k] = decodeMCPServer(c, sub)
	}
	return out
}

var mcpFields = []fieldDef{
	{name: "type"},
	{name: "auth"},
	{name: "command"},
	{name: "args"},
	{name: "env"},
	{name: "cwd"},
	{name: "url"},
	{name: "headers"},
	{name: "tool_timeout"},
	{name: "enabled_tools"},
}

func decodeMCPServer(c *collector, o *jmap) MCPServerConfig {
	out := DefaultMCPServerConfig()
	out.Type = c.readOptLiteral(o, mcpFields[0], out.Type, "stdio", "sse", "streamableHttp")
	out.Auth = c.readOptLiteral(o, mcpFields[1], out.Auth, "oauth")
	out.Command = c.readString(o, mcpFields[2], out.Command)
	out.Args = c.readStringList(o, mcpFields[3], out.Args)
	if v, key, ok := mcpFields[4].get(o); ok {
		out.Env = c.asStringMap(v, c.at(o, key), false)
		if out.Env == nil {
			out.Env = map[string]string{}
		}
	}
	out.CWD = c.readString(o, mcpFields[5], out.CWD)
	out.URL = c.readString(o, mcpFields[6], out.URL)
	if v, key, ok := mcpFields[7].get(o); ok {
		out.Headers = c.asStringMap(v, c.at(o, key), false)
		if out.Headers == nil {
			out.Headers = map[string]string{}
		}
	}
	out.ToolTimeout = c.readInt(o, mcpFields[8], out.ToolTimeout, intBound{}, intBound{})
	out.EnabledTools = c.readStringList(o, mcpFields[9], out.EnabledTools)
	return out
}

var webToolsFields = []fieldDef{
	{name: "enable"},
	{name: "proxy"},
	{name: "user_agent"},
	{name: "search"},
	{name: "fetch"},
}

var webSearchFields = []fieldDef{
	{name: "provider"},
	{name: "api_key"},
	{name: "base_url"},
	{name: "max_results"},
	{name: "timeout"},
}

func decodeWebTools(c *collector, o *jmap) WebToolsConfig {
	out := DefaultToolsConfig().Web
	out.Enable = c.readBool(o, webToolsFields[0], out.Enable)
	out.Proxy = c.readOptString(o, webToolsFields[1], out.Proxy)
	out.UserAgent = c.readOptString(o, webToolsFields[2], out.UserAgent)
	if v, key, ok := webToolsFields[3].get(o); ok {
		if sub, ok2 := c.asObject(v, o.child(key).path, "WebSearchConfig"); ok2 {
			out.Search.Provider = c.readString(sub, webSearchFields[0], out.Search.Provider)
			out.Search.APIKey = c.readString(sub, webSearchFields[1], out.Search.APIKey)
			out.Search.BaseURL = c.readString(sub, webSearchFields[2], out.Search.BaseURL)
			out.Search.MaxResults = c.readInt(sub, webSearchFields[3], out.Search.MaxResults, intBound{}, intBound{})
			out.Search.Timeout = c.readInt(sub, webSearchFields[4], out.Search.Timeout, intBound{}, intBound{})
		}
	}
	if v, key, ok := webToolsFields[4].get(o); ok {
		if sub, ok2 := c.asObject(v, o.child(key).path, "WebFetchConfig"); ok2 {
			out.Fetch.UseJinaReader = c.readBool(sub, fieldDef{name: "use_jina_reader"}, out.Fetch.UseJinaReader)
		}
	}
	return out
}

var execFields = []fieldDef{
	{name: "enable"},
	{name: "timeout"},
	{name: "path_prepend"},
	{name: "path_append"},
	{name: "sandbox"},
	{name: "sandbox_ro_binds"},
	{name: "sandbox_rw_binds"},
	{name: "allowed_env_keys"},
	{name: "allow_patterns"},
	{name: "deny_patterns"},
}

func decodeExec(c *collector, o *jmap) ExecToolConfig {
	out := DefaultToolsConfig().Exec
	out.Enable = c.readBool(o, execFields[0], out.Enable)
	out.Timeout = c.readInt(o, execFields[1], out.Timeout, Ge(0), intBound{})
	out.PathPrepend = c.readString(o, execFields[2], out.PathPrepend)
	out.PathAppend = c.readString(o, execFields[3], out.PathAppend)
	out.Sandbox = c.readString(o, execFields[4], out.Sandbox)
	out.SandboxROBinds = c.readStringList(o, execFields[5], out.SandboxROBinds)
	out.SandboxRWBinds = c.readStringList(o, execFields[6], out.SandboxRWBinds)
	out.AllowedEnvKeys = c.readStringList(o, execFields[7], out.AllowedEnvKeys)
	out.AllowPatterns = c.readStringList(o, execFields[8], out.AllowPatterns)
	out.DenyPatterns = c.readStringList(o, execFields[9], out.DenyPatterns)
	return out
}

var cliAppsFields = []fieldDef{
	{name: "enable"},
	{name: "install_timeout"},
	{name: "run_timeout"},
	{name: "catalog_ttl_seconds"},
}

func decodeCliApps(c *collector, o *jmap) CliAppsToolConfig {
	out := DefaultToolsConfig().CliApps
	out.Enable = c.readBool(o, cliAppsFields[0], out.Enable)
	out.InstallTimeout = c.readInt(o, cliAppsFields[1], out.InstallTimeout, Ge(1), Le(3600))
	out.RunTimeout = c.readInt(o, cliAppsFields[2], out.RunTimeout, Ge(1), Le(600))
	out.CatalogTTLSeconds = c.readInt(o, cliAppsFields[3], out.CatalogTTLSeconds, Ge(60), Le(86400))
	return out
}

var imageGenFields = []fieldDef{
	{name: "enabled"},
	{name: "provider"},
	{name: "model"},
	{name: "default_aspect_ratio"},
	{name: "default_image_size"},
	{name: "max_images_per_turn"},
	{name: "save_dir"},
}

func decodeImageGeneration(c *collector, o *jmap) ImageGenerationToolConfig {
	out := DefaultToolsConfig().ImageGeneration
	out.Enabled = c.readBool(o, imageGenFields[0], out.Enabled)
	out.Provider = c.readString(o, imageGenFields[1], out.Provider)
	out.Model = c.readString(o, imageGenFields[2], out.Model)
	out.DefaultAspectRatio = c.readString(o, imageGenFields[3], out.DefaultAspectRatio)
	out.DefaultImageSize = c.readString(o, imageGenFields[4], out.DefaultImageSize)
	out.MaxImagesPerTurn = c.readInt(o, imageGenFields[5], out.MaxImagesPerTurn, Ge(1), Le(8))
	out.SaveDir = c.readString(o, imageGenFields[6], out.SaveDir)
	return out
}

// ---------------------------------------------------------------------------
// model presets
// ---------------------------------------------------------------------------

var modelPresetFields = []fieldDef{
	{name: "model"},
	{name: "provider"},
	{name: "max_tokens"},
	{name: "context_window_tokens"},
	{name: "temperature"},
	{name: "reasoning_effort"},
}

func decodeModelPresets(c *collector, v any, path []PathPart) map[string]ModelPresetConfig {
	if isJSONNull(v) {
		c.add(path, "dict_type", "Input should be a valid dictionary")
		return map[string]ModelPresetConfig{}
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		c.add(path, "dict_type", "Input should be a valid dictionary")
		return map[string]ModelPresetConfig{}
	}
	out := make(map[string]ModelPresetConfig, len(m))
	for _, k := range sortedKeys(m) {
		sub, ok := c.asObject(m[k], appendPath(path, Field(k)), "ModelPresetConfig")
		if !ok {
			continue
		}
		p := ModelPresetConfig{Provider: "auto", MaxTokens: 8192, ContextWindowTokens: 200_000, Temperature: 0.1}
		p.Model = c.readString(sub, modelPresetFields[0], p.Model)
		if _, _, present := modelPresetFields[0].get(sub); !present {
			c.add(appendPath(sub.path, Field("model")), "missing", "Field required")
		}
		p.Provider = c.readString(sub, modelPresetFields[1], p.Provider)
		p.MaxTokens = c.readInt(sub, modelPresetFields[2], p.MaxTokens, intBound{}, intBound{})
		p.ContextWindowTokens = c.readInt(sub, modelPresetFields[3], p.ContextWindowTokens, intBound{}, intBound{})
		p.Temperature = pyjson.Float(c.readFloat(sub, modelPresetFields[4], float64(p.Temperature)))
		p.ReasoningEffort = c.readOptString(sub, modelPresetFields[5], p.ReasoningEffort)
		out[k] = p
	}
	return out
}

// ---------------------------------------------------------------------------
// Cross-model validation
// ---------------------------------------------------------------------------

// validateModelPreset ports Config._validate_model_preset (schema.py:454-470).
func validateModelPreset(c *collector, cfg *Config) {
	if _, reserved := cfg.ModelPresets["default"]; reserved {
		c.add(nil, "value_error", "model_preset name 'default' is reserved for agents.defaults")
		return
	}
	if name := cfg.Agents.Defaults.ModelPreset; name != nil && *name != "" && *name != "default" {
		if _, ok := cfg.ModelPresets[*name]; !ok {
			c.add(nil, "value_error", "model_preset '"+*name+"' not found in model_presets")
			return
		}
	}
	if name := cfg.Agents.Defaults.Dream.ModelOverride; name != nil && *name != "" && *name != "default" {
		if _, ok := cfg.ModelPresets[*name]; !ok {
			c.add(nil, "value_error", "Dream model preset '"+*name+"' not found in model_presets")
			return
		}
	}
	for _, fb := range cfg.Agents.Defaults.FallbackModels {
		if fb.Inline != nil {
			continue
		}
		if _, ok := cfg.ModelPresets[fb.Name]; !ok {
			c.add(nil, "value_error", "fallback_models entry '"+fb.Name+"' not found in model_presets")
			return
		}
	}
}

// optPyFloat converts an optional float64 into the Python-formatting float
// type, preserving nil.
func optPyFloat(f *float64) *pyjson.Float {
	if f == nil {
		return nil
	}
	out := pyjson.Float(*f)
	return &out
}
