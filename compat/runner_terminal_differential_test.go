// Terminal-response differential tests.
//
// These are the Go half of compat/python/dump_runner_terminal.py: the dumper
// EXECUTES the real Python AgentRunner.run (upstream/nanobot @ 1bb712d3)
// against a scripted stub provider, and this file drives the Go port through
// the same scripts and compares the results field by field.
//
// The subject is the terminal section of AgentRunner._run_core
// (runner.py:675-789) and the branch the port was missing:
//
//	if is_blank_text(clean):
//	    final_content = EMPTY_FINAL_RESPONSE_MESSAGE
//	    stop_reason = "empty_final_response"
//	    error = final_content
//	    self._append_final_message(messages, final_content)      # runner.py:737-754
//
// The PERSISTED TRANSCRIPT is compared, not just the returned content, because
// that is where the difference between `_append_final_message(messages, notice)`
// and "append the blank assistant response" shows up: both produce a run whose
// final content can look plausible while the transcript the next turn replays
// to the provider is wrong.
//
// Nothing here asserts behaviour read off the source by hand: every expected
// value comes from running the reference. The reference venv lives at
// .tools/venv. When it is absent the tests SKIP rather than fail, so a checkout
// without it still builds and tests cleanly — but they never silently pass: the
// skip is reported, and every test also asserts that a non-zero number of cases
// was actually compared.
package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// ---------------------------------------------------------------------------
// Reference loading
// ---------------------------------------------------------------------------

// runnerTerminalRepoRoot resolves the module root from this file's location.
// It is deliberately a separate helper from repoRoot in differential_test.go and
// runnerCleanRepoRoot in runner_clean_differential_test.go so that these tests
// keep working while those files are edited by other work.
func runnerTerminalRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller path")
	}
	return filepath.Dir(filepath.Dir(file))
}

// runnerTerminalCall is one tool call as the reference persists it, flattened
// from the OpenAI wire shape.
type runnerTerminalCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// runnerTerminalMessage is the canonical projection of one transcript entry.
//
// Both sides are projected onto this fixed key set, which is why the comparison
// is strict rather than approximate: a key the reference writes that this struct
// does not model is reported as an error by canonPyMessage instead of being
// silently dropped. The one thing it cannot represent is the reference's
// `reasoning_content: ""` (written by build_assistant_message when
// thinking_blocks is set but reasoning_content is None, helpers.py:708-713);
// no case in this corpus carries thinking_blocks, and core.Message cannot
// express an explicitly-empty modelled field, so that combination is out of
// scope here rather than silently equated.
type runnerTerminalMessage struct {
	Role       string               `json:"role"`
	Content    string               `json:"content"`
	ToolCalls  []runnerTerminalCall `json:"tool_calls,omitempty"`
	ToolCallID string               `json:"tool_call_id,omitempty"`
	Name       string               `json:"name,omitempty"`
	Reasoning  string               `json:"reasoning_content,omitempty"`
}

type runnerTerminalCase struct {
	Label         string            `json:"label"`
	Script        []json.RawMessage `json:"script"`
	UseTools      bool              `json:"use_tools"`
	MaxIterations int               `json:"max_iterations"`
	FinalContent  *string           `json:"final_content"`
	StopReason    *string           `json:"stop_reason"`
	Error         *string           `json:"error"`
	Messages      []map[string]any  `json:"messages"`
	ToolsUsed     []string          `json:"tools_used"`
	ProviderCall  int               `json:"provider_calls"`
	UnportedWhy   string            `json:"unported_reason"`
}

type runnerTerminalAppendFinal struct {
	Label       string           `json:"label"`
	MessagesIn  []map[string]any `json:"messages_in"`
	Content     *string          `json:"content"`
	MessagesOut []map[string]any `json:"messages_out"`
}

type runnerTerminalAppendError struct {
	Label       string           `json:"label"`
	MessagesIn  []map[string]any `json:"messages_in"`
	MessagesOut []map[string]any `json:"messages_out"`
}

type runnerTerminalReference struct {
	UpstreamCommit   string                      `json:"upstream_commit"`
	EmptyFinal       string                      `json:"empty_final_response_message"`
	ErrorPlaceholder string                      `json:"persisted_model_error_placeholder"`
	DefaultError     string                      `json:"default_error_message"`
	Cases            []runnerTerminalCase        `json:"cases"`
	Unported         []runnerTerminalCase        `json:"unported_cases"`
	AppendFinal      []runnerTerminalAppendFinal `json:"append_final_message"`
	AppendError      []runnerTerminalAppendError `json:"append_model_error_placeholder"`
}

// loadRunnerTerminalReference executes the dumper and decodes its JSON document.
func loadRunnerTerminalReference(t *testing.T) *runnerTerminalReference {
	t.Helper()
	root := runnerTerminalRepoRoot(t)
	python := filepath.Join(root, ".tools", "venv", "bin", "python")
	script := filepath.Join(root, "compat", "python", "dump_runner_terminal.py")

	if _, err := os.Stat(python); err != nil {
		t.Skipf("SKIP: reference venv not present at %s — differential check not run", python)
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("SKIP: dumper missing at %s", script)
	}

	out, err := runReferenceCommand("terminal dumper", []string{python, script}, root, nil)
	if err != nil {
		t.Fatalf("terminal dumper failed: %v", err)
	}

	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	var ref runnerTerminalReference
	if err := dec.Decode(&ref); err != nil {
		t.Fatalf("parse terminal reference output: %v", err)
	}
	if ref.UpstreamCommit != "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9" {
		t.Fatalf("dumper reported unexpected upstream commit %q", ref.UpstreamCommit)
	}
	return &ref
}

// runnerTerminalStr collapses the reference's None to "", the same collapse the
// Go string type makes unavoidable.
func runnerTerminalStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---------------------------------------------------------------------------
// Reference transcript projection
// ---------------------------------------------------------------------------

// runnerTerminalKnownKeys are the message keys this harness models. Anything
// else in a reference transcript is a modelling gap, and canonPyMessage fails
// on it rather than ignoring it.
var runnerTerminalKnownKeys = map[string]bool{
	"role": true, "content": true, "tool_calls": true,
	"tool_call_id": true, "name": true, "reasoning_content": true,
}

// canonPyMessage projects one reference transcript entry onto
// runnerTerminalMessage.
func canonPyMessage(t *testing.T, label string, idx int, m map[string]any) runnerTerminalMessage {
	t.Helper()
	for k := range m {
		if !runnerTerminalKnownKeys[k] {
			t.Errorf("%s: reference message %d has unmodelled key %q: %v",
				label, idx, k, m)
		}
	}
	var out runnerTerminalMessage
	out.Role, _ = m["role"].(string)
	switch content := m["content"].(type) {
	case nil:
		// The reference never writes None here (build_assistant_message uses
		// `content or ""`), so treat it as the empty string and let the
		// comparison catch it if that ever changes.
		out.Content = ""
	case string:
		out.Content = content
	default:
		t.Errorf("%s: reference message %d has non-string content %T: %v",
			label, idx, m["content"], m["content"])
	}
	out.ToolCallID, _ = m["tool_call_id"].(string)
	out.Name, _ = m["name"].(string)
	if rc, ok := m["reasoning_content"]; ok && rc != nil {
		s, _ := rc.(string)
		out.Reasoning = s
	}
	if raw, ok := m["tool_calls"]; ok && raw != nil {
		list, ok := raw.([]any)
		if !ok {
			t.Errorf("%s: reference message %d tool_calls is %T, want list", label, idx, raw)
			return out
		}
		for _, item := range list {
			call, ok := item.(map[string]any)
			if !ok {
				t.Errorf("%s: reference message %d tool call is %T, want object", label, idx, item)
				continue
			}
			fn, _ := call["function"].(map[string]any)
			converted := runnerTerminalCall{}
			converted.ID, _ = call["id"].(string)
			if fn != nil {
				converted.Name, _ = fn["name"].(string)
				converted.Arguments, _ = fn["arguments"].(string)
			}
			out.ToolCalls = append(out.ToolCalls, converted)
		}
	}
	return out
}

func canonPyMessages(t *testing.T, label string, msgs []map[string]any) []runnerTerminalMessage {
	t.Helper()
	out := make([]runnerTerminalMessage, 0, len(msgs))
	for i, m := range msgs {
		out = append(out, canonPyMessage(t, label, i, m))
	}
	return out
}

// canonGoMessage projects one Go transcript entry onto runnerTerminalMessage.
//
// Tool calls are flattened exactly as the reference's are, which is legitimate
// because the port's transcript keeps them flat by design and converts them to
// the OpenAI wire shape at the provider boundary
// (internal/provider/openai/request.go:505-509, normalizeToolCalls) — the same
// {"id","type","function":{"name","arguments"}} payload the reference builds in
// ToolCallRequest.to_openai_tool_call (base.py:86-107).
func canonGoMessage(t *testing.T, label string, idx int, m core.Message) runnerTerminalMessage {
	t.Helper()
	out := runnerTerminalMessage{
		Role:       string(m.Role),
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
		Reasoning:  m.ReasoningContent,
	}
	switch {
	case m.Content.IsText():
		out.Content = m.Content.Text
	case m.Content.IsZero():
		out.Content = ""
	default:
		raw, err := json.Marshal(m.Content)
		if err != nil {
			t.Fatalf("%s: marshal Go message %d content: %v", label, idx, err)
		}
		out.Content = string(raw)
	}
	for _, c := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, runnerTerminalCall{
			ID:        c.ID,
			Name:      c.Name,
			Arguments: c.ArgumentsString(),
		})
	}
	return out
}

func canonGoMessages(t *testing.T, label string, msgs []core.Message) []runnerTerminalMessage {
	t.Helper()
	out := make([]runnerTerminalMessage, 0, len(msgs))
	for i, m := range msgs {
		out = append(out, canonGoMessage(t, label, i, m))
	}
	return out
}

func runnerTerminalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<marshal error: %v>", err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Test doubles matching the dumper's script
// ---------------------------------------------------------------------------

// terminalStubProvider replays a scripted response per call.
type terminalStubProvider struct {
	script   []json.RawMessage
	calls    int
	requests []provider.ChatRequest
}

func (p *terminalStubProvider) Name() string { return "terminal-stub" }

func (p *terminalStubProvider) Chat(_ context.Context, req provider.ChatRequest) (*core.Response, error) {
	p.requests = append(p.requests, req)
	if p.calls >= len(p.script) {
		// The dumper's stub answers "script-exhausted" here. The same marker is
		// used so that a port which spends MORE model calls than the reference
		// fails on content rather than accidentally agreeing.
		return &core.Response{Content: "script-exhausted", FinishReason: core.FinishStop}, nil
	}
	raw := p.script[p.calls]
	p.calls++

	var item struct {
		Content          string `json:"content"`
		FinishReason     string `json:"finish_reason"`
		ReasoningContent string `json:"reasoning_content"`
		ToolCalls        []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, fmt.Errorf("terminal stub: bad script entry: %w", err)
	}
	resp := &core.Response{
		Content:          item.Content,
		FinishReason:     core.FinishReason(item.FinishReason),
		ReasoningContent: item.ReasoningContent,
	}
	if resp.FinishReason == "" {
		resp.FinishReason = core.FinishStop
	}
	for _, tc := range item.ToolCalls {
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		resp.ToolCalls = append(resp.ToolCalls, core.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: json.RawMessage(args),
		})
	}
	return resp, nil
}

// terminalEchoTool mirrors the dumper's EchoTool: it returns "echo:<v>".
type terminalEchoTool struct{ tools.ReadOnlyBase }

func (terminalEchoTool) Name() string { return "echo" }
func (terminalEchoTool) Description() string {
	return "Echo the value back."
}
func (terminalEchoTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"v":{"type":"string"}},"required":["v"]}`)
}

func (terminalEchoTool) Execute(_ context.Context, args json.RawMessage) (tools.Result, error) {
	var parsed struct {
		V string `json:"v"`
	}
	if err := json.Unmarshal(args, &parsed); err != nil {
		return tools.Result{}, fmt.Errorf("echo: bad arguments: %w", err)
	}
	return tools.Result{Content: "echo:" + parsed.V}, nil
}

// ---------------------------------------------------------------------------
// The run corpus
// ---------------------------------------------------------------------------

// TestRunnerTerminalCasesMatchesPython drives the Go port and the frozen
// reference through the same response scripts and compares, per case:
//
//   - final_content, stop_reason and error;
//   - the PERSISTED TRANSCRIPT, which is where the blank branch's
//     _append_final_message differs from appending the blank response;
//   - tools_used and the number of provider calls.
//
// The corpus covers the blankness flavours the two runtimes disagree about
// ("" / ASCII whitespace / U+001C..U+001F / NBSP / U+3000), blankness that only
// appears after strip_think, the finish reasons that must NOT reach the blank
// branch (length, error, refusal, content_filter), blank terminals after real
// tool work, retry recovery, and non-blank controls whose raw content differs
// from the cleaned content that must be persisted.
func TestRunnerTerminalCasesMatchesPython(t *testing.T) {
	ref := loadRunnerTerminalReference(t)

	if ref.EmptyFinal == "" {
		t.Fatal("FAIL: the dumper reported an empty EMPTY_FINAL_RESPONSE_MESSAGE")
	}
	if ref.EmptyFinal != agent.EmptyFinalResponseMessage {
		t.Errorf("EMPTY_FINAL_RESPONSE_MESSAGE: Go %q, reference %q",
			agent.EmptyFinalResponseMessage, ref.EmptyFinal)
	}
	if ref.ErrorPlaceholder != agent.PersistedModelErrorPlaceholder {
		t.Errorf("_PERSISTED_MODEL_ERROR_PLACEHOLDER: Go %q, reference %q",
			agent.PersistedModelErrorPlaceholder, ref.ErrorPlaceholder)
	}
	if ref.DefaultError != agent.DefaultError {
		t.Errorf("_DEFAULT_ERROR_MESSAGE: Go %q, reference %q",
			agent.DefaultError, ref.DefaultError)
	}

	compared := 0
	blankBranch := 0
	errorBranch := 0
	lengthBranch := 0
	toolTranscripts := 0
	// repeatDivergences counts cases where the REFERENCE's tools_used contains a
	// repeated entry. It is the documented divergence the set comparison above
	// deliberately tolerates, so it is reported rather than asserted: if it ever
	// drops to zero the tolerance is no longer exercised by this corpus.
	repeatDivergences := 0
	stopReasons := map[string]bool{}

	for _, tc := range ref.Cases {
		tc := tc
		t.Run(tc.Label, func(t *testing.T) {
			stub := &terminalStubProvider{script: tc.Script}
			reg := tools.NewRegistry()
			if tc.UseTools {
				reg.Register(terminalEchoTool{})
			}

			res, err := agent.NewRunner().Run(context.Background(), agent.RunSpec{
				Messages:      []core.Message{*core.NewMessage(core.RoleUser, "hi")},
				Provider:      stub,
				Tools:         reg,
				MaxIterations: tc.MaxIterations,
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			if got, want := res.FinalContent, runnerTerminalStr(tc.FinalContent); got != want {
				t.Errorf("final_content = %q, reference %q", got, want)
			}
			if got, want := string(res.StopReason), runnerTerminalStr(tc.StopReason); got != want {
				t.Errorf("stop_reason = %q, reference %q", got, want)
			}
			if got, want := res.Error, runnerTerminalStr(tc.Error); got != want {
				t.Errorf("error = %q, reference %q", got, want)
			}
			if got, want := stub.calls, tc.ProviderCall; got != want {
				t.Errorf("provider calls = %d, reference %d "+
					"(the scripts are sized so a differing call count changes the "+
					"response the model is deemed to have given)", got, want)
			}
			// tools_used is compared as a SET, because the port and the
			// reference disagree on it in two documented ways that are NOT
			// part of this harness's subject: the reference keeps one entry per
			// successful tool event (so a repeated call appears twice —
			// runner.py:524-528) while the port dedupes, and the reference
			// filters on the tool event's status while the port records every
			// executed call. Both are reported by the caller; the set
			// comparison still catches a tool that ran on one side only.
			if got, want := uniqueSorted(res.ToolsUsed), uniqueSorted(tc.ToolsUsed); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("tools_used set = %v, reference set %v", got, want)
			}
			if len(tc.ToolsUsed) != len(uniqueSorted(tc.ToolsUsed)) {
				repeatDivergences++
			}

			gotMsgs := canonGoMessages(t, tc.Label, res.Messages)
			wantMsgs := canonPyMessages(t, tc.Label, tc.Messages)
			if runnerTerminalJSON(gotMsgs) != runnerTerminalJSON(wantMsgs) {
				t.Errorf("persisted transcript differs\n  go        : %s\n  reference : %s",
					runnerTerminalJSON(gotMsgs), runnerTerminalJSON(wantMsgs))
			}

			// Coverage counters, asserted non-zero by the caller. Without them
			// a corpus that quietly stopped exercising the branch under test
			// would still pass.
			switch runnerTerminalStr(tc.StopReason) {
			case string(agent.StopEmptyFinalResponse):
				blankBranch++
			case string(agent.StopError):
				errorBranch++
			}
			for _, m := range tc.Messages {
				if m["role"] == "tool" {
					toolTranscripts++
					break
				}
			}
			for _, sc := range tc.Script {
				if bytes.Contains(sc, []byte(`"length"`)) {
					lengthBranch++
					break
				}
			}
			stopReasons[runnerTerminalStr(tc.StopReason)] = true
			compared++
		})
	}

	if compared == 0 {
		t.Fatal("FAIL: zero terminal cases compared")
	}
	if blankBranch == 0 {
		t.Fatal("FAIL: no case reached the empty_final_response branch — the branch " +
			"this harness exists for is untested")
	}
	if errorBranch == 0 {
		t.Fatal("FAIL: no case exercised the error branch")
	}
	if lengthBranch == 0 {
		t.Fatal("FAIL: no case exercised a blank 'length' response")
	}
	if toolTranscripts == 0 {
		t.Fatal("FAIL: no case produced a tool result in the transcript")
	}
	// The reference's complete stop-reason vocabulary, from runner.py: the
	// terminal paths this corpus reaches must only ever produce these.
	allowed := map[string]bool{
		"cancelled": true, "completed": true,
		"empty_final_response": true, "error": true, "max_iterations": true,
	}
	var seen []string
	for reason := range stopReasons {
		seen = append(seen, reason)
		if !allowed[reason] {
			t.Errorf("case corpus produced stop reason %q, which is not in the "+
				"reference's vocabulary {cancelled, completed, empty_final_response, "+
				"error, max_iterations}", reason)
		}
	}
	sort.Strings(seen)
	t.Logf("terminal cases: %d compared (%d empty_final_response, %d error, "+
		"%d with a blank length response, %d with a tool result in the transcript, "+
		"%d where the reference repeated a tools_used entry); "+
		"stop reasons seen: %v", compared, blankBranch, errorBranch, lengthBranch,
		toolTranscripts, repeatDivergences, seen)
}

// uniqueSorted returns the distinct elements of in, sorted.
//
// It mirrors Python's sorted(set(in)), which is how tools_used is compared here:
// the port dedupes the list while the reference keeps one entry per successful
// tool event, so only the SET is comparable. The input is not modified.
func uniqueSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// TestRunnerTerminalUnportedCasesDocumented records the reference behaviour of
// the cases this port cannot be compared against, because the reference spends
// a request on a feature the port does not model. They are not asserted as
// equal — they are asserted to EXIST in the dump, so the divergence stays
// visible and reproducible instead of silently dropping out of the corpus.
func TestRunnerTerminalUnportedCasesDocumented(t *testing.T) {
	ref := loadRunnerTerminalReference(t)

	if len(ref.Unported) == 0 {
		t.Fatal("FAIL: the dumper reported no unported cases; the documented " +
			"divergences have disappeared from the corpus")
	}
	for _, tc := range ref.Unported {
		if tc.UnportedWhy == "" {
			t.Errorf("%s: unported case carries no explanation", tc.Label)
		}
		t.Logf("UNPORTED %s: reference final=%q stop=%q calls=%d — %s",
			tc.Label, runnerTerminalStr(tc.FinalContent),
			runnerTerminalStr(tc.StopReason), tc.ProviderCall, tc.UnportedWhy)
	}
}

// ---------------------------------------------------------------------------
// _append_final_message / _append_model_error_placeholder
// ---------------------------------------------------------------------------

// TestRunnerTerminalAppendFinalMessageMatchesPython compares
// agent.AppendFinalMessage against AgentRunner._append_final_message
// (runner.py:1356) over the dumper's corpus.
//
// This is the function the blank branch writes the transcript with, and its two
// non-trivial outcomes — replace the trailing assistant turn, or leave it
// untouched — are not reachable through a run, so they are compared directly.
func TestRunnerTerminalAppendFinalMessageMatchesPython(t *testing.T) {
	ref := loadRunnerTerminalReference(t)

	compared := 0
	appended := 0
	replaced := 0
	unchanged := 0
	for _, tc := range ref.AppendFinal {
		content := runnerTerminalStr(tc.Content)
		messages := make([]core.Message, 0, len(tc.MessagesIn))
		for i, m := range tc.MessagesIn {
			messages = append(messages, goMessageFromReference(t, tc.Label, i, m))
		}
		got := agent.AppendFinalMessage(messages, content)
		want := canonPyMessages(t, tc.Label, tc.MessagesOut)
		gotCanon := canonGoMessages(t, tc.Label, got)
		if runnerTerminalJSON(gotCanon) != runnerTerminalJSON(want) {
			t.Errorf("%s: AppendFinalMessage(%s, %q) = %s, reference %s",
				tc.Label, runnerTerminalJSON(canonPyMessages(t, tc.Label, tc.MessagesIn)),
				content, runnerTerminalJSON(gotCanon), runnerTerminalJSON(want))
		}
		// Classify the outcome so the corpus cannot quietly stop covering one
		// of the three behaviours while still passing. A replacement keeps the
		// transcript length, so the trailing entry is what distinguishes it
		// from a no-op.
		inCanon := canonPyMessages(t, tc.Label, tc.MessagesIn)
		switch {
		case len(want) == len(inCanon)+1:
			appended++
		case len(want) == len(inCanon):
			if len(want) > 0 &&
				runnerTerminalJSON(want[len(want)-1]) != runnerTerminalJSON(inCanon[len(inCanon)-1]) {
				replaced++
			} else {
				unchanged++
			}
		default:
			t.Errorf("%s: the reference turned %d messages into %d — neither an "+
				"append nor a replacement", tc.Label, len(inCanon), len(want))
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("FAIL: zero _append_final_message cases compared")
	}
	if appended == 0 || unchanged == 0 || replaced == 0 {
		t.Fatalf("FAIL: the corpus does not cover all three outcomes "+
			"(append=%d, unchanged=%d, replace=%d) — a corpus that stopped "+
			"exercising the replace branch would pass vacuously",
			appended, unchanged, replaced)
	}
	t.Logf("_append_final_message: %d cases compared (%d append, %d unchanged, %d replace)",
		compared, appended, unchanged, replaced)
}

// TestRunnerTerminalAppendErrorPlaceholderMatchesPython compares
// agent.AppendModelErrorPlaceholder against
// AgentRunner._append_model_error_placeholder (runner.py:1371).
func TestRunnerTerminalAppendErrorPlaceholderMatchesPython(t *testing.T) {
	ref := loadRunnerTerminalReference(t)

	compared := 0
	appended := 0
	for _, tc := range ref.AppendError {
		messages := make([]core.Message, 0, len(tc.MessagesIn))
		for i, m := range tc.MessagesIn {
			messages = append(messages, goMessageFromReference(t, tc.Label, i, m))
		}
		got := agent.AppendModelErrorPlaceholder(messages)
		want := canonPyMessages(t, tc.Label, tc.MessagesOut)
		gotCanon := canonGoMessages(t, tc.Label, got)
		if runnerTerminalJSON(gotCanon) != runnerTerminalJSON(want) {
			t.Errorf("%s: AppendModelErrorPlaceholder = %s, reference %s",
				tc.Label, runnerTerminalJSON(gotCanon), runnerTerminalJSON(want))
		}
		if len(want) == len(tc.MessagesIn)+1 {
			appended++
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("FAIL: zero _append_model_error_placeholder cases compared")
	}
	if appended == 0 {
		t.Fatal("FAIL: no case actually appended the placeholder")
	}
	t.Logf("_append_model_error_placeholder: %d cases compared (%d appended)",
		compared, appended)
}

// goMessageFromReference builds a core.Message from one reference transcript
// entry, so the helpers above can be exercised on the same inputs.
func goMessageFromReference(t *testing.T, label string, idx int, m map[string]any) core.Message {
	t.Helper()
	canonical := canonPyMessage(t, label, idx, m)
	msg := core.Message{Role: core.Role(canonical.Role), Content: core.TextContent(canonical.Content)}
	msg.ToolCallID = canonical.ToolCallID
	msg.Name = canonical.Name
	msg.ReasoningContent = canonical.Reasoning
	for _, c := range canonical.ToolCalls {
		args := c.Arguments
		if args == "" {
			args = "{}"
		}
		msg.ToolCalls = append(msg.ToolCalls, core.ToolCall{
			ID:        c.ID,
			Name:      c.Name,
			Arguments: json.RawMessage(args),
		})
	}
	return msg
}
