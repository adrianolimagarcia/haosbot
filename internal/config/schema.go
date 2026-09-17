package config

// Configuration schema.
//
// Port of upstream/nanobot/nanobot/config/schema.py (707 lines) plus the tool
// sub-configs that live next to their tool implementations:
//   - nanobot/agent/tools/web.py:70-91        (WebToolsConfig, WebSearchConfig, WebFetchConfig)
//   - nanobot/agent/tools/shell.py:94-105     (ExecToolConfig)
//   - nanobot/agent/tools/filesystem.py:28-31 (FileToolsConfig)
//   - nanobot/agent/tools/cli_apps.py:27-33   (CliAppsToolConfig)
//   - nanobot/agent/tools/self.py:30-33       (MyToolConfig)
//   - nanobot/agent/tools/image_generation.py:49-57 (ImageGenerationToolConfig)
//
// Every type here descends (directly or transitively) from `Base`, which sets
// alias_generator=to_camel + populate_by_name=True (nanobot/config_base.py:12-15).
// The json tags below are therefore the SERIALIZATION aliases, i.e. either the
// field's explicit `serialization_alias` or to_camel(field_name).

import (
	"encoding/json"
	"fmt"
	"github.com/adrianolimagarcia/nanobot-go/internal/pyjson"
	"sort"
)

// ---------------------------------------------------------------------------
// Root
// ---------------------------------------------------------------------------

// Config is the root configuration, mirroring schema.py:422-438.
//
// Note: in Python `Config` extends `BaseSettings` (not `Base`), so the root
// model has NO alias generator; its field names are already single words and
// `model_presets` carries an explicit AliasChoices("modelPresets","model_presets")
// with serialization_alias="modelPresets".
type Config struct {
	Agents        AgentsConfig                 `json:"agents"`
	Channels      ChannelsConfig               `json:"channels"`
	Transcription TranscriptionConfig          `json:"transcription"`
	Providers     ProvidersConfig              `json:"providers"`
	API           ApiConfig                    `json:"api"`
	Gateway       GatewayConfig                `json:"gateway"`
	Tools         ToolsConfig                  `json:"tools"`
	ModelPresets  map[string]ModelPresetConfig `json:"modelPresets"`

	// sourcePath mirrors the private `_source_path` PrivateAttr (schema.py:425),
	// bound by Config.bind_source_path (schema.py:445-447).
	sourcePath string
}

// SourcePath returns the config file this instance was loaded from, or "" when
// the instance was built programmatically.
func (c *Config) SourcePath() string { return c.sourcePath }

// BindSourcePath records the config file that owns instance-level runtime data.
// Port of schema.py:445-447 (expanduser + resolve(strict=False)).
func (c *Config) BindSourcePath(path string) { c.sourcePath = resolvePath(path) }

// RuntimeDataDir returns the active instance data directory when loaded from a
// config path. Port of schema.py:449-452.
func (c *Config) RuntimeDataDir() string {
	if c.sourcePath == "" {
		return ""
	}
	return parentDir(c.sourcePath)
}

// WorkspacePath returns the expanded workspace path (schema.py:490-493).
//
// DELIBERATE DIVERGENCE from the reference, for the empty case only: Python's
// `Config.workspace_path` is `Path(self.agents.defaults.workspace).expanduser()`
// with no fallback, so an unset workspace yields "." — and this port, which
// returns a string rather than a Path, yielded "". Either way the caller gets a
// "ghost workspace": a path that is not the configured or default workspace
// (internal/api/openai_compat.go passes this value to the slash-command router,
// whose /status reply prints it). An empty workspace therefore resolves to
// DefaultWorkspace() — the same branding-aware fallback
// cmd/haosbot/runtime.go:369-371 and cmd/nanobot/runtime.go:399-401 already
// apply to an empty workspace — while any explicitly configured value, including
// the field's own literal "~/.nanobot/workspace" default (schema.py:119), is
// returned unchanged.
func (c *Config) WorkspacePath() string {
	if c.Agents.Defaults.Workspace == "" {
		return DefaultWorkspace()
	}
	return expandUser(c.Agents.Defaults.Workspace)
}

// ResolveDefaultPreset returns the implicit `default` preset built from the
// agents.defaults fields. Port of schema.py:472-479.
func (c *Config) ResolveDefaultPreset() ModelPresetConfig {
	d := c.Agents.Defaults
	return ModelPresetConfig{
		Model:               d.Model,
		Provider:            d.Provider,
		MaxTokens:           d.MaxTokens,
		ContextWindowTokens: d.ContextWindowTokens,
		Temperature:         d.Temperature,
		ReasoningEffort:     d.ReasoningEffort,
	}
}

// ResolvePreset returns the effective model parameters from a named preset.
// Port of Config.resolve_preset (schema.py:481-488).
//
// `name` mirrors the Python signature's optional argument, and the distinction
// matters: Python uses `self.agents.defaults.model_preset if name is None else
// name`, and then `if not name or name == "default"` falls back to the implicit
// default preset. So:
//
//	name == nil                 -> agents.defaults.model_preset (may itself be empty)
//	name != nil, "" or "default" -> the implicit default preset, ignoring model_preset
//	otherwise                   -> the named entry, or an error
func (c *Config) ResolvePreset(name *string) (ModelPresetConfig, error) {
	effective := c.Agents.Defaults.ModelPreset
	if name != nil {
		effective = name
	}
	if effective == nil || *effective == "" || *effective == "default" {
		return c.ResolveDefaultPreset(), nil
	}
	p, ok := c.ModelPresets[*effective]
	if !ok {
		// Python raises KeyError here.
		return ModelPresetConfig{}, fmt.Errorf("model_preset %q not found in model_presets", *effective)
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// agents
// ---------------------------------------------------------------------------

// AgentsConfig mirrors schema.py:187-190.
type AgentsConfig struct {
	Defaults AgentDefaults `json:"defaults"`
}

// AgentDefaults mirrors schema.py:116-184.
type AgentDefaults struct {
	Workspace             string              `json:"workspace"`
	ModelPreset           *string             `json:"modelPreset"`
	Model                 string              `json:"model"`
	Provider              string              `json:"provider"`
	MaxTokens             int                 `json:"maxTokens"`
	ContextWindowTokens   int                 `json:"contextWindowTokens"`
	Temperature           pyjson.Float        `json:"temperature"`
	FallbackModels        []FallbackCandidate `json:"fallbackModels"`
	MaxToolIterations     int                 `json:"maxToolIterations"`
	MaxConcurrentSubagent int                 `json:"maxConcurrentSubagents"`
	MaxToolResultChars    int                 `json:"maxToolResultChars"`
	ProviderRetryMode     string              `json:"providerRetryMode"`
	ToolHintMaxLength     int                 `json:"toolHintMaxLength"`
	ReasoningEffort       *string             `json:"reasoningEffort"`
	Timezone              string              `json:"timezone"`
	TimezoneMode          string              `json:"timezoneMode"`
	BotName               string              `json:"botName"`
	BotIcon               string              `json:"botIcon"`
	UnifiedSession        bool                `json:"unifiedSession"`
	DisabledSkills        []string            `json:"disabledSkills"`

	// SessionTTLMinutes has validation_alias AliasChoices("idleCompactAfterMinutes",
	// "sessionTtlMinutes") and serialization_alias "idleCompactAfterMinutes"
	// (schema.py:147-152): it READS from two camelCase spellings plus its own
	// snake_case name, but always WRITES as "idleCompactAfterMinutes".
	SessionTTLMinutes int `json:"idleCompactAfterMinutes"`

	// IdleCompactCheckIntervalSecs carries no explicit alias, so both
	// "idleCompactCheckIntervalSeconds" and the field name are accepted.
	IdleCompactCheckIntervalSecs int `json:"idleCompactCheckIntervalSeconds"`

	Dream DreamConfig `json:"dream"`
}

// DreamConfig mirrors schema.py:53-80.
type DreamConfig struct {
	Enabled   bool `json:"enabled"`
	IntervalH int  `json:"intervalH"`
	// Cron is exclude_if None (schema.py:60-63) so it disappears from
	// serialization when unset; every other optional field serializes as null.
	Cron *string `json:"cron,omitempty"`
	// ModelOverride accepts "modelOverride", "model" or "model_override"
	// (schema.py:64-67) and serializes as "modelOverride".
	ModelOverride *string `json:"modelOverride"`
}

// InlineFallbackConfig mirrors schema.py:83-91.
type InlineFallbackConfig struct {
	Model               string        `json:"model"`
	Provider            string        `json:"provider"`
	MaxTokens           *int          `json:"maxTokens"`
	ContextWindowTokens *int          `json:"contextWindowTokens"`
	Temperature         *pyjson.Float `json:"temperature"`
	ReasoningEffort     *string       `json:"reasoningEffort"`
}

// FallbackCandidate models `FallbackCandidate = str | InlineFallbackConfig`
// (schema.py:94). Exactly one of Name / Inline is set.
type FallbackCandidate struct {
	Name   string
	Inline *InlineFallbackConfig
}

// MarshalJSON emits either a bare JSON string or an inline object, matching the
// Python union.
func (f FallbackCandidate) MarshalJSON() ([]byte, error) {
	if f.Inline != nil {
		return json.Marshal(*f.Inline)
	}
	return json.Marshal(f.Name)
}

// UnmarshalJSON accepts a JSON string or an inline object.
func (f *FallbackCandidate) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = FallbackCandidate{Name: s}
		return nil
	}
	var in InlineFallbackConfig
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	*f = FallbackCandidate{Inline: &in}
	return nil
}

// ModelPresetConfig mirrors schema.py:97-113.
type ModelPresetConfig struct {
	Model               string       `json:"model"`
	Provider            string       `json:"provider"`
	MaxTokens           int          `json:"maxTokens"`
	ContextWindowTokens int          `json:"contextWindowTokens"`
	Temperature         pyjson.Float `json:"temperature"`
	ReasoningEffort     *string      `json:"reasoningEffort"`
}

// ---------------------------------------------------------------------------
// channels / transcription
// ---------------------------------------------------------------------------

// ChannelsConfig mirrors schema.py:23-39. It declares extra="allow"
// (schema.py:31): unknown keys are preserved verbatim as channel configs.
type ChannelsConfig struct {
	SendProgress          bool    `json:"sendProgress"`
	SendToolHints         bool    `json:"sendToolHints"`
	ShowReasoning         bool    `json:"showReasoning"`
	ExtractDocumentText   bool    `json:"extractDocumentText"`
	SendMaxRetries        int     `json:"sendMaxRetries"`
	TranscriptionProvider string  `json:"transcriptionProvider"`
	TranscriptionLanguage *string `json:"transcriptionLanguage"`

	// Extra holds the extra="allow" fields (e.g. "telegram", "discord").
	Extra map[string]any `json:"-"`
}

// MarshalJSON appends extra="allow" fields after the declared ones, which is
// what pydantic's model_dump does.
func (c ChannelsConfig) MarshalJSON() ([]byte, error) {
	type alias ChannelsConfig
	return marshalWithExtra(alias(c), anyExtra(c.Extra))
}

// TranscriptionConfig mirrors schema.py:42-50.
type TranscriptionConfig struct {
	Enabled        bool    `json:"enabled"`
	Provider       *string `json:"provider"`
	Model          *string `json:"model"`
	Language       *string `json:"language"`
	MaxDurationSec int     `json:"maxDurationSec"`
	MaxUploadMB    int     `json:"maxUploadMb"`
}

// ---------------------------------------------------------------------------
// providers
// ---------------------------------------------------------------------------

// ProviderConfig mirrors schema.py:193-230.
type ProviderConfig struct {
	// DisplayName is exclude_if None (schema.py:197-200): omitted when nil.
	DisplayName   *string           `json:"displayName,omitempty"`
	APIKey        *string           `json:"apiKey"`
	APIBase       *string           `json:"apiBase"`
	APIType       string            `json:"apiType"`
	ExtraHeaders  map[string]string `json:"extraHeaders"`
	ExtraBody     map[string]any    `json:"extraBody"`
	ExtraQuery    map[string]string `json:"extraQuery"`
	Proxy         *string           `json:"proxy"`
	ThinkingStyle *string           `json:"thinkingStyle"`
}

// BedrockProviderConfig mirrors schema.py:233-237.
type BedrockProviderConfig struct {
	ProviderConfig
	Region  *string `json:"region"`
	Profile *string `json:"profile"`
}

// ProvidersConfig mirrors schema.py:240-323. It declares extra="allow": any
// unknown key becomes an OpenAI-compatible custom provider.
//
// openai_codex / xai_grok / github_copilot carry exclude=True (schema.py:287-289)
// so they never serialize; save_config re-adds the non-credential subset.
type ProvidersConfig struct {
	Custom               ProviderConfig        `json:"custom"`
	AzureOpenAI          ProviderConfig        `json:"azureOpenai"`
	Bedrock              BedrockProviderConfig `json:"bedrock"`
	Anthropic            ProviderConfig        `json:"anthropic"`
	OpenAI               ProviderConfig        `json:"openai"`
	OpenRouter           ProviderConfig        `json:"openrouter"`
	OrcaRouter           ProviderConfig        `json:"orcarouter"`
	AssemblyAI           ProviderConfig        `json:"assemblyai"`
	HuggingFace          ProviderConfig        `json:"huggingface"`
	Skywork              ProviderConfig        `json:"skywork"`
	DeepSeek             ProviderConfig        `json:"deepseek"`
	Groq                 ProviderConfig        `json:"groq"`
	Zhipu                ProviderConfig        `json:"zhipu"`
	DashScope            ProviderConfig        `json:"dashscope"`
	ModelScope           ProviderConfig        `json:"modelscope"`
	VLLM                 ProviderConfig        `json:"vllm"`
	Ollama               ProviderConfig        `json:"ollama"`
	LMStudio             ProviderConfig        `json:"lmStudio"`
	AtomicChat           ProviderConfig        `json:"atomicChat"`
	OVMS                 ProviderConfig        `json:"ovms"`
	Gemini               ProviderConfig        `json:"gemini"`
	Moonshot             ProviderConfig        `json:"moonshot"`
	KimiCoding           ProviderConfig        `json:"kimiCoding"`
	MiniMax              ProviderConfig        `json:"minimax"`
	MiniMaxAnthropic     ProviderConfig        `json:"minimaxAnthropic"`
	Mistral              ProviderConfig        `json:"mistral"`
	StepFun              ProviderConfig        `json:"stepfun"`
	XiaomiMiMo           ProviderConfig        `json:"xiaomiMimo"`
	LongCat              ProviderConfig        `json:"longcat"`
	AntLing              ProviderConfig        `json:"antLing"`
	AIHubMix             ProviderConfig        `json:"aihubmix"`
	SiliconFlow          ProviderConfig        `json:"siliconflow"`
	EdenAI               ProviderConfig        `json:"edenai"`
	Novita               ProviderConfig        `json:"novita"`
	VolcEngine           ProviderConfig        `json:"volcengine"`
	VolcEngineCodingPlan ProviderConfig        `json:"volcengineCodingPlan"`
	BytePlus             ProviderConfig        `json:"byteplus"`
	BytePlusCodingPlan   ProviderConfig        `json:"byteplusCodingPlan"`
	OpenAICodex          ProviderConfig        `json:"-"`
	XAIGrok              ProviderConfig        `json:"-"`
	GitHubCopilot        ProviderConfig        `json:"-"`
	Qianfan              ProviderConfig        `json:"qianfan"`
	NVIDIA               ProviderConfig        `json:"nvidia"`
	OpenCode             ProviderConfig        `json:"opencode"`
	OpenCodeZen          ProviderConfig        `json:"opencodeZen"`
	OpenCodeGo           ProviderConfig        `json:"opencodeGo"`

	// ExtraProviders holds the extra="allow" custom providers, keyed by their
	// raw config key.
	ExtraProviders map[string]ProviderConfig `json:"-"`
}

// MarshalJSON appends extra="allow" custom providers after the declared ones.
func (p ProvidersConfig) MarshalJSON() ([]byte, error) {
	type alias ProvidersConfig
	extra := make(map[string]json.RawMessage, len(p.ExtraProviders))
	for k, v := range p.ExtraProviders {
		b, err := marshalNoEscape(v)
		if err != nil {
			return nil, err
		}
		extra[k] = b
	}
	return marshalWithExtra(alias(p), extra)
}

// ---------------------------------------------------------------------------
// api / gateway
// ---------------------------------------------------------------------------

// ApiConfig mirrors schema.py:333-350.
type ApiConfig struct {
	Host    string       `json:"host"`
	Port    int          `json:"port"`
	Timeout pyjson.Float `json:"timeout"`
	APIKey  string       `json:"apiKey"`
}

// GatewayConfig mirrors schema.py:353-359, plus two port-only queue limits.
//
// The reference's GatewayConfig declares only host/port/restart_mode/heartbeat.
// MaxInboundQueue and MaxOutboundQueue are a DELIBERATE DIVERGENCE: the Python
// bus uses unbounded asyncio.Queues (nanobot/bus/queue.py:32-33), so the
// reference has no such setting to mirror, and this port must not run unbounded
// on the small devices it targets.
//
// They live here rather than at the root of Config because the reference's root
// Config is a BaseSettings with extra="forbid" (schema.py:422), so a root-level
// key would make a config file written by this port fail to load in the
// reference ("Unknown setting"). GatewayConfig descends from `Base`, whose
// pydantic default is extra="ignore", so the reference loads these keys and
// ignores them — verified against upstream 1bb712d3.
type GatewayConfig struct {
	Host        string          `json:"host"`
	Port        int             `json:"port"`
	RestartMode string          `json:"restartMode"`
	Heartbeat   HeartbeatConfig `json:"heartbeat"`

	// MaxInboundQueue caps the gateway's pending inbound messages;
	// MaxOutboundQueue does the same for outbound messages. Zero means
	// unbounded, matching bus.Options, but the defaults are non-zero.
	MaxInboundQueue  int `json:"maxInboundQueue"`
	MaxOutboundQueue int `json:"maxOutboundQueue"`
}

// HeartbeatConfig mirrors schema.py:326-330.
type HeartbeatConfig struct {
	Enabled   bool `json:"enabled"`
	IntervalS int  `json:"intervalS"`
}

// ---------------------------------------------------------------------------
// tools
// ---------------------------------------------------------------------------

// ToolsConfig mirrors schema.py:384-419.
type ToolsConfig struct {
	Web                            WebToolsConfig             `json:"web"`
	Exec                           ExecToolConfig             `json:"exec"`
	File                           FileToolsConfig            `json:"file"`
	CliApps                        CliAppsToolConfig          `json:"cliApps"`
	My                             MyToolConfig               `json:"my"`
	ImageGeneration                ImageGenerationToolConfig  `json:"imageGeneration"`
	MaxSessionMessagesPerMinute    int                        `json:"maxSessionMessagesPerMinute"`
	RestrictToWorkspace            bool                       `json:"restrictToWorkspace"`
	WebUIAllowLocalServiceAccess   bool                       `json:"webuiAllowLocalServiceAccess"`
	WebUIAllowRemotePackageInstall bool                       `json:"webuiAllowRemotePackageInstall"`
	MCPServers                     map[string]MCPServerConfig `json:"mcpServers"`
	SSRFWhitelist                  []string                   `json:"ssrfWhitelist"`
}

// MCPServerConfig mirrors schema.py:362-374.
type MCPServerConfig struct {
	Type         *string           `json:"type"`
	Auth         *string           `json:"auth"`
	Command      string            `json:"command"`
	Args         []string          `json:"args"`
	Env          map[string]string `json:"env"`
	CWD          string            `json:"cwd"`
	URL          string            `json:"url"`
	Headers      map[string]string `json:"headers"`
	ToolTimeout  int               `json:"toolTimeout"`
	EnabledTools []string          `json:"enabledTools"`
}

// WebToolsConfig mirrors agent/tools/web.py:84-91.
type WebToolsConfig struct {
	Enable    bool            `json:"enable"`
	Proxy     *string         `json:"proxy"`
	UserAgent *string         `json:"userAgent"`
	Search    WebSearchConfig `json:"search"`
	Fetch     WebFetchConfig  `json:"fetch"`
}

// WebSearchConfig mirrors agent/tools/web.py:70-76.
type WebSearchConfig struct {
	Provider   string `json:"provider"`
	APIKey     string `json:"apiKey"`
	BaseURL    string `json:"baseUrl"`
	MaxResults int    `json:"maxResults"`
	Timeout    int    `json:"timeout"`
}

// WebFetchConfig mirrors agent/tools/web.py:79-81.
type WebFetchConfig struct {
	UseJinaReader bool `json:"useJinaReader"`
}

// ExecToolConfig mirrors agent/tools/shell.py:94-105.
type ExecToolConfig struct {
	Enable         bool     `json:"enable"`
	Timeout        int      `json:"timeout"`
	PathPrepend    string   `json:"pathPrepend"`
	PathAppend     string   `json:"pathAppend"`
	Sandbox        string   `json:"sandbox"`
	SandboxROBinds []string `json:"sandboxRoBinds"`
	SandboxRWBinds []string `json:"sandboxRwBinds"`
	AllowedEnvKeys []string `json:"allowedEnvKeys"`
	AllowPatterns  []string `json:"allowPatterns"`
	DenyPatterns   []string `json:"denyPatterns"`
}

// FileToolsConfig mirrors agent/tools/filesystem.py:28-31.
type FileToolsConfig struct {
	Enable bool `json:"enable"`
}

// CliAppsToolConfig mirrors agent/tools/cli_apps.py:27-33.
type CliAppsToolConfig struct {
	Enable            bool `json:"enable"`
	InstallTimeout    int  `json:"installTimeout"`
	RunTimeout        int  `json:"runTimeout"`
	CatalogTTLSeconds int  `json:"catalogTtlSeconds"`
}

// MyToolConfig mirrors agent/tools/self.py:30-33.
type MyToolConfig struct {
	Enable   bool `json:"enable"`
	AllowSet bool `json:"allowSet"`
}

// ImageGenerationToolConfig mirrors agent/tools/image_generation.py:49-57.
type ImageGenerationToolConfig struct {
	Enabled            bool   `json:"enabled"`
	Provider           string `json:"provider"`
	Model              string `json:"model"`
	DefaultAspectRatio string `json:"defaultAspectRatio"`
	DefaultImageSize   string `json:"defaultImageSize"`
	MaxImagesPerTurn   int    `json:"maxImagesPerTurn"`
	SaveDir            string `json:"saveDir"`
}

// ---------------------------------------------------------------------------
// serialization helpers
// ---------------------------------------------------------------------------

func anyExtra(m map[string]any) map[string]json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		b, err := marshalNoEscape(v)
		if err != nil {
			// Unrepresentable extra value: skip rather than corrupt the document.
			continue
		}
		out[k] = b
	}
	return out
}

// marshalWithExtra marshals base and appends extra entries after its declared
// fields, mirroring how pydantic serializes extra="allow" models (extras follow
// the declared fields). Go map iteration is unordered, so extra keys are emitted
// sorted for determinism; Python preserves input insertion order. Key order in a
// JSON object is not semantically meaningful.
func marshalWithExtra(base any, extra map[string]json.RawMessage) ([]byte, error) {
	raw, err := marshalNoEscape(base)
	if err != nil {
		return nil, err
	}
	if len(extra) == 0 {
		return raw, nil
	}
	if len(raw) < 2 || raw[0] != '{' || raw[len(raw)-1] != '}' {
		return raw, nil
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]byte, 0, len(raw)+64*len(keys))
	out = append(out, raw[:len(raw)-1]...)
	for _, k := range keys {
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		out = append(out, ',')
		out = append(out, kb...)
		out = append(out, ':')
		out = append(out, extra[k]...)
	}
	out = append(out, '}')
	return out, nil
}

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

// DefaultConfig returns a configuration equivalent to the Python reference's
// `Config()` with no arguments (loader.py:61).
//
// It is produced by validating an EMPTY document rather than by copying struct
// literals, because the reference's defaults are not simply the declared field
// defaults: `AgentDefaults.resolve_timezone` (schema.py:159-173) rewrites
// `timezone` to the system zone whenever `timezone_mode` resolves to "auto",
// which is the default. Going through the decoder keeps that behavior in one
// place instead of duplicating it.
func DefaultConfig() *Config {
	cfg, _ := decodeConfig(map[string]any{})
	return cfg
}

// defaultConfigSkeleton is the literal field-default table used as the starting
// point for decoding. Prefer DefaultConfig().
//
// The agent defaults are pre-resolved through `resolve_timezone` with an empty
// mapping, because that is what `AgentDefaults()` does in Python: the declared
// `timezone` default is "UTC" but the model_validator rewrites it to
// detect_system_timezone() whenever `timezone_mode` resolves to "auto". Without
// this step, a document that omits `agents` (or `agents.defaults`) entirely would
// report "UTC" where the reference reports the host zone.
func defaultConfigSkeleton() *Config {
	return &Config{
		Agents:        AgentsConfig{Defaults: defaultAgentDefaultsResolved()},
		Channels:      DefaultChannelsConfig(),
		Transcription: DefaultTranscriptionConfig(),
		Providers:     DefaultProvidersConfig(),
		API:           ApiConfig{Host: "127.0.0.1", Port: 8900, Timeout: 120.0, APIKey: ""},
		Gateway: GatewayConfig{
			Host:             "127.0.0.1",
			Port:             18790,
			RestartMode:      "auto",
			Heartbeat:        HeartbeatConfig{Enabled: true, IntervalS: 30 * 60},
			MaxInboundQueue:  DefaultMaxInboundQueue,
			MaxOutboundQueue: DefaultMaxOutboundQueue,
		},
		Tools:        DefaultToolsConfig(),
		ModelPresets: map[string]ModelPresetConfig{},
	}
}

// DefaultAgentDefaults returns the DECLARED field defaults from
// schema.py:119-157, including the literal `timezone = "UTC"` and
// `timezone_mode = "auto"`.
//
// CAUTION: these are not the effective defaults. The reference's
// `resolve_timezone` model_validator (schema.py:159-173) replaces `timezone`
// with `detect_system_timezone()` whenever `timezone_mode` resolves to "auto",
// which it does by default. Use DefaultConfig() — or, when a mapping was
// supplied, decodeAgentDefaults — for the values a loaded config actually holds.
// defaultAgentDefaultsResolved performs that step for the skeleton.
func DefaultAgentDefaults() AgentDefaults {
	return AgentDefaults{
		Workspace:                    "~/.nanobot/workspace",
		ModelPreset:                  nil,
		Model:                        "anthropic/claude-opus-4-5",
		Provider:                     "auto",
		MaxTokens:                    8192,
		ContextWindowTokens:          200_000,
		Temperature:                  0.1,
		FallbackModels:               []FallbackCandidate{},
		MaxToolIterations:            200,
		MaxConcurrentSubagent:        4,
		MaxToolResultChars:           16_000,
		ProviderRetryMode:            "standard",
		ToolHintMaxLength:            40,
		ReasoningEffort:              nil,
		Timezone:                     "UTC",
		TimezoneMode:                 "auto",
		BotName:                      "nanobot",
		BotIcon:                      "🐈",
		UnifiedSession:               false,
		DisabledSkills:               []string{},
		SessionTTLMinutes:            15,
		IdleCompactCheckIntervalSecs: 60,
		Dream: DreamConfig{
			Enabled:       true,
			IntervalH:     2,
			Cron:          nil,
			ModelOverride: nil,
		},
	}
}

// defaultAgentDefaultsResolved applies `resolve_timezone` to an empty mapping,
// producing the values `AgentDefaults()` yields in Python: timezone_mode "auto"
// and timezone = detect_system_timezone().
func defaultAgentDefaultsResolved() AgentDefaults {
	d := DefaultAgentDefaults()
	resolved := resolveTimezone(map[string]any{})
	if s, ok := resolved["timezone"].(string); ok {
		d.Timezone = s
	}
	if s, ok := resolved["timezoneMode"].(string); ok {
		d.TimezoneMode = s
	}
	return d
}

// DefaultChannelsConfig mirrors schema.py:33-39.
func DefaultChannelsConfig() ChannelsConfig {
	return ChannelsConfig{
		SendProgress:          true,
		SendToolHints:         true,
		ShowReasoning:         true,
		ExtractDocumentText:   true,
		SendMaxRetries:        3,
		TranscriptionProvider: "groq",
		TranscriptionLanguage: nil,
	}
}

// DefaultTranscriptionConfig mirrors schema.py:44-50.
func DefaultTranscriptionConfig() TranscriptionConfig {
	return TranscriptionConfig{
		Enabled:        true,
		MaxDurationSec: 120,
		MaxUploadMB:    25,
	}
}

// DefaultProviderConfig mirrors schema.py:195-208.
func DefaultProviderConfig() ProviderConfig {
	return ProviderConfig{APIType: "auto"}
}

// DefaultProvidersConfig mirrors schema.py:249-294.
func DefaultProvidersConfig() ProvidersConfig {
	p := ProviderConfig{APIType: "auto"}
	return ProvidersConfig{
		Custom: p, AzureOpenAI: p, Anthropic: p, OpenAI: p, OpenRouter: p,
		OrcaRouter: p, AssemblyAI: p, HuggingFace: p, Skywork: p, DeepSeek: p,
		Groq: p, Zhipu: p, DashScope: p, ModelScope: p, VLLM: p, Ollama: p,
		LMStudio: p, AtomicChat: p, OVMS: p, Gemini: p, Moonshot: p,
		KimiCoding: p, MiniMax: p, MiniMaxAnthropic: p, Mistral: p, StepFun: p,
		XiaomiMiMo: p, LongCat: p, AntLing: p, AIHubMix: p, SiliconFlow: p,
		EdenAI: p, Novita: p, VolcEngine: p, VolcEngineCodingPlan: p,
		BytePlus: p, BytePlusCodingPlan: p, OpenAICodex: p, XAIGrok: p,
		GitHubCopilot: p, Qianfan: p, NVIDIA: p, OpenCode: p, OpenCodeZen: p,
		OpenCodeGo: p,
		Bedrock:    BedrockProviderConfig{ProviderConfig: p},
	}
}

// DefaultToolsConfig mirrors schema.py:392-419 and the tool sub-config defaults.
func DefaultToolsConfig() ToolsConfig {
	return ToolsConfig{
		Web: WebToolsConfig{
			Enable: true,
			Search: WebSearchConfig{
				Provider:   "duckduckgo",
				APIKey:     "",
				BaseURL:    "",
				MaxResults: 5,
				Timeout:    30,
			},
			Fetch: WebFetchConfig{UseJinaReader: true},
		},
		Exec: ExecToolConfig{
			Enable:         true,
			Timeout:        60,
			SandboxROBinds: []string{},
			SandboxRWBinds: []string{},
			AllowedEnvKeys: []string{},
			AllowPatterns:  []string{},
			DenyPatterns:   []string{},
		},
		File:    FileToolsConfig{Enable: true},
		CliApps: CliAppsToolConfig{Enable: true, InstallTimeout: 300, RunTimeout: 60, CatalogTTLSeconds: 3600},
		My:      MyToolConfig{Enable: true, AllowSet: false},
		ImageGeneration: ImageGenerationToolConfig{
			Enabled:            false,
			Provider:           "openrouter",
			Model:              "openai/gpt-5.4-image-2",
			DefaultAspectRatio: "1:1",
			DefaultImageSize:   "1K",
			MaxImagesPerTurn:   4,
			SaveDir:            "generated",
		},
		MaxSessionMessagesPerMinute:    6,
		RestrictToWorkspace:            false,
		WebUIAllowLocalServiceAccess:   true,
		WebUIAllowRemotePackageInstall: false,
		MCPServers:                     map[string]MCPServerConfig{},
		SSRFWhitelist:                  []string{},
	}
}

// DefaultMCPServerConfig mirrors schema.py:365-374.
func DefaultMCPServerConfig() MCPServerConfig {
	return MCPServerConfig{
		Command:      "",
		Args:         []string{},
		Env:          map[string]string{},
		CWD:          "",
		URL:          "",
		Headers:      map[string]string{},
		ToolTimeout:  30,
		EnabledTools: []string{"*"},
	}
}
