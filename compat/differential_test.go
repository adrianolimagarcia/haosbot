// Package compat holds differential tests that compare nanobot-go against the
// frozen Python reference.
//
// Unlike the unit tests in internal/, these tests do not assert against values
// written by hand from reading the source. They execute the real Python
// implementation and compare its output with the Go implementation's output.
// That is the only way to catch a misread of the upstream code.
//
// The reference runs from a venv at .tools/venv. When that venv is absent the
// tests SKIP rather than fail, so a checkout without it still builds and tests
// cleanly — but they never silently pass: the skip is reported.
package compat

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/memory"
	"github.com/adrianolimagarcia/nanobot-go/internal/prompt"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/pyjson"
	"github.com/adrianolimagarcia/nanobot-go/internal/runtimecontext"
	"github.com/adrianolimagarcia/nanobot-go/internal/session"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// repoRoot resolves the module root from this file's location.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller path")
	}
	return filepath.Dir(filepath.Dir(file))
}

type reference struct {
	UpstreamCommit    string            `json:"upstream_commit"`
	AgentDefaults     map[string]any    `json:"agent_defaults"`
	SerializedDefault map[string]any    `json:"serialized_defaults"`
	AliasAcceptance   map[string]any    `json:"alias_acceptance"`
	StorageKeys       map[string]string `json:"storage_keys"`
	Paths             map[string]string `json:"paths"`
	RetryClassify     []struct {
		Name      string `json:"name"`
		Transient bool   `json:"transient"`
	} `json:"retry_classification"`
	Governance map[string]struct {
		Input   []map[string]any `json:"input"`
		Output  []map[string]any `json:"output"`
		Changed bool             `json:"changed"`
	} `json:"governance"`
	Budget []struct {
		Window    int `json:"window"`
		MaxTokens int `json:"max_tokens"`
		Budget    int `json:"budget"`
	} `json:"budget"`
	TruncateText []struct {
		Text  string `json:"text"`
		Limit int    `json:"limit"`
		Out   string `json:"out"`
	} `json:"truncate_text"`
	StripThink []struct {
		In  string `json:"in"`
		Out string `json:"out"`
	} `json:"strip_think"`
	GitStore       map[string]any `json:"gitstore"`
	LengthRecovery struct {
		Restore []struct {
			Content  string `json:"content"`
			Original string `json:"original"`
			Result   string `json:"result"`
		} `json:"restore"`
		Messages []struct {
			Content string `json:"content"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"messages"`
	} `json:"length_recovery"`
	PyJSON struct {
		Floats []struct {
			Repr    string `json:"repr"`
			Encoded string `json:"encoded"`
		} `json:"floats"`
		Strings []struct {
			Value   string `json:"value"`
			Encoded string `json:"encoded"`
		} `json:"strings"`
	} `json:"pyjson"`
	ToolResults struct {
		EmptyMarkers map[string]string `json:"empty_markers"`
		Emptiness    []struct {
			Label  string `json:"label"`
			Result any    `json:"result"`
		} `json:"emptiness"`
		SafeFilenames map[string]string `json:"safe_filenames"`
		Rendered      []struct {
			Label  string `json:"label"`
			Result string `json:"result"`
		} `json:"rendered"`
		Normalize []struct {
			Label          string `json:"label"`
			Empty          any    `json:"empty"`
			ReadFileExempt string `json:"read_file_exempt"`
		} `json:"normalize"`
	} `json:"tool_results"`
	PyStr struct {
		WhitespaceCodepoints []int `json:"whitespace_codepoints"`
		Cases                []struct {
			In     string `json:"in"`
			Strip  string `json:"strip"`
			RStrip string `json:"rstrip"`
			LStrip string `json:"lstrip"`
		} `json:"cases"`
	} `json:"pystr"`
	Dream struct {
		CommitMessages []struct {
			Prefix   string  `json:"prefix"`
			DiffBody *string `json:"diff_body"`
			Out      string  `json:"out"`
		} `json:"commit_messages"`
		SessionKey struct {
			Prefix        string `json:"prefix"`
			Timestamp     string `json:"timestamp"`
			TimestampLen  int    `json:"timestamp_len"`
			MatchesFormat bool   `json:"matches_format"`
		} `json:"session_key"`
		Prompt struct {
			TemplateSource   string `json:"template_source"`
			BuiltinSkillsDir string `json:"builtin_skills_dir"`
			SkillCreatorPath string `json:"skill_creator_path"`
			Rendered         string `json:"rendered"`
		} `json:"prompt"`
		MediaBreadcrumbs []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
			Media   any    `json:"media"`
			Out     any    `json:"out"`
		} `json:"media_breadcrumbs"`
		ImagePlaceholders []struct {
			Path *string `json:"path"`
			Out  string  `json:"out"`
		} `json:"image_placeholders"`
		FormatMessages []struct {
			Label    string           `json:"label"`
			Messages []map[string]any `json:"messages"`
			Out      string           `json:"out"`
		} `json:"format_messages"`
		RawCheckpoints []struct {
			Label    string           `json:"label"`
			Messages []map[string]any `json:"messages"`
			MaxChars *int             `json:"max_chars"`
			Out      string           `json:"out"`
		} `json:"raw_checkpoints"`
		RawArchiveMaxChars int `json:"raw_archive_max_chars"`
		BuildDreamPrompt   []struct {
			Label      string  `json:"label"`
			MaxEntries int     `json:"max_entries"`
			LastCursor int     `json:"last_cursor"`
			Out        *string `json:"out"`
			Cursor     *int    `json:"cursor"`
			Error      *string `json:"error"`
		} `json:"build_dream_prompt"`
		PromptHistoryJSONL string `json:"prompt_history_jsonl"`
		PublicHistory      []struct {
			Label   string         `json:"label"`
			Message map[string]any `json:"message"`
			Out     map[string]any `json:"out"`
		} `json:"public_history"`
		RunStatus []struct {
			Label     string `json:"label"`
			Kind      string `json:"kind"`
			Metadata  any    `json:"metadata"`
			Completed bool   `json:"completed"`
			Reason    string `json:"reason"`
		} `json:"run_status"`
	} `json:"dream"`
}

// loadReference runs the Python dumper and parses its output. It skips the
// test when the venv or the dumper is unavailable.
func loadReference(t *testing.T) *reference {
	t.Helper()
	out := runDumper(t)
	var ref reference
	if err := json.Unmarshal(out, &ref); err != nil {
		t.Fatalf("parse reference output: %v", err)
	}
	return &ref
}

// loadReferenceNumbers is loadReference with json.Decoder.UseNumber, so every
// untyped number in the document stays a json.Number.
//
// The Dream corpus needs that: the reference's `str()` distinguishes a JSON
// integer from a JSON float (str(1720000000) is "1720000000" while
// str(1.5) is "1.5"), and a plain json.Unmarshal collapses both into float64,
// which would make the comparison pass or fail for the wrong reason. Typed
// struct fields are unaffected by UseNumber.
func loadReferenceNumbers(t *testing.T) *reference {
	t.Helper()
	out := runDumper(t)
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	var ref reference
	if err := dec.Decode(&ref); err != nil {
		t.Fatalf("parse reference output: %v", err)
	}
	return &ref
}

// runDumper executes compat/python/dump_reference.py and returns its stdout.
// It skips the test when the venv or the dumper is unavailable.
func runDumper(t *testing.T) []byte {
	t.Helper()
	root := repoRoot(t)
	python := filepath.Join(root, ".tools", "venv", "bin", "python")
	script := filepath.Join(root, "compat", "python", "dump_reference.py")

	if _, err := os.Stat(python); err != nil {
		t.Skipf("SKIP: reference venv not present at %s — differential check not run", python)
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("SKIP: dumper missing at %s", script)
	}

	// The dumper must run with cwd = repo root so its own path resolution works.
	out, err := runReferenceCommand("reference dumper", []string{python, script}, root, nil)
	if err != nil {
		t.Fatalf("reference dumper failed: %v", err)
	}
	return out
}

// HAOSBot intentionally diverges from the frozen Nanobot reference in a very
// small set of product defaults. Keep these overrides explicit so differential
// tests still fail on every other upstream mismatch.
var intentionalDefaultDivergences = map[string]any{
	"workspace":             "~/.haosbot/workspace",
	"context_window_tokens": float64(128_000),
	"bot_name":              "haosbot",
}

var intentionalSerializedDefaultDivergences = map[string]any{
	"workspace":            "~/.haosbot/workspace",
	"contextWindowTokens": float64(128_000),
	"botName":             "haosbot",
}

// ---------------------------------------------------------------------------
// 1. Defaults
// ---------------------------------------------------------------------------

func TestDefaultsMatchPythonReference(t *testing.T) {
	ref := loadReference(t)

	// DefaultConfig() is the hermetic counterpart of the reference's
	// AgentDefaults(): both are the DECLARED field defaults with the timezone
	// validator applied, and neither reads a file. LoadDefault() would instead
	// merge the developer's real ~/.haosbot/config.json (or the legacy
	// ~/.nanobot/config.json), so this test would compare a local configuration
	// against the reference's defaults and pass only on a machine that happens to
	// have no config file.
	cfg := config.DefaultConfig()
	d := cfg.Agents.Defaults

	got := map[string]any{
		"workspace":                           d.Workspace,
		"model":                               d.Model,
		"provider":                            d.Provider,
		"max_tokens":                          float64(d.MaxTokens),
		"context_window_tokens":               float64(d.ContextWindowTokens),
		"temperature":                         float64(d.Temperature),
		"max_tool_iterations":                 float64(d.MaxToolIterations),
		"max_concurrent_subagents":            float64(d.MaxConcurrentSubagent),
		"max_tool_result_chars":               float64(d.MaxToolResultChars),
		"provider_retry_mode":                 d.ProviderRetryMode,
		"tool_hint_max_length":                float64(d.ToolHintMaxLength),
		"timezone":                            d.Timezone,
		"timezone_mode":                       d.TimezoneMode,
		"bot_name":                            d.BotName,
		"bot_icon":                            d.BotIcon,
		"unified_session":                     d.UnifiedSession,
		"session_ttl_minutes":                 float64(d.SessionTTLMinutes),
		"idle_compact_check_interval_seconds": float64(d.IdleCompactCheckIntervalSecs),
	}

	for _, key := range sortedKeys(ref.AgentDefaults) {
		want := ref.AgentDefaults[key]
		gotVal, ok := got[key]
		if !ok {
			t.Errorf("field %q is in the Python reference but not compared by this test", key)
			continue
		}
		if expected, intentional := intentionalDefaultDivergences[key]; intentional {
			if !reflect.DeepEqual(expected, gotVal) {
				t.Errorf("intentional HAOSBot default %q changed: want=%#v (%T)  got=%#v (%T)", key, expected, expected, gotVal, gotVal)
			}
			continue
		}
		if !reflect.DeepEqual(want, gotVal) {
			t.Errorf("default %q: python=%#v (%T)  go=%#v (%T)", key, want, want, gotVal, gotVal)
		}
	}
	for _, key := range sortedKeys(got) {
		if _, ok := ref.AgentDefaults[key]; !ok {
			t.Errorf("field %q is compared but absent from the Python reference", key)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. Serialization (aliases + null handling)
// ---------------------------------------------------------------------------

func TestSerializationMatchesPythonReference(t *testing.T) {
	ref := loadReference(t)

	// Hermetic for the same reason as TestDefaultsMatchPythonReference: the
	// reference serializes AgentDefaults(), never a loaded config file.
	cfg := config.DefaultConfig()

	raw, err := json.Marshal(cfg.Agents.Defaults)
	if err != nil {
		t.Fatalf("marshal defaults: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// The Python side dumps the whole AgentDefaults model; compare key sets
	// first, because a missing or extra key is the failure mode that breaks
	// round-tripping most often.
	var missing, extra []string
	for k := range ref.SerializedDefault {
		if _, ok := got[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range got {
		if _, ok := ref.SerializedDefault[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("keys present in Python but missing from Go: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("keys present in Go but not Python: %v", extra)
	}

	// Compare values for the keys both sides have.
	for _, k := range sortedKeys(ref.SerializedDefault) {
		want := ref.SerializedDefault[k]
		gotVal, ok := got[k]
		if !ok {
			continue // already reported above
		}
		if expected, intentional := intentionalSerializedDefaultDivergences[k]; intentional {
			if !reflect.DeepEqual(expected, gotVal) {
				t.Errorf("intentional HAOSBot serialized default %q changed: want=%#v (%T)  got=%#v (%T)", k, expected, expected, gotVal, gotVal)
			}
			continue
		}
		if !reflect.DeepEqual(want, gotVal) {
			t.Errorf("serialized %q: python=%#v (%T)  go=%#v (%T)", k, want, want, gotVal, gotVal)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. Aliases
// ---------------------------------------------------------------------------

func TestAliasAcceptanceMatchesPythonReference(t *testing.T) {
	ref := loadReference(t)

	for _, spelling := range sortedKeys(ref.AliasAcceptance) {
		want := ref.AliasAcceptance[spelling]
		if s, ok := want.(string); ok && strings.HasPrefix(s, "REJECTED") {
			t.Logf("python rejects alias %q: %s (Go behaviour not asserted)", spelling, s)
			continue
		}
		wantNum, ok := want.(float64)
		if !ok {
			t.Errorf("unexpected reference value for alias %q: %#v", spelling, want)
			continue
		}

		path := filepath.Join(t.TempDir(), "config.json")
		doc := `{"agents":{"defaults":{` + strconv.Quote(spelling) + `:` +
			strconv.Itoa(int(wantNum)) + `}}}`
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}

		cfg, err := config.Load(path)
		if err != nil {
			t.Errorf("alias %q: Python accepts it but Go rejected it: %v", spelling, err)
			continue
		}
		if got := cfg.Agents.Defaults.SessionTTLMinutes; float64(got) != wantNum {
			t.Errorf("alias %q: python=%v go=%v", spelling, wantNum, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Session storage keys
// ---------------------------------------------------------------------------

func TestStorageKeysMatchPythonReference(t *testing.T) {
	ref := loadReference(t)

	for key, want := range ref.StorageKeys {
		if got := session.StorageKey(key); got != want {
			t.Errorf("StorageKey(%q): python=%q go=%q", key, want, got)
		}
		// Round-trip: the stem must decode back to the original key.
		if back, ok := session.DecodeStorageKey(want); !ok || back != key {
			t.Errorf("DecodeStorageKey(%q) = %q, %v; want %q, true", want, back, ok, key)
		}
	}
}

// ---------------------------------------------------------------------------
// 5. Paths
// ---------------------------------------------------------------------------

func TestPathsMatchPythonReference(t *testing.T) {
	ref := loadReference(t)

	// DefaultConfigPath is the Go side of the same resolution the Python
	// `paths` module performs.
	gotConfig := config.DefaultConfigPath()
	wantDataDir := ref.Paths["workspace"]
	// workspace is <data_dir>/workspace, so strip that suffix to get data dir.
	wantDataDir = strings.TrimSuffix(wantDataDir, string(filepath.Separator)+"workspace")
	wantConfig := filepath.Join(wantDataDir, "config.json")

	if gotConfig != wantConfig {
		t.Errorf("config path: python=%q go=%q", wantConfig, gotConfig)
	}
}

// ---------------------------------------------------------------------------
// 6. Retry classification
// ---------------------------------------------------------------------------

// TestRetryClassificationMatchesPythonReference compares the Go classifier
// against the reference's LLMProvider.is_transient_response over a matrix of
// error shapes.
//
// This matters more than it looks: a port that treats every 429 as retryable
// will retry a permanently exhausted quota, and one that treats none as
// retryable will give up on an ordinary rate limit. Both are silent
// behavioural divergences that no unit test written from reading the source
// would reliably catch.
func TestRetryClassificationMatchesPythonReference(t *testing.T) {
	ref := loadReference(t)
	if len(ref.RetryClassify) == 0 {
		t.Fatal("reference produced no retry classification cases")
	}

	code := func(c int) *int { return &c }
	boolp := func(b bool) *bool { return &b }

	// The matrix mirrors dump_retry_classification() in the Python harness.
	build := map[string]*core.Response{
		"success": {FinishReason: core.FinishStop},
		"500":     {FinishReason: core.FinishError, ErrorStatusCode: code(500)},
		"502":     {FinishReason: core.FinishError, ErrorStatusCode: code(502)},
		"503":     {FinishReason: core.FinishError, ErrorStatusCode: code(503)},
		"504":     {FinishReason: core.FinishError, ErrorStatusCode: code(504)},
		"408":     {FinishReason: core.FinishError, ErrorStatusCode: code(408)},
		"409":     {FinishReason: core.FinishError, ErrorStatusCode: code(409)},
		"400":     {FinishReason: core.FinishError, ErrorStatusCode: code(400)},
		"401":     {FinishReason: core.FinishError, ErrorStatusCode: code(401)},
		"403":     {FinishReason: core.FinishError, ErrorStatusCode: code(403)},
		"402":     {FinishReason: core.FinishError, ErrorStatusCode: code(402)},
		"404":     {FinishReason: core.FinishError, ErrorStatusCode: code(404)},
		"429-ratelimit-type": {FinishReason: core.FinishError,
			ErrorStatusCode: code(429), ErrorType: "rate_limit_exceeded"},
		"429-ratelimit-code": {FinishReason: core.FinishError,
			ErrorStatusCode: code(429), ErrorCode: "rate_limit_error"},
		"429-quota-type": {FinishReason: core.FinishError,
			ErrorStatusCode: code(429), ErrorType: "insufficient_quota"},
		"429-quota-text": {FinishReason: core.FinishError,
			ErrorStatusCode: code(429), Content: "insufficient_quota"},
		"429-billing-text": {FinishReason: core.FinishError,
			ErrorStatusCode: code(429), Content: "billing hard limit reached"},
		"429-unknown": {FinishReason: core.FinishError, ErrorStatusCode: code(429)},
		"429-retryafter-text": {FinishReason: core.FinishError,
			ErrorStatusCode: code(429), Content: "rate limit, retry after 5s"},
		"kind-timeout":    {FinishReason: core.FinishError, ErrorKind: "timeout"},
		"kind-connection": {FinishReason: core.FinishError, ErrorKind: "connection"},
		"text-503":        {FinishReason: core.FinishError, Content: "upstream returned 503 server error"},
		"text-overloaded": {FinishReason: core.FinishError, Content: "provider overloaded"},
		"text-timedout":   {FinishReason: core.FinishError, Content: "request timed out"},
		"text-nonsense":   {FinishReason: core.FinishError, Content: "totally unknown failure"},
		"explicit-true": {FinishReason: core.FinishError,
			ErrorStatusCode: code(400), ErrorShouldRetry: boolp(true)},
		"explicit-false": {FinishReason: core.FinishError,
			ErrorStatusCode: code(503), ErrorShouldRetry: boolp(false)},
	}

	for _, tc := range ref.RetryClassify {
		resp, ok := build[tc.Name]
		if !ok {
			t.Errorf("case %q is in the Python reference but has no Go counterpart", tc.Name)
			continue
		}
		got, _ := provider.ClassifyResponse(resp)
		if got != tc.Transient {
			t.Errorf("case %q: python transient=%v, go transient=%v", tc.Name, tc.Transient, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 7. Context governance
// ---------------------------------------------------------------------------

// msgFromFixture converts one reference fixture message into a core.Message.
//
// It reads the persisted OpenAI tool_call shape (name and arguments nested
// under "function"), which is what the reference stores and what the session
// layer translates via convertToolCalls. core.Message.UnmarshalJSON alone
// expects the flat shape, so the translation is done explicitly here.
func msgFromFixture(t *testing.T, raw map[string]any) core.Message {
	t.Helper()
	role, _ := raw["role"].(string)
	m := core.Message{Role: core.Role(role)}

	if content, ok := raw["content"].(string); ok {
		m.Content = core.TextContent(content)
	}
	if id, ok := raw["tool_call_id"].(string); ok {
		m.ToolCallID = id
	}
	if name, ok := raw["name"].(string); ok {
		m.Name = name
	}
	calls, ok := raw["tool_calls"].([]any)
	if !ok {
		return m
	}
	for _, rawCall := range calls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			continue
		}
		var tc core.ToolCall
		if id, ok := call["id"].(string); ok {
			tc.ID = id
		}
		fn, _ := call["function"].(map[string]any)
		if fn != nil {
			if name, ok := fn["name"].(string); ok {
				tc.Name = name
			}
			if args, ok := fn["arguments"].(string); ok {
				tc.Arguments = json.RawMessage(args)
			}
		} else if name, ok := call["name"].(string); ok {
			// Tolerate the flat shape too, so the fixture format can evolve.
			tc.Name = name
			if args, ok := call["arguments"].(string); ok {
				tc.Arguments = json.RawMessage(args)
			}
		}
		m.ToolCalls = append(m.ToolCalls, tc)
	}
	return m
}

// msgToComparable renders a message the same way the reference fixture is
// shaped, so the two can be compared as parsed JSON.
func msgToComparable(m core.Message) map[string]any {
	out := map[string]any{"role": string(m.Role)}
	if m.Content.IsText() {
		out["content"] = m.Content.Text
	}
	if m.ToolCallID != "" {
		out["tool_call_id"] = m.ToolCallID
	}
	if m.Name != "" {
		out["name"] = m.Name
	}
	if len(m.ToolCalls) > 0 {
		calls := make([]any, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			args := ""
			if tc.Arguments != nil {
				args = string(tc.Arguments)
			}
			calls = append(calls, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": args,
				},
			})
		}
		out["tool_calls"] = calls
	}
	return out
}

// TestGovernanceMatchesPythonReference compares PrepareForModel against the
// reference's four history-repair steps over a matrix of structural defects.
//
// These are the defects that make upstream APIs reject an entire request, so a
// port that repairs them differently — or not at all — silently wedges sessions.
func TestGovernanceMatchesPythonReference(t *testing.T) {
	ref := loadReference(t)
	if len(ref.Governance) == 0 {
		t.Fatal("reference produced no governance fixtures")
	}

	for name, fixture := range ref.Governance {
		t.Run(name, func(t *testing.T) {
			input := make([]core.Message, 0, len(fixture.Input))
			for _, raw := range fixture.Input {
				input = append(input, msgFromFixture(t, raw))
			}

			got := agent.PrepareForModel(input)

			if len(got) != len(fixture.Output) {
				t.Fatalf("message count = %d, python produced %d\n  go:     %v\n  python: %v",
					len(got), len(fixture.Output), renderAll(got), renderAllFixture(fixture.Output))
			}
			for i := range got {
				want := normalizeFixtureMessage(fixture.Output[i])
				have := normalizeGoMessage(msgToComparable(got[i]))
				if !reflect.DeepEqual(have, want) {
					t.Errorf("message %d differs:\n  go:     %v\n  python: %v", i, have, want)
				}
			}
		})
	}
}

// normalizeFixtureMessage drops keys the Go representation does not model, so
// the comparison is about repaired structure rather than serialization detail.
func normalizeFixtureMessage(m map[string]any) map[string]any {
	out := map[string]any{"role": m["role"]}
	if c, ok := m["content"]; ok {
		out["content"] = c
	}
	if v, ok := m["tool_call_id"]; ok {
		out["tool_call_id"] = v
	}
	if v, ok := m["name"]; ok {
		out["name"] = v
	}
	if v, ok := m["tool_calls"]; ok {
		calls, _ := v.([]any)
		norm := make([]any, 0, len(calls))
		for _, rc := range calls {
			call, _ := rc.(map[string]any)
			if call == nil {
				continue
			}
			fn, _ := call["function"].(map[string]any)
			norm = append(norm, map[string]any{
				"id":   call["id"],
				"type": "function",
				"function": map[string]any{
					"name":      fn["name"],
					"arguments": fn["arguments"],
				},
			})
		}
		out["tool_calls"] = norm
	}
	return out
}

func normalizeGoMessage(m map[string]any) map[string]any { return m }

func renderAll(msgs []core.Message) []any {
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, msgToComparable(m))
	}
	return out
}

func renderAllFixture(msgs []map[string]any) []any {
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, normalizeFixtureMessage(m))
	}
	return out
}

// TestInputBudgetMatchesPythonReference pins the input budget arithmetic.
//
// The zero case is the subtle one: the reference guards with
// isinstance(max_tokens, int), and 0 IS an int, so an explicit 0 reserves no
// output tokens instead of falling back to the provider default.
func TestInputBudgetMatchesPythonReference(t *testing.T) {
	ref := loadReference(t)
	if len(ref.Budget) == 0 {
		t.Fatal("reference produced no budget cases")
	}
	for _, tc := range ref.Budget {
		maxTokens := tc.MaxTokens
		got := agent.InputBudget(tc.Window, &maxTokens)
		if got != tc.Budget {
			t.Errorf("InputBudget(window=%d, max_tokens=%d) = %d, python = %d",
				tc.Window, tc.MaxTokens, got, tc.Budget)
		}
	}
}

// ---------------------------------------------------------------------------
// 8. Tool-result normalization
// ---------------------------------------------------------------------------

// TestToolResultNormalizationMatchesPythonReference compares the empty-result
// marker, the filename sanitizer and the offload reference text.
//
// These strings reach the model verbatim, so a divergence is not cosmetic: a
// different marker changes what the model believes happened, and a different
// sanitizer writes tool output to a different path than the Python runtime
// would, splitting the two implementations' state.
func TestToolResultNormalizationMatchesPythonReference(t *testing.T) {
	ref := loadReference(t)
	tr := ref.ToolResults
	if len(tr.EmptyMarkers) == 0 {
		t.Fatal("reference produced no tool-result data")
	}

	t.Run("empty_markers", func(t *testing.T) {
		for name, want := range tr.EmptyMarkers {
			if got := agent.EmptyToolResultMessage(name); got != want {
				t.Errorf("EmptyToolResultMessage(%q) = %q, want %q", name, got, want)
			}
		}
	})

	t.Run("safe_filenames", func(t *testing.T) {
		for name, want := range tr.SafeFilenames {
			if got := agent.SafeFilename(name); got != want {
				t.Errorf("SafeFilename(%q) = %q, want %q", name, got, want)
			}
		}
	})

	t.Run("emptiness", func(t *testing.T) {
		// Build the same inputs the Python harness used.
		build := map[string]core.Content{
			"none":         {},
			"empty_string": core.TextContent(""),
			"whitespace":   core.TextContent("   \n\t "),
			"text":         core.TextContent("real output"),
			"empty_list":   core.BlockContent([]core.ContentBlock{}),
			"blank_blocks": core.BlockContent([]core.ContentBlock{{Type: "text", Text: "  "}}),
			"text_blocks":  core.BlockContent([]core.ContentBlock{{Type: "text", Text: "hello"}}),
			"image_block": core.BlockContent([]core.ContentBlock{
				{Type: "image_url", ImageURL: json.RawMessage(`{"url":"x"}`)}}),
		}
		for _, tc := range tr.Emptiness {
			in, ok := build[tc.Label]
			if !ok {
				t.Errorf("case %q has no Go counterpart", tc.Label)
				continue
			}
			got := agent.EnsureNonemptyContent("exec", in)

			// Compare only the emptiness DECISION: the reference returns the
			// original list object when it is non-empty, so the payload is
			// compared by whether it was replaced by the marker.
			wantStr, wantIsStr := tc.Result.(string)
			if wantIsStr {
				if !got.IsText() || got.Text != wantStr {
					t.Errorf("case %q: got %+v, want the marker %q", tc.Label, got, wantStr)
				}
				continue
			}
			if got.IsText() {
				t.Errorf("case %q: content was replaced by %q but python kept the payload",
					tc.Label, got.Text)
			}
		}
	})

	t.Run("rendered_reference", func(t *testing.T) {
		cases := map[string]struct {
			path      string
			size      int
			preview   string
			truncated bool
			maxChars  int
		}{
			"plain":               {"/w/a.txt", 5000, "abc", true, 16000},
			"untruncated_preview": {"/w/a.txt", 10, "abc", false, 16000},
			"max_chars_overflow":  {"/w/a.txt", 5000, "abc", true, 40},
		}
		for _, tc := range tr.Rendered {
			in, ok := cases[tc.Label]
			if !ok {
				t.Errorf("case %q has no Go counterpart", tc.Label)
				continue
			}
			got := agent.RenderToolResultReference(in.path, in.size, in.preview, in.truncated, in.maxChars)
			if got != tc.Result {
				t.Errorf("case %q differs:\n  go:     %q\n  python: %q", tc.Label, got, tc.Result)
			}
		}
	})

	t.Run("read_file_is_exempt", func(t *testing.T) {
		for _, tc := range tr.Normalize {
			// read_file has its own bound; offloading it would create a
			// persist -> read -> persist loop.
			content := core.TextContent(strings.Repeat("x", 500))
			got := agent.NormalizeToolResult("", "cli:1", "c2", "read_file", content, 50)
			if !got.IsText() || got.Text != tc.ReadFileExempt {
				t.Errorf("case %q: read_file result was modified (len %d, want %d)",
					tc.Label, len(got.Text), len(tc.ReadFileExempt))
			}
		}
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestFloatFormattingMatchesPythonReference compares pyjson.FormatFloat against
// the strings json.dumps actually produced.
//
// This is the int/float distinction on disk: Go writes 1.0 as "1", Python
// writes "1.0", and a value saved by one runtime then read by the other
// silently changes type. The reference strings also pin the scientific-notation
// thresholds, which are Python's and not Go's.
func TestFloatFormattingMatchesPythonReference(t *testing.T) {
	ref := loadReference(t)
	if len(ref.PyJSON.Floats) == 0 {
		t.Fatal("reference produced no float data")
	}

	// Python's repr() is the key because json.dumps emits bare Infinity/NaN,
	// which is not valid JSON. The table is explicit rather than parsed so a
	// typo is a test failure, not a silently different float.
	byRepr := map[string]float64{
		"1.0": 1.0, "0.1": 0.1, "2.5": 2.5, "100.0": 100.0,
		"1000000000000000.0": 1e15, "1e+16": 1e16, "1e+17": 1e17, "1e+20": 1e20,
		"0.0001": 1e-4, "1e-05": 1e-5, "1e-07": 1e-7,
		"0.0": 0.0, "-0.0": math.Copysign(0, -1), "-1.5": -1.5,
		"3.141592653589793": 3.141592653589793, "1.5e+300": 1.5e300,
		"inf": math.Inf(1), "-inf": math.Inf(-1), "nan": math.NaN(),
	}

	checked := 0
	for _, tc := range ref.PyJSON.Floats {
		in, ok := byRepr[tc.Repr]
		if !ok {
			t.Errorf("no Go mapping for Python repr %q", tc.Repr)
			continue
		}
		got := pyjson.FormatFloat(in)
		if got != tc.Encoded {
			t.Errorf("FormatFloat(%s) = %s, want %s", tc.Repr, got, tc.Encoded)
			continue
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no float cases were compared")
	}
	t.Logf("compared %d float renderings against Python", checked)
}

// TestStringEscapingMatchesPythonReference covers the characters where Go's
// encoder and json.dumps(..., ensure_ascii=False) disagree: <, >, & (Go escapes
// them by default) and U+2028/U+2029 (Go escapes them unconditionally, and
// SetEscapeHTML(false) does not disable it).
func TestStringEscapingMatchesPythonReference(t *testing.T) {
	ref := loadReference(t)
	if len(ref.PyJSON.Strings) == 0 {
		t.Fatal("reference produced no string data")
	}

	checked := 0
	for _, tc := range ref.PyJSON.Strings {
		got := goEncodeString(tc.Value)
		if got != tc.Encoded {
			t.Errorf("encoding %q = %s, want %s", tc.Value, got, tc.Encoded)
			continue
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no string cases were compared")
	}
	t.Logf("compared %d string renderings against Python", checked)
}

// goEncodeString renders s exactly as Config.Save writes it: Go's encoder with
// HTML escaping off, then the line separators reconciled to Python's output.
func goEncodeString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		panic(err)
	}
	return string(pyjson.UnescapeLineSeparators(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))))
}

// TestLengthRecoveryMatchesPythonReference pins the whitespace repair and the
// continuation prompt against the reference's own output.
//
// RestoreOuterWhitespace looks like it should be a no-op when content equals
// original, and it is not: the base hook's finalize_content is the identity, so
// nothing was stripped and the boundary whitespace is re-appended, doubling it.
// That is the reference's behaviour, and reproducing it is the point.
func TestLengthRecoveryMatchesPythonReference(t *testing.T) {
	ref := loadReference(t)
	lr := ref.LengthRecovery
	if len(lr.Restore) == 0 || len(lr.Messages) == 0 {
		t.Fatal("reference produced no length-recovery data")
	}

	t.Run("restore_outer_whitespace", func(t *testing.T) {
		for _, tc := range lr.Restore {
			got := agent.RestoreOuterWhitespace(tc.Content, tc.Original)
			if got != tc.Result {
				t.Errorf("RestoreOuterWhitespace(%q, %q) = %q, want %q",
					tc.Content, tc.Original, got, tc.Result)
			}
		}
		t.Logf("compared %d whitespace cases", len(lr.Restore))
	})

	t.Run("recovery_message", func(t *testing.T) {
		for _, tc := range lr.Messages {
			msg := agent.BuildLengthRecoveryMessage(tc.Content)
			if string(msg.Role) != tc.Message.Role {
				t.Errorf("role = %q, want %q", msg.Role, tc.Message.Role)
			}
			if !msg.Content.IsText() {
				t.Fatalf("content is not text for input len %d", len(tc.Content))
			}
			if msg.Content.Text != tc.Message.Content {
				t.Errorf("input len %d: message differs from the reference\n got: %q\nwant: %q",
					len(tc.Content), msg.Content.Text, tc.Message.Content)
			}
		}
		t.Logf("compared %d recovery messages", len(lr.Messages))
	})
}

// TestStripThinkMatchesPythonReference covers a helper whose reference
// implementation uses a backreference and a negative lookahead, neither of
// which Go's RE2 supports. Both were rewritten, and this corpus is what proves
// the rewrites are equivalent rather than merely close.
//
// The corpus is adversarial on purpose: the alternation order decides the
// outcome for `<thinking广场`, and the character class is deliberately
// ASCII-only so that a CJK character after `<think` counts as a leak. Using
// Go's `\w` would reintroduce the bug the reference fixed.
func TestStripThinkMatchesPythonReference(t *testing.T) {
	ref := loadReference(t)
	if len(ref.StripThink) == 0 {
		t.Fatal("reference produced no strip_think data")
	}
	bad := 0
	for _, tc := range ref.StripThink {
		got := textutil.StripThink(tc.In)
		if got != tc.Out {
			bad++
			t.Errorf("StripThink(%q)\n got: %q\nwant: %q", tc.In, got, tc.Out)
		}
	}
	if bad > 0 {
		t.Fatalf("%d of %d cases differ", bad, len(ref.StripThink))
	}
	t.Logf("compared %d strip_think cases", len(ref.StripThink))
}

// TestTruncateTextMatchesPythonReference pins the unit and the overshoot.
//
// Two things here are counter-intuitive and were both wrong in the first port:
// the limit counts CHARACTERS, not bytes, so a CJK string is not cut to a third
// of its length; and the suffix is appended after the cut, so the result is
// longer than the limit rather than clamped to it.
func TestTruncateTextMatchesPythonReference(t *testing.T) {
	ref := loadReference(t)
	if len(ref.TruncateText) == 0 {
		t.Fatal("reference produced no truncate_text data")
	}
	for _, tc := range ref.TruncateText {
		got := textutil.TruncateText(tc.Text, tc.Limit)
		if got != tc.Out {
			t.Errorf("TruncateText(%d chars, %d)\n got: %q (%d chars)\nwant: %q (%d chars)",
				len([]rune(tc.Text)), tc.Limit, got, len([]rune(got)), tc.Out, len([]rune(tc.Out)))
		}
	}
	t.Logf("compared %d truncate_text cases", len(ref.TruncateText))
}

// TestPyStripMatchesPythonReference pins the whitespace definition itself.
//
// This is not a stylistic check. Go's unicode.IsSpace and Python's
// str.isspace() disagree on exactly four code points (U+001C..U+001F), so
// strings.TrimSpace is a silently wrong substitute for Python's str.strip().
// The reference enumerates its own whitespace set, and the Go table must
// reproduce that set exactly — no more and no fewer code points.
func TestPyStripMatchesPythonReference(t *testing.T) {
	ref := loadReference(t)
	if len(ref.PyStr.Cases) == 0 {
		t.Fatal("reference produced no pystr data")
	}

	// The set itself, code point by code point.
	if len(ref.PyStr.WhitespaceCodepoints) == 0 {
		t.Fatal("reference produced no whitespace code points")
	}
	for _, cp := range ref.PyStr.WhitespaceCodepoints {
		if !textutil.PyIsSpace(rune(cp)) {
			t.Errorf("PyIsSpace(U+%04X) = false, reference says whitespace", cp)
		}
	}
	// Exhaustive negative check: no code point outside the reference set may
	// be reported as whitespace. Sampling would miss exactly the kind of
	// near-miss this test exists to catch.
	inSet := make(map[rune]bool, len(ref.PyStr.WhitespaceCodepoints))
	for _, cp := range ref.PyStr.WhitespaceCodepoints {
		inSet[rune(cp)] = true
	}
	for cp := rune(0); cp <= 0x10FFFF; cp++ {
		if cp >= 0xD800 && cp <= 0xDFFF {
			continue // surrogate range is not valid UTF-8 and cannot appear
		}
		if got, want := textutil.PyIsSpace(cp), inSet[cp]; got != want {
			t.Fatalf("PyIsSpace(U+%04X) = %v, reference says %v", cp, got, want)
		}
	}

	bad := 0
	for _, tc := range ref.PyStr.Cases {
		if got := textutil.PyStrip(tc.In); got != tc.Strip {
			bad++
			t.Errorf("PyStrip(%q)\n got: %q\nwant: %q", tc.In, got, tc.Strip)
		}
		if got := textutil.PyRStrip(tc.In); got != tc.RStrip {
			bad++
			t.Errorf("PyRStrip(%q)\n got: %q\nwant: %q", tc.In, got, tc.RStrip)
		}
		if got := textutil.PyLStrip(tc.In); got != tc.LStrip {
			bad++
			t.Errorf("PyLStrip(%q)\n got: %q\nwant: %q", tc.In, got, tc.LStrip)
		}
	}
	if bad > 0 {
		t.Fatalf("%d of %d strip cases differ", bad, len(ref.PyStr.Cases))
	}
	t.Logf("compared %d whitespace code points and %d strip cases",
		len(ref.PyStr.WhitespaceCodepoints), len(ref.PyStr.Cases))
}

// ---------------------------------------------------------------------------
// 9. Dream memory consolidation
// ---------------------------------------------------------------------------

// dreamFormatDivergences names the reference cases whose expected value this
// port deliberately does not reproduce, with the reason.
//
// Only one shape diverges, and it is not reachable from a plain text
// transcript: a Python dict preserves insertion order and a Go map does not, so
// a JSON object nested inside a multimodal content list is rendered with sorted
// keys here where Python renders document order. Single-key objects — the shape
// the corpus also covers — render identically.
var dreamFormatDivergences = map[string]string{
	"multimodal_nested": "Go maps have no key order; a nested object is rendered with sorted keys",
}

// dreamNestedDivergence is the exact string this port produces for that case.
// Pinning it keeps the divergence honest: if the rendering ever changes, this
// fails rather than drifting silently away from the documented behaviour.
const dreamNestedDivergence = "[t] USER: [{'b': True, 'f': 1.5, 'n': 1, 'nul': None, 'type': 'text'}]"

// TestDreamMatchesPythonReference compares the Dream subsystem against the real
// Python implementation over the corpus in dump_dream().
//
// Three things here cannot be checked any other way:
//
//   - `_format_messages` stringifies with str(), so the corpus contains an int
//     timestamp, a bool role and a multimodal content list. A port that reaches
//     for fmt.Sprint or a byte slice diverges on exactly those cases.
//   - The runtime-context marker's version test is a Python VALUE comparison
//     (`marker.get("version") != 1`), so {"version": true} and
//     {"version": 1.0} are stripped while {"version": "1"} is not.
//   - The rendered Dream prompt must equal Jinja2's
//     render_template("agent/dream.md", strip=True, ...) byte for byte, which
//     is a claim about the embedded template copy AND about rstrip semantics.
func TestDreamMatchesPythonReference(t *testing.T) {
	ref := loadReferenceNumbers(t)
	d := ref.Dream
	if len(d.CommitMessages) == 0 || len(d.FormatMessages) == 0 || len(d.RawCheckpoints) == 0 ||
		len(d.PublicHistory) == 0 || len(d.RunStatus) == 0 || len(d.MediaBreadcrumbs) == 0 ||
		len(d.ImagePlaceholders) == 0 {
		t.Fatal("reference produced an incomplete dream section")
	}

	compared := 0
	divergent := 0

	t.Run("commit_message", func(t *testing.T) {
		for _, tc := range d.CommitMessages {
			body := ""
			if tc.DiffBody != nil {
				body = *tc.DiffBody
			}
			got := memory.BuildDreamCommitMessage(tc.Prefix, body)
			if got != tc.Out {
				t.Errorf("BuildDreamCommitMessage(%q, %q)\n got: %q\nwant: %q",
					tc.Prefix, body, got, tc.Out)
				continue
			}
			compared++
		}
	})

	t.Run("session_key", func(t *testing.T) {
		key := memory.DreamSessionKey()
		prefix, stamp, found := strings.Cut(key, ":")
		if !found {
			t.Fatalf("DreamSessionKey() = %q, no separator", key)
		}
		if prefix != d.SessionKey.Prefix {
			t.Errorf("prefix = %q, want %q", prefix, d.SessionKey.Prefix)
		}
		if len(stamp) != d.SessionKey.TimestampLen {
			t.Errorf("timestamp %q has length %d, want %d", stamp, len(stamp), d.SessionKey.TimestampLen)
		}
		if !d.SessionKey.MatchesFormat {
			t.Fatalf("reference timestamp %q does not match %%Y%%m%%d-%%H%%M%%S", d.SessionKey.Timestamp)
		}
		// Same shape, not the same value: the reference reads the clock too.
		if len(stamp) != 15 || stamp[8] != '-' {
			t.Errorf("timestamp %q is not <8 digits>-<6 digits>", stamp)
		}
		for _, c := range stamp {
			if c != '-' && (c < '0' || c > '9') {
				t.Errorf("timestamp %q contains %q", stamp, c)
			}
		}
		compared++
	})

	t.Run("default_prompt", func(t *testing.T) {
		// 1. The embedded copy must be the upstream file, byte for byte.
		path := filepath.Join(repoRoot(t), "internal", "prompt", "templates", "agent", "dream.md")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read embedded template: %v", err)
		}
		if string(raw) != d.Prompt.TemplateSource {
			t.Errorf("internal/prompt/templates/agent/dream.md differs from the upstream template (go: %d bytes, python: %d bytes)",
				len(raw), len(d.Prompt.TemplateSource))
		}

		// 2. Rendering with the reference's own skill_creator_path must equal
		//    what Jinja2 produced, including the rstrip.
		if got := prompt.RenderDreamPrompt(d.Prompt.SkillCreatorPath); got != d.Prompt.Rendered {
			t.Errorf("RenderDreamPrompt(%q) differs from the reference rendering\n got: %q\nwant: %q",
				d.Prompt.SkillCreatorPath, got, d.Prompt.Rendered)
		}

		// 3. The store-level default, with BUILTIN_SKILLS_DIR substituted for
		//    the reference's value.
		saved := memory.BuiltinSkillsDir
		memory.BuiltinSkillsDir = d.Prompt.BuiltinSkillsDir
		defer func() { memory.BuiltinSkillsDir = saved }()
		if got := memory.DefaultDreamPrompt(); got != d.Prompt.Rendered {
			t.Errorf("DefaultDreamPrompt() differs from the reference rendering\n got: %q\nwant: %q",
				got, d.Prompt.Rendered)
		}
		if got, want := memory.SkillCreatorPath(), d.Prompt.SkillCreatorPath; got != want {
			t.Errorf("SkillCreatorPath() = %q, want %q", got, want)
		}
		compared += 4
	})

	t.Run("media_breadcrumbs", func(t *testing.T) {
		for _, tc := range d.MediaBreadcrumbs {
			got := textutil.ContentWithMediaBreadcrumbs(tc.Role, tc.Content, tc.Media)
			if !reflect.DeepEqual(got, tc.Out) {
				t.Errorf("ContentWithMediaBreadcrumbs(%q, %#v, %#v)\n got: %#v\nwant: %#v",
					tc.Role, tc.Content, tc.Media, got, tc.Out)
				continue
			}
			compared++
		}
	})

	t.Run("image_placeholders", func(t *testing.T) {
		for _, tc := range d.ImagePlaceholders {
			path := ""
			if tc.Path != nil {
				path = *tc.Path
			}
			if got := textutil.ImagePlaceholderText(path); got != tc.Out {
				t.Errorf("ImagePlaceholderText(%q) = %q, want %q", path, got, tc.Out)
				continue
			}
			compared++
		}
	})

	t.Run("format_messages", func(t *testing.T) {
		for _, tc := range d.FormatMessages {
			got := memory.FormatMessages(tc.Messages)
			if reason, isDivergent := dreamFormatDivergences[tc.Label]; isDivergent {
				if got == tc.Out {
					t.Errorf("case %q was expected to diverge (%s) but reproduced the reference exactly",
						tc.Label, reason)
				}
				if tc.Label == "multimodal_nested" && got != dreamNestedDivergence {
					t.Errorf("case %q: sorted-key rendering changed\n got: %q\nwant: %q",
						tc.Label, got, dreamNestedDivergence)
				}
				divergent++
				continue
			}
			if got != tc.Out {
				t.Errorf("FormatMessages(%s)\n got: %q\nwant: %q", tc.Label, got, tc.Out)
				continue
			}
			compared++
		}
	})

	t.Run("raw_checkpoint", func(t *testing.T) {
		if d.RawArchiveMaxChars != 16000 {
			t.Errorf("reference _RAW_ARCHIVE_MAX_CHARS = %d, want 16000", d.RawArchiveMaxChars)
		}
		store, err := memory.NewMemoryStore(t.TempDir(), 0)
		if err != nil {
			t.Fatalf("NewMemoryStore: %v", err)
		}
		for _, tc := range d.RawCheckpoints {
			got := store.BuildRawCheckpoint(tc.Messages, tc.MaxChars)
			if got != tc.Out {
				t.Errorf("BuildRawCheckpoint(%s, max_chars=%v)\n got: %q\nwant: %q",
					tc.Label, tc.MaxChars, got, tc.Out)
				continue
			}
			compared++
		}
	})

	t.Run("build_dream_prompt", func(t *testing.T) {
		// The prompt embeds the built-in template, whose one variable is the
		// bundled skill-creator path. Substituting the reference's own value
		// makes the two sides comparable byte for byte.
		saved := memory.BuiltinSkillsDir
		memory.BuiltinSkillsDir = d.Prompt.BuiltinSkillsDir
		defer func() { memory.BuiltinSkillsDir = saved }()

		if len(d.BuildDreamPrompt) == 0 || d.PromptHistoryJSONL == "" {
			t.Fatal("reference produced no build_dream_prompt cases")
		}
		for _, tc := range d.BuildDreamPrompt {
			ws := t.TempDir()
			if err := os.MkdirAll(filepath.Join(ws, "memory"), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(ws, "memory", "history.jsonl"),
				[]byte(d.PromptHistoryJSONL), 0o644); err != nil {
				t.Fatalf("write history: %v", err)
			}
			store, err := memory.NewMemoryStore(ws, 0)
			if err != nil {
				t.Fatalf("NewMemoryStore: %v", err)
			}
			if err := store.SetLastDreamCursor(tc.LastCursor); err != nil {
				t.Fatalf("SetLastDreamCursor: %v", err)
			}

			prompt, cursor, ok, err := store.BuildDreamPrompt(tc.MaxEntries)
			switch {
			case tc.Error != nil:
				// The reference raised (IndexError for an empty batch); the
				// port reports an error instead of an empty prompt.
				if err == nil {
					t.Errorf("%s: expected an error (reference raised %s), got prompt %q",
						tc.Label, *tc.Error, prompt)
					continue
				}
			case tc.Out == nil:
				if err != nil || ok {
					t.Errorf("%s: expected nothing to process, got ok=%v err=%v", tc.Label, ok, err)
					continue
				}
			default:
				if err != nil {
					t.Errorf("%s: BuildDreamPrompt: %v", tc.Label, err)
					continue
				}
				if !ok {
					t.Errorf("%s: ok = false, want a prompt", tc.Label)
					continue
				}
				if prompt != *tc.Out {
					t.Errorf("%s: prompt differs (go %d chars, python %d chars)\n go: %q\nwant: %q",
						tc.Label, len([]rune(prompt)), len([]rune(*tc.Out)), tail(prompt), tail(*tc.Out))
					continue
				}
				if tc.Cursor != nil && cursor != *tc.Cursor {
					t.Errorf("%s: cursor = %d, want %d", tc.Label, cursor, *tc.Cursor)
					continue
				}
			}
			compared++
		}
	})

	t.Run("public_history", func(t *testing.T) {
		for _, tc := range d.PublicHistory {
			got := runtimecontext.PublicHistoryMessage(tc.Message)
			if !reflect.DeepEqual(got, tc.Out) {
				t.Errorf("PublicHistoryMessage(%s)\n got: %#v\nwant: %#v", tc.Label, got, tc.Out)
				continue
			}
			compared++
		}
	})

	t.Run("run_status", func(t *testing.T) {
		for _, tc := range d.RunStatus {
			var resp *core.OutboundMessage
			switch tc.Kind {
			case "no_response":
				resp = nil
			case "metadata_none", "metadata_list":
				// core.OutboundMessage.Metadata is a map, so a non-dict
				// metadata has no Go representation. Both of the reference's
				// cases answer "missing response metadata", which is what a
				// nil map answers here.
				resp = &core.OutboundMessage{}
			default:
				metadata, _ := tc.Metadata.(map[string]any)
				resp = &core.OutboundMessage{Metadata: metadata}
			}
			if got := memory.DreamRunCompleted(resp); got != tc.Completed {
				t.Errorf("DreamRunCompleted(%s) = %v, want %v", tc.Label, got, tc.Completed)
			}
			if got := memory.DreamIncompletionReason(resp); got != tc.Reason {
				t.Errorf("DreamIncompletionReason(%s) = %q, want %q", tc.Label, got, tc.Reason)
			}
			compared += 2
		}
	})

	if compared == 0 {
		t.Fatal("no dream cases were compared")
	}
	t.Logf("compared %d dream cases against Python (%d documented divergence(s))", compared, divergent)
}

// tail returns the last 120 characters of s, for readable diff output on
// multi-kilobyte prompts.
func tail(s string) string {
	runes := []rune(s)
	if len(runes) <= 120 {
		return s
	}
	return "..." + string(runes[len(runes)-120:])
}
