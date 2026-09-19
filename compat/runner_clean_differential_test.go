// Runner-clean differential tests.
//
// These are the Go half of compat/python/dump_runner_clean.py: the dumper
// executes the REAL Python implementation of nanobot's `clean` computation —
// AgentProgressHook.finalize_content (progress_hook.py:180), strip_think
// (utils/helpers.py:168), is_blank_text (utils/runtime.py:63),
// AgentRunner._restore_outer_whitespace (runner.py:77), the empty-content retry
// predicate (runner.py:588-593) and
// LLMProvider._error_response_from_exception (base.py:950) — and this file
// drives the Go port through the same inputs and compares the results.
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
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// ---------------------------------------------------------------------------
// Reference loading
// ---------------------------------------------------------------------------

// runnerCleanRepoRoot resolves the module root from this file's location.
//
// It is a separate helper from repoRoot in differential_test.go on purpose:
// these tests must keep working while that file is edited by other work.
func runnerCleanRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller path")
	}
	return filepath.Dir(filepath.Dir(file))
}

type runnerCleanReference struct {
	UpstreamCommit string `json:"upstream_commit"`
	StripThink     []struct {
		Label           string  `json:"label"`
		In              string  `json:"in"`
		StripThink      string  `json:"strip_think"`
		Clean           *string `json:"clean"`
		CleanIdempotent *string `json:"clean_idempotent"`
	} `json:"strip_think"`
	IsBlankText []struct {
		Label  string  `json:"label"`
		In     *string `json:"in"`
		Result bool    `json:"result"`
	} `json:"is_blank_text"`
	RestoreOuterWhitespace []struct {
		Content  string  `json:"content"`
		Original *string `json:"original"`
		Result   string  `json:"result"`
	} `json:"restore_outer_whitespace"`
	EmptyRetryTruthTable []struct {
		FinishReason       string  `json:"finish_reason"`
		ContentKind        string  `json:"content_kind"`
		Content            *string `json:"content"`
		WithTools          bool    `json:"with_tools"`
		HasToolCalls       bool    `json:"has_tool_calls"`
		ShouldExecuteTools bool    `json:"should_execute_tools"`
		Clean              *string `json:"clean"`
		RetryCheckReached  bool    `json:"retry_check_reached"`
		RetryEligible      bool    `json:"retry_eligible"`
	} `json:"empty_retry_truth_table"`
	ErrorDetail []struct {
		Label        string `json:"label"`
		ClassName    string `json:"class_name"`
		StrValue     string `json:"str_value"`
		StrStripped  string `json:"str_stripped"`
		Detail       string `json:"detail"`
		FinishReason string `json:"finish_reason"`
	} `json:"error_detail"`
}

// loadRunnerCleanReference executes the dumper and decodes its JSON document.
func loadRunnerCleanReference(t *testing.T) *runnerCleanReference {
	t.Helper()
	root := runnerCleanRepoRoot(t)
	python := filepath.Join(root, ".tools", "venv", "bin", "python")
	script := filepath.Join(root, "compat", "python", "dump_runner_clean.py")

	if _, err := os.Stat(python); err != nil {
		t.Skipf("SKIP: reference venv not present at %s — differential check not run", python)
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("SKIP: dumper missing at %s", script)
	}

	out, err := runReferenceCommand("runner-clean dumper", []string{python, script}, root, nil)
	if err != nil {
		t.Fatalf("runner-clean dumper failed: %v", err)
	}

	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	var ref runnerCleanReference
	if err := dec.Decode(&ref); err != nil {
		t.Fatalf("parse runner-clean reference output: %v", err)
	}
	if ref.UpstreamCommit != "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9" {
		t.Fatalf("dumper reported unexpected upstream commit %q", ref.UpstreamCommit)
	}
	return &ref
}

// runnerCleanStr collapses the reference's None to "", which is exactly the
// collapse AgentProgressHook._strip_think's `or None` performs and which the Go
// string type makes unavoidable.
func runnerCleanStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---------------------------------------------------------------------------
// strip_think / AgentProgressHook._strip_think
// ---------------------------------------------------------------------------

// TestRunnerCleanStripThinkMatchesPython compares textutil.StripThink — the
// function the Go `clean` is built from — against BOTH the module-level
// strip_think and the hook's static _strip_think, over an adversarial corpus.
//
// The corpus is where the two whitespace definitions can be told apart:
// U+001C..U+001F are Python whitespace and not Go's, and they appear at the
// boundaries of inputs that also contain thinking tags. NBSP and the other
// Unicode spaces are whitespace for both runtimes and serve as controls.
func TestRunnerCleanStripThinkMatchesPython(t *testing.T) {
	ref := loadRunnerCleanReference(t)

	compared := 0
	for _, tc := range ref.StripThink {
		got := textutil.StripThink(tc.In)
		if got != tc.StripThink {
			t.Errorf("%s: StripThink(%q) = %q, reference strip_think = %q",
				tc.Label, tc.In, got, tc.StripThink)
		}
		// `clean` is `strip_think(text) or None`: None for an empty result.
		if got != runnerCleanStr(tc.Clean) {
			t.Errorf("%s: clean(%q) = %q, reference _strip_think = %q",
				tc.Label, tc.In, got, runnerCleanStr(tc.Clean))
		}
		// The reference applies the hook once per response, but the value also
		// flows through history persistence, so idempotence matters.
		if twice := textutil.StripThink(got); twice != runnerCleanStr(tc.CleanIdempotent) {
			t.Errorf("%s: clean(clean(%q)) = %q, reference = %q",
				tc.Label, tc.In, twice, runnerCleanStr(tc.CleanIdempotent))
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("FAIL: zero strip_think cases compared")
	}
	t.Logf("strip_think: %d cases compared", compared)
}

// ---------------------------------------------------------------------------
// is_blank_text
// ---------------------------------------------------------------------------

// TestRunnerCleanBlankTextMatchesPython compares agent.IsBlankText against
// utils.runtime.is_blank_text over the same corpus.
//
// A disagreement here is always the whitespace DEFINITION rather than the
// predicate, so the test also counts how many corpus entries separate Python's
// strip from Go's TrimSpace (U+001C..U+001F) and fails if that count is zero.
func TestRunnerCleanBlankTextMatchesPython(t *testing.T) {
	ref := loadRunnerCleanReference(t)

	compared := 0
	trimspaceDisagreements := 0
	for _, tc := range ref.IsBlankText {
		in := runnerCleanStr(tc.In)
		got := agent.IsBlankText(in)
		if got != tc.Result {
			t.Errorf("%s: IsBlankText(%q) = %v, reference is_blank_text = %v",
				tc.Label, in, got, tc.Result)
		}
		trimSpaceBlank := strings.TrimSpace(in) == ""
		if trimSpaceBlank != tc.Result {
			trimspaceDisagreements++
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("FAIL: zero is_blank_text cases compared")
	}
	// The corpus must actually exercise the trap, otherwise this test could
	// pass while proving nothing.
	if trimspaceDisagreements == 0 {
		t.Fatal("FAIL: the corpus contains no input where strings.TrimSpace " +
			"disagrees with Python's strip — the trap is untested")
	}
	t.Logf("is_blank_text: %d cases compared, %d of them diverge from strings.TrimSpace",
		compared, trimspaceDisagreements)
}

// ---------------------------------------------------------------------------
// _restore_outer_whitespace
// ---------------------------------------------------------------------------

// TestRunnerCleanRestoreOuterWhitespaceMatchesPython compares
// agent.RestoreOuterWhitespace against runner.py:77 over every
// content × original combination in the dump, including original=None, an
// all-whitespace original and the non-ASCII whitespace set.
func TestRunnerCleanRestoreOuterWhitespaceMatchesPython(t *testing.T) {
	ref := loadRunnerCleanReference(t)

	compared := 0
	nonASCII := 0
	for _, tc := range ref.RestoreOuterWhitespace {
		original := runnerCleanStr(tc.Original)
		got := agent.RestoreOuterWhitespace(tc.Content, original)
		if got != tc.Result {
			t.Errorf("RestoreOuterWhitespace(%q, %q) = %q, reference = %q",
				tc.Content, original, got, tc.Result)
		}
		for _, r := range original {
			if r > 0x7f && textutil.PyIsSpace(r) {
				nonASCII++
				break
			}
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("FAIL: zero _restore_outer_whitespace cases compared")
	}
	if nonASCII == 0 {
		t.Fatal("FAIL: no case carried non-ASCII Python whitespace in `original`")
	}
	t.Logf("_restore_outer_whitespace: %d cases compared, %d with non-ASCII whitespace",
		compared, nonASCII)
}

// ---------------------------------------------------------------------------
// The empty-content retry predicate
// ---------------------------------------------------------------------------

// TestRunnerCleanEmptyRetryTruthTable compares the runner's retry condition
// against the reference's, mechanically, for every row the dumper emits:
// 7 finish reasons × {None, blank, non-blank, thinking-only} ×
// {with tool calls, without}.
//
// The Go side reproduces the runner's own expression —
//
//	!resp.HasToolCalls() && agent.EmptyRetryEligible(finishReason, clean)
//
// — because that is what the port evaluates. The reference's
// `retry_check_reached` column (not should_execute_tools) is compared as well,
// which is what justifies !HasToolCalls() as its stand-in.
func TestRunnerCleanEmptyRetryTruthTable(t *testing.T) {
	ref := loadRunnerCleanReference(t)

	compared := 0
	eligible := 0
	excludedByReason := 0
	for _, tc := range ref.EmptyRetryTruthTable {
		content := runnerCleanStr(tc.Content)
		clean := textutil.StripThink(content)
		if clean != runnerCleanStr(tc.Clean) {
			t.Errorf("%s/%s/tools=%v: clean(%q) = %q, reference = %q",
				tc.FinishReason, tc.ContentKind, tc.WithTools,
				content, clean, runnerCleanStr(tc.Clean))
		}

		resp := &core.Response{FinishReason: core.FinishReason(tc.FinishReason)}
		if tc.HasToolCalls {
			resp.ToolCalls = []core.ToolCall{{ID: "c1", Name: "echo"}}
		}

		// The runner's condition.
		got := !resp.HasToolCalls() && agent.EmptyRetryEligible(core.FinishReason(tc.FinishReason), clean)
		if got != tc.RetryEligible {
			t.Errorf("%s/%s/tools=%v: retry eligible = %v, reference = %v",
				tc.FinishReason, tc.ContentKind, tc.WithTools, got, tc.RetryEligible)
		}

		// The control-flow guard the Go condition stands in for.
		if reached := !resp.ShouldExecuteTools(); reached != tc.RetryCheckReached {
			t.Errorf("%s/%s/tools=%v: retry check reached = %v, reference = %v",
				tc.FinishReason, tc.ContentKind, tc.WithTools, reached, tc.RetryCheckReached)
		}
		if tc.RetryEligible {
			eligible++
		}
		switch core.FinishReason(tc.FinishReason) {
		case core.FinishError, core.FinishLength, core.FinishRefusal, core.FinishContentFilter:
			excludedByReason++
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("FAIL: zero empty-retry cases compared")
	}
	if eligible == 0 {
		t.Fatal("FAIL: no case is eligible for the retry — the table proves nothing")
	}
	if excludedByReason == 0 {
		t.Fatal("FAIL: no case exercised the finish_reason exclusion")
	}
	t.Logf("empty-retry truth table: %d cases compared (%d eligible, %d on excluded finish reasons)",
		compared, eligible, excludedByReason)
}

// ---------------------------------------------------------------------------
// _error_response_from_exception detail formatting
// ---------------------------------------------------------------------------

// runnerCleanErrProvider fails every call with a fixed error, so the runner
// routes it through errorResponseFromException exactly as it would a real
// transport failure.
type runnerCleanErrProvider struct{ err error }

func (p *runnerCleanErrProvider) Name() string { return "runner-clean-err" }

func (p *runnerCleanErrProvider) Chat(context.Context, provider.ChatRequest) (*core.Response, error) {
	return nil, p.err
}

// TestRunnerCleanErrorDetailMatchesPython checks base.py:994,
// `detail = str(exc).strip() or type(exc).__name__`, from two directions:
//
//  1. The reference's own output is checked against the rule, so the rule is
//     confirmed rather than assumed. The blank-str cases are the point: the
//     reference emits the CLASS NAME, never an empty or whitespace-only detail.
//  2. The Go port is driven end to end with an error carrying the same message,
//     and its user-visible content is compared.
//
// For a NON-blank message the two must agree byte-for-byte. For a blank one
// they cannot: Python falls back to the exception class name, and Go's closest
// analogue is the dynamic type's unqualified name (agent.goErrorTypeName), so
// the Go side asserts the shape of its own rule and the count of such cases is
// reported. The difference is inherent to the languages, not a porting defect.
func TestRunnerCleanErrorDetailMatchesPython(t *testing.T) {
	ref := loadRunnerCleanReference(t)

	compared := 0
	exact := 0
	fallback := 0
	for _, tc := range ref.ErrorDetail {
		// (1) The reference satisfies its own documented rule.
		want := tc.StrStripped
		if want == "" {
			want = tc.ClassName
		}
		if tc.Detail != "Error calling LLM: "+want {
			t.Errorf("%s: reference detail %q does not match "+
				"`str(exc).strip() or type(exc).__name__` = %q", tc.Label, tc.Detail, want)
		}

		// (2) The Go port, driven end to end.
		res, err := agent.NewRunner().Run(context.Background(), agent.RunSpec{
			Messages: []core.Message{*core.NewMessage(core.RoleUser, "go")},
			Provider: &runnerCleanErrProvider{err: errors.New(tc.StrValue)},
		})
		if err != nil {
			t.Fatalf("%s: Run: %v", tc.Label, err)
		}
		if res.StopReason != agent.StopError {
			t.Errorf("%s: stop reason = %q, want error", tc.Label, res.StopReason)
		}

		if tc.StrStripped != "" {
			if res.FinalContent != tc.Detail {
				t.Errorf("%s: Go detail %q, reference %q", tc.Label, res.FinalContent, tc.Detail)
			}
			exact++
		} else {
			// The message is blank, so both sides fall back to a type name —
			// Python's class name, Go's dynamic type name. Assert the rule, not
			// an equality the languages cannot have.
			if !strings.HasPrefix(res.FinalContent, "Error calling LLM: ") {
				t.Errorf("%s: Go detail %q lacks the reference prefix", tc.Label, res.FinalContent)
			}
			name := strings.TrimPrefix(res.FinalContent, "Error calling LLM: ")
			if name == "" {
				t.Errorf("%s: Go fell back to an empty detail", tc.Label)
			}
			if res.FinalContent == tc.Detail {
				// Not an error, but it would mean the comparison above stopped
				// discriminating; report it so the log is not misleading.
				t.Logf("%s: Go and Python happen to agree on the fallback name (%q)",
					tc.Label, name)
			}
			fallback++
		}
		compared++
	}
	if compared == 0 {
		t.Fatal("FAIL: zero error-detail cases compared")
	}
	if fallback == 0 {
		t.Fatal("FAIL: no case exercised the blank-message fallback")
	}
	if exact == 0 {
		t.Fatal("FAIL: no case compared the non-blank message path exactly")
	}
	t.Logf("error detail: %d cases compared (%d exact, %d type-name fallbacks)",
		compared, exact, fallback)
}
