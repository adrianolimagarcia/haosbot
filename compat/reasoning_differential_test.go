// Reasoning-extraction differential tests.
//
// These are the Go half of compat/python/dump_reasoning.py: the dumper executes
// the REAL Python implementation — strip_reasoning_tags (utils/helpers.py:226),
// extract_think (helpers.py:240), extract_reasoning (helpers.py:292) — and, for
// the runner section, drives the REAL AgentRunner with a scripted provider and a
// recording hook so the emit gate of runner.py:477-480 is observed as the
// reference's own behaviour. This file drives the Go port through the same
// inputs and compares the results.
//
// Nothing here asserts behaviour read off the source by hand: every expected
// value comes from running the reference.
//
// The reference venv lives at .tools/venv. When it is absent the tests SKIP
// rather than fail, so a checkout without it still builds and tests cleanly —
// but they never silently pass: the skip is reported, and every test also
// asserts that a non-zero number of cases was actually compared.
package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// ---------------------------------------------------------------------------
// Reference loading
// ---------------------------------------------------------------------------

// reasoningRepoRoot resolves the module root from this file's location.
func reasoningRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller path")
	}
	return filepath.Dir(filepath.Dir(file))
}

type reasoningReference struct {
	UpstreamCommit string `json:"upstream_commit"`
	StripTags      []struct {
		Label            string `json:"label"`
		In               string `json:"in"`
		Result           string `json:"result"`
		ResultNonStrInt  string `json:"result_non_str_int"`
		ResultNonStrNone string `json:"result_non_str_none"`
	} `json:"strip_reasoning_tags"`
	ExtractThink []struct {
		Label    string  `json:"label"`
		In       string  `json:"in"`
		Thinking *string `json:"thinking"`
		Cleaned  string  `json:"cleaned"`
	} `json:"extract_think"`
	ExtractReasoning []struct {
		Label          string           `json:"label"`
		ReasoningInput *string          `json:"reasoning_content"`
		ThinkingBlocks []map[string]any `json:"thinking_blocks"`
		Content        *string          `json:"content"`
		ReasoningText  *string          `json:"reasoning_text"`
		CleanedContent *string          `json:"cleaned_content"`
	} `json:"extract_reasoning"`
	RunnerEmission []reasoningEmissionCase `json:"runner_emission"`
}

// reasoningEmissionCase is one scripted AgentRunner run, as the dumper recorded
// it. The scripted response travels WITH the expectations so the Go half cannot
// drift from the scenario table in the dumper.
type reasoningEmissionCase struct {
	Label          string   `json:"label"`
	Streaming      bool     `json:"streaming"`
	ThinkingDeltas []string `json:"thinking_deltas"`
	ContentDeltas  []string `json:"content_deltas"`
	Response       struct {
		Content          *string          `json:"content"`
		ReasoningContent *string          `json:"reasoning_content"`
		ThinkingBlocks   []map[string]any `json:"thinking_blocks"`
	} `json:"response"`
	Emitted           []string `json:"emitted"`
	EndCalls          int      `json:"end_calls"`
	FinalContent      string   `json:"final_content"`
	AssistantContents []string `json:"assistant_contents"`
	StopReason        string   `json:"stop_reason"`
}

// loadReasoningReference executes the dumper and decodes its JSON document.
func loadReasoningReference(t *testing.T) *reasoningReference {
	t.Helper()
	root := reasoningRepoRoot(t)
	python := filepath.Join(root, ".tools", "venv", "bin", "python")
	script := filepath.Join(root, "compat", "python", "dump_reasoning.py")

	if _, err := os.Stat(python); err != nil {
		t.Skipf("SKIP: reference venv not present at %s — differential check not run", python)
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("SKIP: dumper missing at %s", script)
	}

	out, err := runReferenceCommand("reasoning dumper", []string{python, script}, root, nil)
	if err != nil {
		t.Fatalf("reasoning dumper failed: %v", err)
	}

	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	var ref reasoningReference
	if err := dec.Decode(&ref); err != nil {
		t.Fatalf("parse reasoning reference output: %v", err)
	}
	if ref.UpstreamCommit != "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9" {
		t.Fatalf("dumper reported unexpected upstream commit %q", ref.UpstreamCommit)
	}
	return &ref
}

// reasoningStr collapses the reference's None to "" for display purposes only.
func reasoningStr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// ---------------------------------------------------------------------------
// strip_reasoning_tags
// ---------------------------------------------------------------------------

// TestReasoningStripTagsMatchesPython compares textutil.StripReasoningTags
// against the reference over an adversarial corpus.
//
// The corpus is where the two whitespace definitions can be told apart:
// Python's `\s` is str.isspace() and includes U+000B, U+001C..U+001F, U+0085,
// NBSP, U+1680, U+2000..U+200A, U+2028, U+2029, U+202F, U+205F and U+3000 —
// every one of which Go's regexp `\s` does NOT match. A port that writes `\s`
// in these patterns fails here rather than in production.
//
// It also pins the isinstance guard: a non-str argument strips to "".
func TestReasoningStripTagsMatchesPython(t *testing.T) {
	ref := loadReasoningReference(t)

	compared := 0
	for _, tc := range ref.StripTags {
		got := textutil.StripReasoningTags(tc.In)
		if got != tc.Result {
			t.Errorf("%s: StripReasoningTags(%q) = %q, reference = %q",
				tc.Label, tc.In, got, tc.Result)
		}
		// The reference's `if not isinstance(text, str): return ""` guard.
		if got := textutil.StripReasoningTags(123); got != tc.ResultNonStrInt {
			t.Errorf("%s: StripReasoningTags(123) = %q, reference = %q",
				tc.Label, got, tc.ResultNonStrInt)
		}
		if got := textutil.StripReasoningTags(nil); got != tc.ResultNonStrNone {
			t.Errorf("%s: StripReasoningTags(nil) = %q, reference = %q",
				tc.Label, got, tc.ResultNonStrNone)
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("FAIL: zero strip_reasoning_tags cases compared")
	}
	t.Logf("strip_reasoning_tags: %d cases compared", compared)
}

// ---------------------------------------------------------------------------
// extract_think
// ---------------------------------------------------------------------------

// TestReasoningExtractThinkMatchesPython compares textutil.ExtractThink against
// the reference over an adversarial corpus.
//
// Both halves of the result matter: the extracted thinking text (including the
// None-vs-"" distinction — no closed block is None, but ONE EMPTY block is "")
// and the cleaned text, which is StripThink's output.
func TestReasoningExtractThinkMatchesPython(t *testing.T) {
	ref := loadReasoningReference(t)

	compared := 0
	for _, tc := range ref.ExtractThink {
		gotThinking, gotCleaned := textutil.ExtractThink(tc.In)
		if reasoningStr(gotThinking) != reasoningStr(tc.Thinking) {
			t.Errorf("%s: ExtractThink(%q) thinking = %q, reference = %q",
				tc.Label, tc.In, reasoningStr(gotThinking), reasoningStr(tc.Thinking))
		}
		// A nil pointer must mean the reference's None, and "" must mean "".
		if (gotThinking == nil) != (tc.Thinking == nil) {
			t.Errorf("%s: ExtractThink(%q) thinking nil-ness = %v, reference None-ness = %v",
				tc.Label, tc.In, gotThinking == nil, tc.Thinking == nil)
		}
		if gotCleaned != tc.Cleaned {
			t.Errorf("%s: ExtractThink(%q) cleaned = %q, reference = %q",
				tc.Label, tc.In, gotCleaned, tc.Cleaned)
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("FAIL: zero extract_think cases compared")
	}
	t.Logf("extract_think: %d cases compared", compared)
}

// ---------------------------------------------------------------------------
// extract_reasoning
// ---------------------------------------------------------------------------

// TestReasoningExtractReasoningMatchesPython compares textutil.ExtractReasoning
// against the reference across the full cross-product of the three sources.
//
// This is the function runner.py:467 calls on every response, so its three-tier
// fallback and its truthiness collapses are what decide which reasoning a model
// turn surfaces:
//
//   - the tier tests are PYTHON TRUTHINESS, so reasoning_content="" falls
//     through to the next source while reasoning_content="   " selects this
//     tier and then strips to "";
//   - lower-priority sources are ignored when a higher-priority one is present,
//     but inline tags are always scrubbed from the content;
//   - the thinking_blocks tier collapses an empty join to None (`joined or
//     None`) while the inline tier can return "".
func TestReasoningExtractReasoningMatchesPython(t *testing.T) {
	ref := loadReasoningReference(t)

	compared := 0
	for _, tc := range ref.ExtractReasoning {
		gotReasoning, gotCleaned := textutil.ExtractReasoning(
			tc.ReasoningInput, tc.ThinkingBlocks, tc.Content)

		if reasoningStr(gotReasoning) != reasoningStr(tc.ReasoningText) {
			t.Errorf("%s: reasoning_text = %q, reference = %q",
				tc.Label, reasoningStr(gotReasoning), reasoningStr(tc.ReasoningText))
		}
		if (gotReasoning == nil) != (tc.ReasoningText == nil) {
			t.Errorf("%s: reasoning_text nil-ness = %v, reference None-ness = %v",
				tc.Label, gotReasoning == nil, tc.ReasoningText == nil)
		}
		if reasoningStr(gotCleaned) != reasoningStr(tc.CleanedContent) {
			t.Errorf("%s: cleaned_content = %q, reference = %q",
				tc.Label, reasoningStr(gotCleaned), reasoningStr(tc.CleanedContent))
		}
		if (gotCleaned == nil) != (tc.CleanedContent == nil) {
			t.Errorf("%s: cleaned_content nil-ness = %v, reference None-ness = %v",
				tc.Label, gotCleaned == nil, tc.CleanedContent == nil)
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("FAIL: zero extract_reasoning cases compared")
	}
	t.Logf("extract_reasoning: %d cases compared", compared)
}

// ---------------------------------------------------------------------------
// runner emission
// ---------------------------------------------------------------------------

// reasoningProvider is a scripted NON-streaming provider: it implements only
// provider.Provider, so the Go runner takes the non-streamed path — the path
// this change wires extract_reasoning into.
type reasoningProvider struct {
	resp *core.Response
}

func (p *reasoningProvider) Chat(context.Context, provider.ChatRequest) (*core.Response, error) {
	return p.resp, nil
}

func (p *reasoningProvider) Name() string { return "reasoning-scripted" }

// reasoningStreamProvider emits reasoning and text deltas before its final
// response, so the runner's streaming path sets the streamed-reasoning flag.
//
// The delta order mirrors the reference harness: thinking deltas first, then
// content deltas, then the final response.
type reasoningStreamProvider struct {
	resp      *core.Response
	reasoning []string
	text      []string
}

func (p *reasoningStreamProvider) Chat(context.Context, provider.ChatRequest) (*core.Response, error) {
	return p.resp, nil
}

func (p *reasoningStreamProvider) Name() string { return "reasoning-stream-scripted" }

func (p *reasoningStreamProvider) ChatStream(
	ctx context.Context, _ provider.ChatRequest,
) (<-chan core.StreamEvent, error) {
	ch := make(chan core.StreamEvent, len(p.reasoning)+len(p.text)+1)
	for _, d := range p.reasoning {
		ch <- core.StreamEvent{Kind: core.StreamReasoning, Text: d}
	}
	for _, d := range p.text {
		ch <- core.StreamEvent{Kind: core.StreamText, Text: d}
	}
	ch <- core.StreamEvent{Kind: core.StreamDone, Response: p.resp}
	close(ch)
	return ch, nil
}

// reasoningRecordingHook records what the runner emits, exactly as the
// reference's own _RecordingHook does: a falsy reasoning payload is not
// recorded, and every end call is counted.
type reasoningRecordingHook struct {
	agent.NopHook
	emitted  []string
	endCalls int
}

func (h *reasoningRecordingHook) OnReasoningDelta(_ context.Context, delta string) {
	if delta != "" {
		h.emitted = append(h.emitted, delta)
	}
}

func (h *reasoningRecordingHook) OnReasoningEnd(context.Context) { h.endCalls++ }

// TestReasoningRunnerEmissionMatchesPython drives the Go runner with the same
// scripted responses the dumper drives the real AgentRunner with, and compares
// what reached the hook.
//
// The subject is runner.py:477-480 — reasoning carried by a NON-streamed
// response must reach the hook (with an end marker), exactly once, and must be
// suppressed when the same reasoning already went out as streamed deltas.
//
// The final content is compared only for scenarios the reference reports as
// `completed`. A blank response instead exhausts the iteration budget and is
// finalized by a path that never calls extract_reasoning (runner.py:1207-1215),
// so its final content is produced by machinery outside this harness's subject.
func TestReasoningRunnerEmissionMatchesPython(t *testing.T) {
	ref := loadReasoningReference(t)

	compared := 0
	completedCompared := 0
	for _, tc := range ref.RunnerEmission {
		resp := &core.Response{
			Content:          derefOrEmpty(tc.Response.Content),
			FinishReason:     core.FinishStop,
			ReasoningContent: derefOrEmpty(tc.Response.ReasoningContent),
			ThinkingBlocks:   tc.Response.ThinkingBlocks,
		}

		var p provider.Provider = &reasoningProvider{resp: resp}
		if tc.Streaming {
			p = &reasoningStreamProvider{
				resp:      resp,
				reasoning: tc.ThinkingDeltas,
				text:      tc.ContentDeltas,
			}
		}

		hook := &reasoningRecordingHook{}
		result, err := agent.NewRunner().Run(context.Background(), agent.RunSpec{
			Messages:      []core.Message{*core.NewMessage(core.RoleUser, "question")},
			Provider:      p,
			Model:         "test-model",
			MaxIterations: 1,
			Hook:          hook,
		})
		if err != nil {
			t.Fatalf("%s: Run returned error: %v", tc.Label, err)
		}

		if !reasoningEqualStrings(hook.emitted, tc.Emitted) {
			t.Errorf("%s: emitted %v, reference emitted %v",
				tc.Label, hook.emitted, tc.Emitted)
		}
		if hook.endCalls != tc.EndCalls {
			t.Errorf("%s: end_calls = %d, reference = %d",
				tc.Label, hook.endCalls, tc.EndCalls)
		}

		if tc.StopReason == string(agent.StopCompleted) {
			if result.FinalContent != tc.FinalContent {
				t.Errorf("%s: final_content = %q, reference = %q",
					tc.Label, result.FinalContent, tc.FinalContent)
			}
			if got := assistantContents(result); !reasoningEqualStrings(got, tc.AssistantContents) {
				t.Errorf("%s: assistant contents %v, reference %v",
					tc.Label, got, tc.AssistantContents)
			}
			completedCompared++
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("FAIL: zero runner emission cases compared")
	}
	if completedCompared == 0 {
		t.Fatal("FAIL: no runner scenario reached the completed path")
	}
	t.Logf("runner_emission: %d cases compared (%d completed-path)",
		compared, completedCompared)
}

// assistantContents returns the content of every assistant message, matching
// the dumper's extraction from result.messages.
func assistantContents(result *agent.RunResult) []string {
	var out []string
	for _, m := range result.Messages {
		if m.Role == core.RoleAssistant {
			out = append(out, m.Content.Text)
		}
	}
	return out
}

func reasoningEqualStrings(a, b []string) bool {
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

func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
