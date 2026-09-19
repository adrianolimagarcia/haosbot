// Telegram-channel differential test.
//
// This file drives the Go port of internal/channels/telegram through the same
// inputs as the frozen Python reference and compares the results element by
// element. compat/python/dump_telegram.py produces the reference values; see its
// docstring for how it obtains them (AST extraction plus two direct imports,
// because nanobot.channels.telegram.runtime cannot be imported without
// python-telegram-bot).
//
// Nothing here asserts against values transcribed by reading the Python. Every
// expectation comes out of the reference process at test time.
package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/telegram"
	"github.com/adrianolimagarcia/nanobot-go/internal/pairing"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// ---------------------------------------------------------------------------
// Reference document
// ---------------------------------------------------------------------------

type tgSplitCase struct {
	Content          string   `json:"content"`
	MaxLen           int      `json:"max_len"`
	JoinEqualsLstrip bool     `json:"join_equals_lstrip"`
	Chunks           []string `json:"chunks"`
}

type tgHTMLCase struct {
	Text        string `json:"text"`
	HTML        string `json:"html"`
	Stripped    string `json:"stripped"`
	StrippedBlk string `json:"stripped_block"`
	Escaped     string `json:"escaped"`
	Blockquote  string `json:"blockquote"`
}

type tgHTMLChunkCase struct {
	Content    string     `json:"content"`
	MaxHTMLLen int        `json:"max_html_len"`
	OK         bool       `json:"ok"`
	Chunks     [][]string `json:"chunks"`
	HTMLOnly   []string   `json:"html_only"`
	Error      string     `json:"error"`
}

type tgTableBoxCase struct {
	Lines []string `json:"lines"`
	Out   string   `json:"out"`
}

type tgProxyCase struct {
	Input string `json:"input"`
	Valid bool   `json:"valid"`
}

type tgFieldError struct {
	Type string `json:"type"`
	Loc  []any  `json:"loc"`
	Msg  string `json:"msg"`
}

type tgConfigCase struct {
	Values map[string]any `json:"values"`
	OK     bool           `json:"ok"`
	Dump   map[string]any `json:"dump"`
	Errors []tgFieldError `json:"errors"`
}

type tgAllowedCase struct {
	Config     map[string]any `json:"config"`
	ConfigForm string         `json:"config_form"`
	ConfigDump map[string]any `json:"config_dump"`
	Approved   []string       `json:"approved"`
	Sender     string         `json:"sender"`
	Result     bool           `json:"result"`
}

type tgNormalizeCase struct {
	Input string `json:"input"`
	Out   string `json:"out"`
}

type tgDisplayLineCase struct {
	Input   string   `json:"input"`
	Findall []string `json:"findall"`
	Sub     string   `json:"sub"`
}

type tgBusSlashCase struct {
	Input string `json:"input"`
	Match bool   `json:"match"`
}

type tgEntityCase struct {
	Type   string `json:"type"`
	Offset *int   `json:"offset"`
	Length *int   `json:"length"`
	UserID *int64 `json:"user_id"`
}

type tgMentionCase struct {
	Text        string         `json:"text"`
	BotUsername string         `json:"bot_username"`
	BotID       *int64         `json:"bot_id"`
	Entities    []tgEntityCase `json:"entities"`
	Result      bool           `json:"result"`
}

type tgGroupCase struct {
	ChatType        string         `json:"chat_type"`
	GroupPolicy     string         `json:"group_policy"`
	BotID           *int64         `json:"bot_id"`
	BotUsername     string         `json:"bot_username"`
	Text            *string        `json:"text"`
	Caption         *string        `json:"caption"`
	Entities        []tgEntityCase `json:"entities"`
	CaptionEntities []tgEntityCase `json:"caption_entities"`
	ReplyUserID     *int64         `json:"reply_user_id"`
	Result          bool           `json:"result"`
}

type tgValidateCase struct {
	Values   map[string]any `json:"values"`
	Scenario string         `json:"scenario"`
	OK       bool           `json:"ok"`
	Error    string         `json:"error"`
	Payload  map[string]any `json:"payload"`
}

type tgSetupField struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Choices  []string `json:"choices"`
	Default  any      `json:"default"`
	Writable bool     `json:"writable"`
	Snapshot bool     `json:"snapshot"`
}

type tgSetupSpec struct {
	Fields   []tgSetupField `json:"fields"`
	Required []struct {
		Alternatives [][]string `json:"alternatives"`
	} `json:"required"`
	OfficialURL        string   `json:"official_url"`
	VerifiesConnection bool     `json:"verifies_connection"`
	HasValidator       bool     `json:"has_validator"`
	Secrets            []string `json:"secrets"`
	SimpleRequired     []string `json:"simple_required_fields"`
	GroupPolicies      []string `json:"group_policies"`
}

type tgUnicode struct {
	UnidataVersion string  `json:"unidata_version"`
	EAWWide        [][]int `json:"east_asian_width_wide"`
	StrIsDigit     [][]int `json:"str_isdigit"`
	StrIsSpace     [][]int `json:"str_isspace"`
	ReUnicodeWord  [][]int `json:"re_unicode_word"`
	ReUnicodeSpace [][]int `json:"re_unicode_space"`
}

type telegramReference struct {
	UpstreamCommit string            `json:"upstream_commit"`
	Constants      map[string]any    `json:"constants"`
	Split          []tgSplitCase     `json:"split"`
	HTML           []tgHTMLCase      `json:"html"`
	HTMLChunks     []tgHTMLChunkCase `json:"html_chunks"`
	TableBox       []tgTableBoxCase  `json:"table_box"`
	Proxy          []tgProxyCase     `json:"proxy"`
	ConfigDefaults map[string]any    `json:"config_defaults"`
	Config         []tgConfigCase    `json:"config"`
	Allowed        []tgAllowedCase   `json:"allowed"`
	Commands       struct {
		Aliases          [][]string          `json:"aliases"`
		DisplayRePattern string              `json:"display_re_pattern"`
		DisplayReFindall [][]string          `json:"display_re_findall"`
		Normalize        []tgNormalizeCase   `json:"normalize"`
		CommandText      []tgNormalizeCase   `json:"command_text"`
		DisplayLines     []tgDisplayLineCase `json:"display_lines"`
		BusSlash         []tgBusSlashCase    `json:"bus_slash"`
	} `json:"commands"`
	Mentions     []tgMentionCase  `json:"mentions"`
	GroupMessage []tgGroupCase    `json:"group_message"`
	Validate     []tgValidateCase `json:"validate"`
	SetupSpec    tgSetupSpec      `json:"setup_spec"`
	Unicode      tgUnicode        `json:"unicode"`
}

// unicodeVersion is unicode.Version, the Unicode release Go's tables came from.
const unicodeVersion = unicode.Version

// The reference document is produced ONCE per test binary. Running the dumper
// costs about four seconds — it enumerates every code point several times — and
// there is one test per corpus section, so an uncached loader would spend a
// minute re-deriving the same bytes.
var (
	telegramRefOnce sync.Once
	telegramRefDoc  *telegramReference
	telegramRefErr  error
	telegramRefSkip string
)

// loadTelegramReference runs compat/python/dump_telegram.py and parses it.
func loadTelegramReference(t *testing.T) *telegramReference {
	t.Helper()
	telegramRefOnce.Do(func() {
		telegramRefDoc, telegramRefSkip, telegramRefErr = runTelegramDumper()
	})
	if telegramRefSkip != "" {
		t.Skip(telegramRefSkip)
	}
	if telegramRefErr != nil {
		t.Fatalf("telegram reference dumper failed: %v", telegramRefErr)
	}
	return telegramRefDoc
}

func runTelegramDumper() (*telegramReference, string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return nil, "", fmt.Errorf("cannot resolve caller path")
	}
	root := filepath.Dir(filepath.Dir(file))
	python := filepath.Join(root, ".tools", "venv", "bin", "python")
	script := filepath.Join(root, "compat", "python", "dump_telegram.py")
	if _, err := os.Stat(python); err != nil {
		return nil, fmt.Sprintf("SKIP: reference venv not present at %s — differential check not run", python), nil
	}
	if _, err := os.Stat(script); err != nil {
		return nil, fmt.Sprintf("SKIP: dumper missing at %s", script), nil
	}
	out, err := runReferenceCommand("telegram dumper", []string{python, script}, root, nil)
	if err != nil {
		return nil, "", err
	}
	var ref telegramReference
	dec := json.NewDecoder(bytes.NewReader(out))
	// UseNumber keeps every untyped number as json.Number, which is what the
	// config corpus needs: the reference distinguishes a JSON integer from a
	// JSON float (int coercion accepts 8080.0 but rejects 8080.5).
	dec.UseNumber()
	if err := dec.Decode(&ref); err != nil {
		return nil, "", fmt.Errorf("parse telegram reference output: %w", err)
	}
	return &ref, "", nil
}

// TestTelegramDifferentialCaseCount reports how many reference-derived
// assertions the harness covers.
//
// It exists because a differential suite that silently runs nothing looks
// exactly like one that passes. The count is derived from the CORPUS SIZES in
// the reference document, not from the running tally, so it does not depend on
// the order Go happens to run the tests in.
func TestTelegramDifferentialCaseCount(t *testing.T) {
	ref := loadTelegramReference(t)

	type section struct {
		name  string
		cases int
	}
	sections := []section{
		{"constants", len(ref.Constants)},
		{"split", len(ref.Split)},
		{"html", len(ref.HTML) * 5},
		{"html_chunks", len(ref.HTMLChunks)},
		{"table_box", len(ref.TableBox)},
		{"proxy", len(ref.Proxy)},
		{"config_defaults", len(ref.ConfigDefaults)},
		{"config", len(ref.Config)},
		{"allowed", len(ref.Allowed)},
		{"commands.aliases", len(ref.Commands.Aliases)},
		{"commands.display_re_pattern", 1},
		{"commands.normalize", len(ref.Commands.Normalize)},
		{"commands.display_lines", len(ref.Commands.DisplayLines) * 2},
		{"commands.display_re_findall", len(ref.Commands.DisplayReFindall)},
		{"commands.command_text", len(ref.Commands.CommandText)},
		{"commands.bus_slash", len(ref.Commands.BusSlash)},
		{"mentions", len(ref.Mentions)},
		{"group_message", len(ref.GroupMessage)},
		{"validate", len(ref.Validate)},
		{"setup_spec", len(ref.SetupSpec.Fields) + 6},
		{"unicode", 5},
	}
	total := 0
	for _, s := range sections {
		if s.cases == 0 {
			t.Errorf("section %s is EMPTY: the differential check would pass vacuously", s.name)
		}
		total += s.cases
		t.Logf("  %-28s %6d", s.name, s.cases)
	}
	if total == 0 {
		t.Fatal("no differential cases at all")
	}
	t.Logf("Telegram differential cases against the Python reference: %d", total)
	t.Logf("reference commit: %s", ref.UpstreamCommit)
}

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

func TestTelegramConstantsMatchPythonReference(t *testing.T) {
	ref := loadTelegramReference(t)

	// The reference spells these with their own private/public convention, so
	// the mapping is explicit rather than derived from the Go name. Every
	// constant the dumper emits must appear here, and vice versa: a constant
	// added to the port without being pinned would otherwise go unnoticed.
	goValues := map[string]any{
		"TELEGRAM_MAX_MESSAGE_LEN":         telegram.MaxMessageLen,
		"TELEGRAM_HTML_MAX_LEN":            telegram.HTMLMaxLen,
		"TELEGRAM_RICH_MAX_LEN":            telegram.RichMaxLen,
		"TELEGRAM_REPLY_CONTEXT_MAX_LEN":   telegram.ReplyContextMaxLen,
		"TELEGRAM_RICH_DRAFT_MIN_INTERVAL": telegram.RichDraftMinInterval,
		"COMPACTION_NOTICES_MAX":           telegram.CompactionNoticesMax,
		"POLL_STALE_SECONDS":               telegram.PollStaleSeconds,
		"POLL_WATCH_INTERVAL":              telegram.PollWatchInterval,
		"RESTART_BACKOFF_INITIAL_SECONDS":  telegram.RestartBackoffInitialSeconds,
		"RESTART_BACKOFF_MAX_SECONDS":      telegram.RestartBackoffMaxSeconds,
		"APP_RESTART_SEND_WAIT_SECONDS":    telegram.AppRestartSendWaitSeconds,
		"_SEND_MAX_RETRIES":                telegram.SendMaxRetries,
		"_SEND_RETRY_BASE_DELAY":           telegram.SendRetryBaseDelay,
		"_STREAM_EDIT_INTERVAL_DEFAULT":    telegram.StreamEditIntervalDefault,
	}
	if len(ref.Constants) != len(goValues) {
		t.Fatalf("reference exposes %d constants, the test maps %d; reference keys: %v",
			len(ref.Constants), len(goValues), sortedKeys(ref.Constants))
	}
	for name, got := range goValues {
		refValue, ok := ref.Constants[name]
		if !ok {
			t.Errorf("reference does not expose %s; the ported constant is unchecked", name)
			continue
		}
		if !jsonNumbersEqual(refValue, got) {
			t.Errorf("%s: Go=%v reference=%v", name, got, refValue)
			continue
		}
	}
	t.Logf("constants: %d matched", len(goValues))
}

// jsonNumbersEqual compares a decoded JSON number with a Go numeric value
// without caring which Go type it has.
func jsonNumbersEqual(ref any, got any) bool {
	refStr := fmt.Sprintf("%v", ref)
	gotStr := fmt.Sprintf("%v", got)
	if refStr == gotStr {
		return true
	}
	rf, err1 := strconv.ParseFloat(refStr, 64)
	gf, err2 := strconv.ParseFloat(gotStr, 64)
	return err1 == nil && err2 == nil && rf == gf
}

// ---------------------------------------------------------------------------
// Split
// ---------------------------------------------------------------------------

func TestTelegramSplitMatchesPythonReference(t *testing.T) {
	ref := loadTelegramReference(t)
	if len(ref.Split) == 0 {
		t.Fatal("split corpus is empty")
	}
	for i, tc := range ref.Split {
		got := telegram.SplitMarkdown(tc.Content, tc.MaxLen)
		if !equalStrings(got, tc.Chunks) {
			t.Fatalf("split[%d] content=%q max_len=%d:\n Go       =%q\n reference=%q",
				i, tc.Content, tc.MaxLen, got, tc.Chunks)
		}
		// The reference records whether the chunks rejoin to the lstripped
		// input. Asserting the recorded flag keeps the harness honest about
		// the fact that they usually do NOT (fences are re-emitted).
		joined := strings.Join(got, "")
		equals := joined == textutil.PyLStrip(tc.Content)
		if equals != tc.JoinEqualsLstrip {
			t.Fatalf("split[%d] join_equals_lstrip: Go=%v reference=%v", i, equals, tc.JoinEqualsLstrip)
		}
	}
	t.Logf("split: %d cases matched", len(ref.Split))
}

// ---------------------------------------------------------------------------
// Markdown -> Telegram HTML
// ---------------------------------------------------------------------------

func TestTelegramHTMLMatchesPythonReference(t *testing.T) {
	ref := loadTelegramReference(t)
	if len(ref.HTML) == 0 {
		t.Fatal("html corpus is empty")
	}
	for i, tc := range ref.HTML {
		check := func(name, got, want string) {
			if got != want {
				t.Fatalf("html[%d] %s input=%q:\n Go       =%q\n reference=%q", i, name, tc.Text, got, want)
			}
		}
		check("MarkdownToHTML", telegram.MarkdownToHTML(tc.Text), tc.HTML)
		check("StripMarkdownInline", telegram.StripMarkdownInline(tc.Text), tc.Stripped)
		check("StripMarkdownBlock", telegram.StripMarkdownBlock(tc.Text), tc.StrippedBlk)
		check("EscapeHTML", telegram.EscapeHTML(tc.Text), tc.Escaped)
		check("ToolHintBlockquote", telegram.ToolHintBlockquote(tc.Text), tc.Blockquote)
	}
	t.Logf("html: %d cases matched (5 conversions each)", len(ref.HTML))
}

func TestTelegramTableBoxMatchesPythonReference(t *testing.T) {
	ref := loadTelegramReference(t)
	if len(ref.TableBox) == 0 {
		t.Fatal("table_box corpus is empty")
	}
	for i, tc := range ref.TableBox {
		got := telegram.RenderTableBox(tc.Lines)
		if got != tc.Out {
			t.Fatalf("table_box[%d] lines=%q:\n Go       =%q\n reference=%q", i, tc.Lines, got, tc.Out)
		}
	}
	t.Logf("table_box: %d cases matched", len(ref.TableBox))
}

func TestTelegramHTMLChunksMatchPythonReference(t *testing.T) {
	ref := loadTelegramReference(t)
	if len(ref.HTMLChunks) == 0 {
		t.Fatal("html_chunks corpus is empty")
	}
	for i, tc := range ref.HTMLChunks {
		chunks, err := telegram.SplitMarkdownHTMLChunks(tc.Content, tc.MaxHTMLLen)
		if !tc.OK {
			if err == nil {
				t.Fatalf("html_chunks[%d] expected %q, got success", i, tc.Error)
			}
			if !strings.Contains(tc.Error, err.Error()) {
				t.Fatalf("html_chunks[%d] error: Go=%q reference=%q", i, err.Error(), tc.Error)
			}
			continue
		}
		if err != nil {
			t.Fatalf("html_chunks[%d] unexpected error: %v", i, err)
		}
		if len(chunks) != len(tc.Chunks) {
			t.Fatalf("html_chunks[%d] got %d chunks, reference has %d", i, len(chunks), len(tc.Chunks))
		}
		for j, pair := range tc.Chunks {
			if chunks[j].Markdown != pair[0] || chunks[j].HTML != pair[1] {
				t.Fatalf("html_chunks[%d][%d]: Go=(%q,%q) reference=(%q,%q)",
					i, j, chunks[j].Markdown, chunks[j].HTML, pair[0], pair[1])
			}
		}
		html, err := telegram.SplitMarkdownHTML(tc.Content, tc.MaxHTMLLen)
		if err != nil {
			t.Fatalf("html_chunks[%d] SplitMarkdownHTML: %v", i, err)
		}
		if !equalStrings(html, tc.HTMLOnly) {
			t.Fatalf("html_chunks[%d] html_only: Go=%q reference=%q", i, html, tc.HTMLOnly)
		}
	}
	t.Logf("html_chunks: %d cases matched", len(ref.HTMLChunks))
}

// ---------------------------------------------------------------------------
// Proxy URL validation
// ---------------------------------------------------------------------------

func TestTelegramProxyValidationMatchesPythonReference(t *testing.T) {
	ref := loadTelegramReference(t)
	if len(ref.Proxy) == 0 {
		t.Fatal("proxy corpus is empty")
	}
	for i, tc := range ref.Proxy {
		got := telegram.ProxyURLIsValid(tc.Input)
		if got != tc.Valid {
			t.Fatalf("proxy[%d] %q: Go=%v reference=%v", i, tc.Input, got, tc.Valid)
		}
	}
	t.Logf("proxy: %d cases matched", len(ref.Proxy))
}

// ---------------------------------------------------------------------------
// TelegramConfig
// ---------------------------------------------------------------------------

// configToAliasMap renders the Go Config with the reference's camelCase aliases,
// which is what model_dump(by_alias=True) produces.
func configToAliasMap(cfg telegram.Config) map[string]any {
	out := map[string]any{
		"enabled":               cfg.Enabled,
		"token":                 cfg.Token,
		"mode":                  cfg.Mode,
		"allowFrom":             stringSliceToAny(cfg.AllowFrom),
		"proxy":                 nil,
		"replyToMessage":        cfg.ReplyToMessage,
		"reactEmoji":            cfg.ReactEmoji,
		"groupPolicy":           cfg.GroupPolicy,
		"connectionPoolSize":    cfg.ConnectionPoolSize,
		"poolTimeout":           cfg.PoolTimeout,
		"streaming":             cfg.Streaming,
		"inlineKeyboards":       cfg.InlineKeyboards,
		"richMessages":          cfg.RichMessages,
		"streamEditInterval":    cfg.StreamEditInterval,
		"webhookUrl":            cfg.WebhookURL,
		"webhookListenHost":     cfg.WebhookListenHost,
		"webhookListenPort":     cfg.WebhookListenPort,
		"webhookPath":           cfg.WebhookPath,
		"webhookSecretToken":    cfg.WebhookSecretToken,
		"webhookMaxConnections": cfg.WebhookMaxConnections,
	}
	if cfg.Proxy != nil {
		out["proxy"] = *cfg.Proxy
	}
	return out
}

func stringSliceToAny(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

func TestTelegramConfigDefaultsMatchPythonReference(t *testing.T) {
	ref := loadTelegramReference(t)
	got := configToAliasMap(telegram.DefaultConfig())
	diff := diffJSONMaps("config_defaults", got, ref.ConfigDefaults)
	if diff != "" {
		t.Fatal(diff)
	}
	t.Logf("config_defaults: %d keys matched", len(ref.ConfigDefaults))
}

func TestTelegramConfigMatchesPythonReference(t *testing.T) {
	ref := loadTelegramReference(t)
	if len(ref.Config) == 0 {
		t.Fatal("config corpus is empty")
	}
	for i, tc := range ref.Config {
		values := configValues(tc.Values)
		cfg, err := telegram.ParseConfig(values)
		if tc.OK {
			if err != nil {
				t.Fatalf("config[%d] values=%v: unexpected error: %v", i, tc.Values, err)
			}
			got := configToAliasMap(cfg)
			if diff := diffJSONMaps(fmt.Sprintf("config[%d] values=%v", i, tc.Values), got, configValues(tc.Dump)); diff != "" {
				t.Fatal(diff)
			}
			continue
		}
		if err == nil {
			t.Fatalf("config[%d] values=%v: expected a ValidationError, got success", i, tc.Values)
		}
		var verr *telegram.ValidationError
		if !asValidationError(err, &verr) {
			t.Fatalf("config[%d] values=%v: error is %T, want *telegram.ValidationError", i, tc.Values, err)
		}
		if len(verr.Errors) != len(tc.Errors) {
			t.Fatalf("config[%d] values=%v: Go has %d errors (%v), reference has %d (%v)",
				i, tc.Values, len(verr.Errors), verr.Errors, len(tc.Errors), tc.Errors)
		}
		for j, want := range tc.Errors {
			gotErr := verr.Errors[j]
			if gotErr.Type != want.Type || gotErr.Msg != want.Msg || !equalLoc(gotErr.Loc, want.Loc) {
				t.Fatalf("config[%d] values=%v error[%d]:\n Go       ={%s %v %q}\n reference={%s %v %q}",
					i, tc.Values, j, gotErr.Type, gotErr.Loc, gotErr.Msg, want.Type, want.Loc, want.Msg)
			}
		}
	}
	t.Logf("config: %d cases matched", len(ref.Config))
}

// configValues passes the decoded values through unchanged.
//
// The reference document is decoded with json.Decoder.UseNumber, so every
// untyped number arrives as a json.Number whose literal text still says whether
// it was written as an integer or a float. ParseConfig classifies numbers by
// exactly that, which is what lets it accept 8080.0 and reject 8080.5 the way
// pydantic does. Decoding without UseNumber would collapse both to float64 and
// make the corpus unable to tell the two apart.
func configValues(in map[string]any) map[string]any {
	return in
}

func equalLoc(got []string, want []any) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != fmt.Sprintf("%v", want[i]) {
			return false
		}
	}
	return true
}

func asValidationError(err error, target **telegram.ValidationError) bool {
	if verr, ok := err.(*telegram.ValidationError); ok {
		*target = verr
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// is_allowed
// ---------------------------------------------------------------------------

func TestTelegramIsAllowedMatchesPythonReference(t *testing.T) {
	ref := loadTelegramReference(t)
	if len(ref.Allowed) == 0 {
		t.Fatal("allowed corpus is empty")
	}

	dir := t.TempDir()
	storePath := filepath.Join(dir, "pairing.json")
	store := pairing.NewStore(storePath)

	// Approvals are grouped so the store file is written once per distinct set.
	written := map[string]bool{}
	writeApprovals := func(approved []string) {
		key := strings.Join(approved, "\x00")
		if written[key] {
			return
		}
		written[key] = true
		list, _ := json.Marshal(approved)
		payload := []byte(`{"approved":{"telegram":` + string(list) + `}}`)
		if err := os.WriteFile(storePath, payload, 0o600); err != nil {
			t.Fatalf("write pairing store: %v", err)
		}
	}

	for i, tc := range ref.Allowed {
		writeApprovals(tc.Approved)
		var policy telegram.SenderPolicy
		switch tc.ConfigForm {
		case "dict":
			policy = telegram.NewSenderPolicy(mapSection(tc.Config), store)
		case "model":
			cfg, err := telegram.ParseConfig(configValues(tc.Config))
			if err != nil {
				t.Fatalf("allowed[%d] model config %v does not parse: %v", i, tc.Config, err)
			}
			policy = telegram.NewSenderPolicyForConfig(cfg, store)
		default:
			t.Fatalf("allowed[%d] unknown config_form %q", i, tc.ConfigForm)
		}
		got := policy.IsAllowed(tc.Sender)
		if got != tc.Result {
			t.Fatalf("allowed[%d] form=%s config=%v approved=%v sender=%q: Go=%v reference=%v",
				i, tc.ConfigForm, tc.Config, tc.Approved, tc.Sender, got, tc.Result)
		}
	}
	t.Logf("allowed: %d cases matched", len(ref.Allowed))
}

// mapSection wraps a decoded map in the channels.Section the policy takes,
// which is the reference's DICT config shape.
func mapSection(values map[string]any) channels.Section {
	return channels.NewMapSection(values)
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

func TestTelegramCommandsMatchPythonReference(t *testing.T) {
	ref := loadTelegramReference(t)

	if len(telegram.CommandAliases) != len(ref.Commands.Aliases) {
		t.Fatalf("alias count: Go=%d reference=%d", len(telegram.CommandAliases), len(ref.Commands.Aliases))
	}
	for i, pair := range ref.Commands.Aliases {
		if telegram.CommandAliases[i].Alias != pair[0] || telegram.CommandAliases[i].Canonical != pair[1] {
			t.Fatalf("alias[%d]: Go=(%s,%s) reference=(%s,%s)", i,
				telegram.CommandAliases[i].Alias, telegram.CommandAliases[i].Canonical, pair[0], pair[1])
		}
	}

	if telegram.DisplayCommandReSource != ref.Commands.DisplayRePattern {
		t.Fatalf("display regex source drifted:\n Go       =%q\n reference=%q",
			telegram.DisplayCommandReSource, ref.Commands.DisplayRePattern)
	}

	for i, tc := range ref.Commands.Normalize {
		got := telegram.NormalizeCommand(tc.Input)
		if got != tc.Out {
			t.Fatalf("normalize[%d] %q: Go=%q reference=%q", i, tc.Input, got, tc.Out)
		}
	}

	for i, tc := range ref.Commands.DisplayLines {
		found := telegram.FindDisplayCommands(tc.Input)
		if !equalStrings(found, tc.Findall) {
			t.Fatalf("display_lines[%d] findall %q:\n Go       =%q\n reference=%q", i, tc.Input, found, tc.Findall)
		}
		sub := telegram.ReplaceDisplayCommands(tc.Input)
		if sub != tc.Sub {
			t.Fatalf("display_lines[%d] sub %q:\n Go       =%q\n reference=%q", i, tc.Input, sub, tc.Sub)
		}
	}

	// findall on a multi-line text equals the concatenation of the per-line
	// matches: neither `\w`, `/`, `.` nor `-` matches a newline, so a line start
	// is always a valid boundary in both readings. That lets the whole
	// COMMAND_TEXT corpus pin the scanner too.
	for i, tc := range ref.Commands.DisplayReFindall {
		var got []string
		for _, line := range pySplitLines(tc0(ref.Commands.CommandText, i).Input) {
			got = append(got, telegram.FindDisplayCommands(line)...)
		}
		if !equalStrings(got, tc) {
			t.Fatalf("display_re_findall[%d]:\n Go       =%q\n reference=%q", i, got, tc)
		}
	}

	for i, tc := range ref.Commands.CommandText {
		got := telegram.DisplayCommandText(tc.Input)
		if got != tc.Out {
			t.Fatalf("command_text[%d] %q:\n Go       =%q\n reference=%q", i, tc.Input, got, tc.Out)
		}
	}

	for i, tc := range ref.Commands.BusSlash {
		got := telegram.BusSlashCommandRe.MatchString(tc.Input)
		if got != tc.Match {
			t.Fatalf("bus_slash[%d] %q: Go=%v reference=%v", i, tc.Input, got, tc.Match)
		}
	}
	t.Logf("commands: aliases=%d normalize=%d display_lines=%d findall=%d command_text=%d bus_slash=%d",
		len(ref.Commands.Aliases), len(ref.Commands.Normalize), len(ref.Commands.DisplayLines),
		len(ref.Commands.DisplayReFindall), len(ref.Commands.CommandText), len(ref.Commands.BusSlash))
}

func tc0(cases []tgNormalizeCase, i int) tgNormalizeCase {
	if i < len(cases) {
		return cases[i]
	}
	return tgNormalizeCase{}
}

// pySplitLines mirrors Python's str.splitlines (no keepends) closely enough for
// the newline-only corpus here; the full boundary set is covered by
// DisplayCommandText's own tests.
func pySplitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	parts := strings.Split(s, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// ---------------------------------------------------------------------------
// Mentions and group policy
// ---------------------------------------------------------------------------

func TestTelegramMentionsMatchPythonReference(t *testing.T) {
	ref := loadTelegramReference(t)
	if len(ref.Mentions) == 0 {
		t.Fatal("mentions corpus is empty")
	}
	for i, tc := range ref.Mentions {
		got := telegram.HasMentionEntity(tc.Text, toEntities(tc.Entities), tc.BotUsername, tc.BotID)
		if got != tc.Result {
			t.Fatalf("mentions[%d] text=%q entities=%v username=%q bot_id=%v: Go=%v reference=%v",
				i, tc.Text, tc.Entities, tc.BotUsername, tc.BotID, got, tc.Result)
		}
	}
	t.Logf("mentions: %d cases matched", len(ref.Mentions))
}

func toEntities(in []tgEntityCase) []telegram.MentionEntity {
	out := make([]telegram.MentionEntity, 0, len(in))
	for _, e := range in {
		out = append(out, telegram.MentionEntity{
			Type:   e.Type,
			Offset: e.Offset,
			Length: e.Length,
			UserID: e.UserID,
		})
	}
	return out
}

func TestTelegramGroupPolicyMatchesPythonReference(t *testing.T) {
	ref := loadTelegramReference(t)
	if len(ref.GroupMessage) == 0 {
		t.Fatal("group_message corpus is empty")
	}
	for i, tc := range ref.GroupMessage {
		in := telegram.GroupMessageInput{
			ChatType:        tc.ChatType,
			Entities:        toEntities(tc.Entities),
			CaptionEntities: toEntities(tc.CaptionEntities),
			ReplyToUserID:   tc.ReplyUserID,
		}
		if tc.Text != nil {
			in.Text = *tc.Text
		}
		if tc.Caption != nil {
			in.Caption = *tc.Caption
		}
		got := telegram.IsGroupMessageForBot(in, tc.GroupPolicy, tc.BotID, tc.BotUsername)
		if got != tc.Result {
			t.Fatalf("group_message[%d] %v: Go=%v reference=%v", i, tc, got, tc.Result)
		}
	}
	t.Logf("group_message: %d cases matched", len(ref.GroupMessage))
}

// ---------------------------------------------------------------------------
// validate()
// ---------------------------------------------------------------------------

// validateEnv is VALIDATE_ENV from the dumper: the exact environment
// resolve_env_refs sees on both sides.
var validateEnv = map[string]string{
	"TELEGRAM_BOT_TOKEN": "123456:ABCdefGHIjklMNOpqrs",
	"PROXY_URL":          "socks5h://proxy.local:1080",
}

// mockGetMe replays the dumper's GETME_SCENARIOS through the Go transport seam.
func mockGetMe(scenario string) telegram.GetMeFunc {
	return func(token, proxy string) (map[string]any, error) {
		switch scenario {
		case "ok_full":
			return map[string]any{"ok": true, "result": map[string]any{
				"id": json.Number("42"), "username": "nanobot", "first_name": "Nano"}}, nil
		case "ok_id_only":
			return map[string]any{"ok": true, "result": map[string]any{"id": json.Number("42")}}, nil
		case "ok_no_identity":
			return map[string]any{"ok": true, "result": map[string]any{}}, nil
		case "ok_result_not_dict":
			return map[string]any{"ok": true, "result": "nope"}, nil
		case "not_ok_description":
			return map[string]any{"ok": false, "description": "Unauthorized"}, nil
		case "not_ok_error":
			return map[string]any{"ok": false, "error": "bad token"}, nil
		case "not_ok_empty":
			return map[string]any{"ok": false}, nil
		case "http_400", "http_401", "http_403", "http_404", "http_429", "http_500":
			code, _ := strconv.Atoi(strings.TrimPrefix(scenario, "http_"))
			return nil, &telegram.HTTPStatusError{StatusCode: code}
		case "transport":
			return nil, &telegram.TransportError{}
		case "other":
			return nil, fmt.Errorf("boom")
		}
		panic("unknown scenario " + scenario)
	}
}

func TestTelegramValidateMatchesPythonReference(t *testing.T) {
	ref := loadTelegramReference(t)
	if len(ref.Validate) == 0 {
		t.Fatal("validate corpus is empty")
	}

	saved := map[string]*string{}
	for name, value := range validateEnv {
		if old, ok := os.LookupEnv(name); ok {
			saved[name] = &old
		} else {
			saved[name] = nil
		}
		os.Setenv(name, value)
	}
	defer func() {
		for name, old := range saved {
			if old == nil {
				os.Unsetenv(name)
				continue
			}
			os.Setenv(name, *old)
		}
	}()

	for i, tc := range ref.Validate {
		values := configValues(tc.Values)
		payload := telegram.Validate(values, telegram.ValidationContext{}, mockGetMe(tc.Scenario))
		if !tc.OK {
			t.Fatalf("validate[%d] values=%v scenario=%s: reference raised %s, Go returned a payload",
				i, tc.Values, tc.Scenario, tc.Error)
		}
		got := payloadToMap(payload)
		if diff := diffJSONMaps(fmt.Sprintf("validate[%d] values=%v scenario=%s", i, tc.Values, tc.Scenario), got, tc.Payload); diff != "" {
			t.Fatal(diff)
		}
	}
	t.Logf("validate: %d cases matched", len(ref.Validate))
}

// payloadToMap renders the Go Payload the way the reference dict is shaped,
// including the normalised checked_at.
func payloadToMap(p telegram.Payload) map[string]any {
	checks := make([]any, 0, len(p.Checks))
	for _, c := range p.Checks {
		entry := map[string]any{"id": c.ID, "label": c.Label, "status": c.Status}
		if c.Message != "" {
			entry["message"] = c.Message
		}
		if c.ActionURL != "" {
			entry["action_url"] = c.ActionURL
		}
		checks = append(checks, entry)
	}
	missing := make([]any, 0, len(p.MissingFields))
	for _, m := range p.MissingFields {
		missing = append(missing, m)
	}
	identity := map[string]any{}
	for k, v := range p.Identity {
		identity[k] = v
	}
	return map[string]any{
		"name":             p.Name,
		"status":           p.Status,
		"checks":           checks,
		"identity":         identity,
		"missing_fields":   missing,
		"can_enable":       p.CanEnable,
		"requires_restart": p.RequiresRestart,
		// The reference's checked_at is datetime.now(UTC).isoformat() and the
		// dumper replaces it with a sentinel, because it cannot be reproduced.
		"checked_at": "CHECKED_AT",
		"message":    p.Message,
	}
}

// ---------------------------------------------------------------------------
// Setup spec
// ---------------------------------------------------------------------------

func TestTelegramSetupSpecMatchesPythonReference(t *testing.T) {
	ref := loadTelegramReference(t)
	spec := telegram.SETUP_SPEC

	if len(spec.Fields) != len(ref.SetupSpec.Fields) {
		t.Fatalf("field count: Go=%d reference=%d", len(spec.Fields), len(ref.SetupSpec.Fields))
	}
	for i, want := range ref.SetupSpec.Fields {
		got := spec.Fields[i]
		if got.Name != want.Name || string(got.Kind) != want.Kind ||
			got.Writable != want.Writable || got.Snapshot != want.Snapshot {
			t.Fatalf("setup_spec field[%d]: Go=%+v reference=%+v", i, got, want)
		}
		choices := append([]string{}, got.Choices...)
		sort.Strings(choices)
		if !equalStrings(choices, want.Choices) {
			t.Fatalf("setup_spec field[%d] %s choices: Go=%q reference=%q", i, got.Name, choices, want.Choices)
		}
		if !defaultsEqual(got.Default, want.Default) {
			t.Fatalf("setup_spec field[%d] %s default: Go=%#v reference=%#v", i, got.Name, got.Default, want.Default)
		}
	}

	if len(spec.Required) != len(ref.SetupSpec.Required) {
		t.Fatalf("requirement count: Go=%d reference=%d", len(spec.Required), len(ref.SetupSpec.Required))
	}
	for i, want := range ref.SetupSpec.Required {
		got := spec.Required[i].Alternatives
		if len(got) != len(want.Alternatives) {
			t.Fatalf("requirement[%d] alternatives: Go=%v reference=%v", i, got, want.Alternatives)
		}
		for j := range got {
			if !equalStrings(got[j], want.Alternatives[j]) {
				t.Fatalf("requirement[%d][%d]: Go=%v reference=%v", i, j, got[j], want.Alternatives[j])
			}
		}
	}
	if spec.OfficialURL != ref.SetupSpec.OfficialURL {
		t.Fatalf("official_url: Go=%q reference=%q", spec.OfficialURL, ref.SetupSpec.OfficialURL)
	}
	if spec.VerifiesConnection != ref.SetupSpec.VerifiesConnection {
		t.Fatalf("verifies_connection: Go=%v reference=%v", spec.VerifiesConnection, ref.SetupSpec.VerifiesConnection)
	}
	if spec.HasValidator != ref.SetupSpec.HasValidator {
		t.Fatalf("has_validator: Go=%v reference=%v", spec.HasValidator, ref.SetupSpec.HasValidator)
	}
	if !equalStrings(spec.Secrets(), ref.SetupSpec.Secrets) {
		t.Fatalf("secrets: Go=%q reference=%q", spec.Secrets(), ref.SetupSpec.Secrets)
	}
	if !equalStrings(spec.SimpleRequiredFields(), ref.SetupSpec.SimpleRequired) {
		t.Fatalf("simple_required_fields: Go=%q reference=%q", spec.SimpleRequiredFields(), ref.SetupSpec.SimpleRequired)
	}
	if !equalStrings(telegram.GroupPolicies, ref.SetupSpec.GroupPolicies) {
		t.Fatalf("GROUP_POLICIES: Go=%q reference=%q", telegram.GroupPolicies, ref.SetupSpec.GroupPolicies)
	}
	t.Logf("setup_spec: %d fields matched", len(ref.SetupSpec.Fields))
}

func defaultsEqual(got any, want any) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return jsonNumbersEqual(want, got)
}

// ---------------------------------------------------------------------------
// Unicode tables
// ---------------------------------------------------------------------------

// TestTelegramUnicodeTablesMatchPythonReference walks every code point and
// compares the Go predicates with the reference's own range tables.
//
// This is the check that keeps the hand-embedded Unicode-16 deltas honest: Go
// 1.23 ships Unicode 15.0 while the reference runs Unicode 16.0.0, so a table
// that drifts shows up as a specific code point rather than as a mysterious
// rendering difference much later.
func TestTelegramUnicodeTablesMatchPythonReference(t *testing.T) {
	ref := loadTelegramReference(t)
	t.Logf("reference unicodedata %s (Go ships %s)", ref.Unicode.UnidataVersion, goUnicodeVersion())

	checks := []struct {
		name   string
		ranges [][]int
		pred   func(rune) bool
	}{
		{"re \\w", ref.Unicode.ReUnicodeWord, telegram.PyIsWordRune},
		{"str.isdigit", ref.Unicode.StrIsDigit, telegram.PyIsDigitRune},
		{"str.isspace", ref.Unicode.StrIsSpace, textutil.PyIsSpace},
		{"east_asian_width W/F", ref.Unicode.EAWWide, telegram.IsEastAsianWide},
		// Python's `\\s` in a str pattern and str.isspace() are the same
		// predicate, and the two reference tables must agree with each other
		// before either can be used to judge the Go side.
		{"re \\s", ref.Unicode.ReUnicodeSpace, textutil.PyIsSpace},
	}
	for _, c := range checks {
		if got := walkUnicode(c.ranges, c.pred); got != "" {
			t.Fatalf("unicode %s: %s", c.name, got)
		}
	}
}

// walkUnicode compares one predicate against a sorted range table over the whole
// code space, returning a description of the first mismatch.
func walkUnicode(ranges [][]int, pred func(rune) bool) string {
	ri := 0
	for cp := 0; cp <= 0x10FFFF; cp++ {
		for ri < len(ranges) && cp > ranges[ri][1] {
			ri++
		}
		want := ri < len(ranges) && cp >= ranges[ri][0]
		if pred(rune(cp)) != want {
			return fmt.Sprintf("U+%04X: Go=%v reference=%v", cp, !want, want)
		}
	}
	return ""
}

// goUnicodeVersion reports the Unicode version Go's tables were built from, so
// a table drift caused by a toolchain upgrade is visible in the log.
func goUnicodeVersion() string { return unicodeVersion }

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// diffJSONMaps renders a readable difference between two decoded JSON maps,
// with the key path so a nested mismatch is identifiable.
func diffJSONMaps(label string, got, want map[string]any) string {
	var diffs []string
	collectDiffs("", got, want, &diffs)
	if len(diffs) == 0 {
		return ""
	}
	sort.Strings(diffs)
	return fmt.Sprintf("%s: %d difference(s):\n  %s", label, len(diffs), strings.Join(diffs, "\n  "))
}

func collectDiffs(path string, got, want any, out *[]string) {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: Go=%T(%v) reference=map", path, got, got))
			return
		}
		for key, wv := range w {
			gv, present := g[key]
			if !present {
				*out = append(*out, fmt.Sprintf("%s.%s: MISSING in Go (reference=%v)", path, key, wv))
				continue
			}
			collectDiffs(path+"."+key, gv, wv, out)
		}
		for key := range g {
			if _, present := w[key]; !present {
				*out = append(*out, fmt.Sprintf("%s.%s: EXTRA in Go (%v)", path, key, g[key]))
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: Go=%T(%v) reference=list", path, got, got))
			return
		}
		if len(g) != len(w) {
			*out = append(*out, fmt.Sprintf("%s: Go has %d items, reference has %d", path, len(g), len(w)))
		}
		for i := 0; i < len(g) && i < len(w); i++ {
			collectDiffs(fmt.Sprintf("%s[%d]", path, i), g[i], w[i], out)
		}
	default:
		if !scalarEqual(got, want) {
			*out = append(*out, fmt.Sprintf("%s: Go=%#v reference=%#v", path, got, want))
		}
	}
}

// scalarEqual compares two decoded JSON scalars across the int/float/json.Number
// representations.
func scalarEqual(got, want any) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	// JSON cannot represent a non-finite float, so the dumper writes the
	// strings "inf", "-inf" and "nan" in their place. Reverse that mapping
	// before the plain string comparison, or a float("inf") input looks like a
	// type mismatch.
	if ws, ok := want.(string); ok {
		if gf, ok := got.(float64); ok {
			switch ws {
			case "inf":
				return math.IsInf(gf, 1)
			case "-inf":
				return math.IsInf(gf, -1)
			case "nan":
				return math.IsNaN(gf)
			}
		}
	}
	if gs, ok := got.(string); ok {
		ws, ok := want.(string)
		return ok && gs == ws
	}
	if gb, ok := got.(bool); ok {
		wb, ok := want.(bool)
		return ok && gb == wb
	}
	// Numeric comparison is by VALUE, not by literal: the reference's
	// model_dump writes a float default as 5.0 and json.Number preserves that
	// text, while the Go field is a float64 that formats as 5.
	gn, gok := numericString(got)
	wn, wok := numericString(want)
	if gok && wok {
		if gn == wn {
			return true
		}
		gf, err1 := strconv.ParseFloat(gn, 64)
		wf, err2 := strconv.ParseFloat(wn, 64)
		return err1 == nil && err2 == nil && gf == wf
	}
	return fmt.Sprintf("%v", got) == fmt.Sprintf("%v", want)
}

func numericString(v any) (string, bool) {
	switch t := v.(type) {
	case json.Number:
		return t.String(), true
	case int:
		return strconv.Itoa(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64), true
	}
	return "", false
}
