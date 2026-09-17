package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
)

// stubTool is a configurable test tool.
type stubTool struct {
	ReadOnlyBase
	name    string
	desc    string
	params  string
	run     func(ctx context.Context, args json.RawMessage) (Result, error)
	mutates bool
	excl    bool
}

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return s.desc }
func (s *stubTool) Parameters() json.RawMessage {
	if s.params == "" {
		return json.RawMessage(`{"type":"object"}`)
	}
	return json.RawMessage(s.params)
}
func (s *stubTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	if s.run == nil {
		return OK("ok"), nil
	}
	return s.run(ctx, args)
}

// Concurrency overrides for the mutating/exclusive cases.
func (s *stubTool) ReadOnly() bool        { return !s.mutates }
func (s *stubTool) ConcurrencySafe() bool { return !s.mutates }
func (s *stubTool) Exclusive() bool       { return s.excl }

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewRegistry()
	a := &stubTool{name: "a"}
	r.Register(a)

	if got, ok := r.Get("a"); !ok || got.Name() != "a" {
		t.Fatalf("Get(a) = %v, %v", got, ok)
	}
	if _, ok := r.Get("missing"); ok {
		t.Error("Get(missing) should not be found")
	}
	if r.Len() != 1 {
		t.Errorf("Len = %d", r.Len())
	}
}

func TestRegistryDuplicateReplacesButKeepsOrder(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "a"})
	r.Register(&stubTool{name: "b"})
	r.Register(&stubTool{name: "a"}) // replace

	if r.Len() != 2 {
		t.Errorf("Len = %d, want 2", r.Len())
	}
	names := r.Names()
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Errorf("order = %v, want [a b]", names)
	}
}

func TestSchemasSortAlphabetically(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "zeta", desc: "z"})
	r.Register(&stubTool{name: "alpha", desc: "a"})

	got := r.Schemas()
	if len(got) != 2 {
		t.Fatalf("got %d schemas", len(got))
	}
	// ALPHABETICAL, not registration order. get_definitions
	// (agent/tools/registry.py:86-108) sorts built-ins by name as a stable
	// prefix, then sorts and appends "mcp_*" tools. Sorting is what keeps the
	// prompt prefix cacheable, and it is what the reference actually does.
	//
	// This test previously asserted registration order ("a stable tool order
	// keeps the prompt prefix cacheable"), which was a plausible-sounding
	// assumption that the reference contradicts. Verified by running the
	// reference's ToolRegistry.get_definitions() with a registration order of
	// [zeta, alpha]: it returns [alpha, zeta].
	if got[0].Name != "alpha" || got[1].Name != "zeta" {
		t.Errorf("order = %s, %s; want alpha, zeta (get_definitions sorts)", got[0].Name, got[1].Name)
	}
}

// TestSchemasSortBuiltinsThenMCPTools pins the two-group partition of
// get_definitions (registry.py:96-108): non-"mcp_" tools sorted first, then
// "mcp_" tools sorted, concatenated. Verified by running the reference with the
// same registration order, which returned
// [apply_patch, edit_file, exec, list_dir, read_file, write_file, mcp_aaa, mcp_zzz].
func TestSchemasSortBuiltinsThenMCPTools(t *testing.T) {
	r := NewRegistry()
	// Deliberately the Go port's registration order, plus interleaved mcp tools.
	for _, n := range []string{
		"apply_patch", "read_file", "write_file", "edit_file", "list_dir", "exec",
		"mcp_zzz", "mcp_aaa",
	} {
		r.Register(&stubTool{name: n})
	}

	var names []string
	for _, s := range r.Schemas() {
		names = append(names, s.Name)
	}
	want := "apply_patch,edit_file,exec,list_dir,read_file,write_file,mcp_aaa,mcp_zzz"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("Schemas order =\n %s\nwant\n %s", got, want)
	}
}

func TestSchemasDefaultParametersWhenEmpty(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "empty", params: ""})
	s := r.Schemas()[0]
	if !strings.Contains(string(s.Parameters), `"type":"object"`) {
		t.Errorf("parameters = %s", s.Parameters)
	}
}

func TestOpenAIToolShape(t *testing.T) {
	s := provider.ToolSchema{Name: "f", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}
	got := s.OpenAITool()
	if got["type"] != "function" {
		t.Errorf("type = %v", got["type"])
	}
	fn, ok := got["function"].(map[string]any)
	if !ok {
		t.Fatalf("function = %T", got["function"])
	}
	if fn["name"] != "f" || fn["description"] != "d" {
		t.Errorf("function = %v", fn)
	}
}

func TestExecuteUnknownTool(t *testing.T) {
	r := NewRegistry()
	res := r.Execute(context.Background(), core.ToolCall{ID: "1", Name: "nope", Arguments: json.RawMessage(`{}`)})
	if !res.IsError {
		t.Error("expected error result")
	}
	if res.CallID != "1" {
		t.Errorf("CallID = %q", res.CallID)
	}
	if !strings.Contains(res.Content, "unknown tool") {
		t.Errorf("content = %q", res.Content)
	}
}

// TestExecuteRejectsInvalidToolName is the regression test for the
// session-wedging bug (base.py:73): a call with an empty name must never run.
func TestExecuteRejectsInvalidToolName(t *testing.T) {
	r := NewRegistry()
	ran := false
	r.Register(&stubTool{name: "t", run: func(context.Context, json.RawMessage) (Result, error) {
		ran = true
		return OK("ran"), nil
	}})

	res := r.Execute(context.Background(), core.ToolCall{ID: "1", Name: "", Arguments: json.RawMessage(`{}`)})
	if !res.IsError {
		t.Error("expected error result for empty tool name")
	}
	if ran {
		t.Error("tool executed despite invalid name")
	}
}

func TestExecuteSuccess(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "echo", run: func(_ context.Context, args json.RawMessage) (Result, error) {
		return OK("got:" + string(args)), nil
	}})
	res := r.Execute(context.Background(), core.ToolCall{
		ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"a":1}`)})
	if res.IsError {
		t.Errorf("unexpected error: %s", res.Content)
	}
	if res.Content != `got:{"a":1}` {
		t.Errorf("content = %q", res.Content)
	}
	if res.CallID != "c1" {
		t.Errorf("CallID = %q", res.CallID)
	}
}

func TestExecuteToolErrorBecomesContent(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "boom", run: func(context.Context, json.RawMessage) (Result, error) {
		return Result{}, errors.New("kaboom")
	}})
	res := r.Execute(context.Background(), core.ToolCall{ID: "1", Name: "boom", Arguments: json.RawMessage(`{}`)})
	if !res.IsError {
		t.Error("expected error result")
	}
	if !strings.Contains(res.Content, "kaboom") {
		t.Errorf("content = %q", res.Content)
	}
}

func TestExecuteCancellationSurfaces(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "slow", run: func(ctx context.Context, _ json.RawMessage) (Result, error) {
		return Result{}, ctx.Err()
	}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := r.Execute(ctx, core.ToolCall{ID: "1", Name: "slow", Arguments: json.RawMessage(`{}`)})
	if !res.IsError {
		t.Error("expected error result")
	}
	if !strings.Contains(res.Content, "canceled") {
		t.Errorf("content = %q", res.Content)
	}
}

func TestExecuteResultIsErrorPassthrough(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "soft", run: func(context.Context, json.RawMessage) (Result, error) {
		return Result{Content: "soft failure", IsError: true}, nil
	}})
	res := r.Execute(context.Background(), core.ToolCall{ID: "1", Name: "soft", Arguments: json.RawMessage(`{}`)})
	if !res.IsError || res.Content != "soft failure" {
		t.Errorf("res = %+v", res)
	}
}

// ---------------------------------------------------------------------------
// Argument normalization
// ---------------------------------------------------------------------------

func TestNormalizeArguments(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty becomes object", "", "{}", false},
		{"whitespace becomes object", "   ", "{}", false},
		{"null becomes object", "null", "{}", false},
		{"object passes through", `{"a":1}`, `{"a":1}`, false},
		{"nested object", `{"a":{"b":[1,2]}}`, `{"a":{"b":[1,2]}}`, false},
		{"array rejected", `[1,2]`, "", true},
		{"string rejected", `"hello"`, "", true},
		{"number rejected", `42`, "", true},
		{"bool rejected", `true`, "", true},
		{"malformed rejected", `{`, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NormalizeArguments(json.RawMessage(c.in))
			if c.wantErr {
				if got.err == nil {
					t.Fatalf("expected error, got %s", got.json)
				}
				return
			}
			if got.err != nil {
				t.Fatalf("unexpected error: %v", got.err)
			}
			if string(got.json) != c.want {
				t.Errorf("got %s, want %s", got.json, c.want)
			}
		})
	}
}

func TestExecuteRejectsNonObjectArguments(t *testing.T) {
	r := NewRegistry()
	ran := false
	r.Register(&stubTool{name: "t", run: func(context.Context, json.RawMessage) (Result, error) {
		ran = true
		return OK("ran"), nil
	}})

	res := r.Execute(context.Background(), core.ToolCall{ID: "1", Name: "t", Arguments: json.RawMessage(`[1,2,3]`)})
	if !res.IsError {
		t.Error("expected error for array arguments")
	}
	if ran {
		t.Error("tool executed with invalid arguments")
	}
	if !strings.Contains(res.Content, "invalid arguments") {
		t.Errorf("content = %q", res.Content)
	}
}

func TestNormalizeArgumentsErrorMessagesAreSpecific(t *testing.T) {
	// The message reaches the model, so it should say what was wrong.
	cases := map[string]string{
		`[1]`:  "an array",
		`"x"`:  "a string",
		`true`: "a boolean",
		`5`:    "a number",
	}
	for in, want := range cases {
		got := NormalizeArguments(json.RawMessage(in))
		if got.err == nil || !strings.Contains(got.err.Error(), want) {
			t.Errorf("input %s: err = %v, want containing %q", in, got.err, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Concurrency flags
// ---------------------------------------------------------------------------

func TestConcurrencyFlags(t *testing.T) {
	ro := &stubTool{name: "ro", mutates: false}
	if !IsReadOnly(ro) || !IsConcurrencySafe(ro) || IsExclusive(ro) {
		t.Errorf("read-only tool flags wrong: ro=%v safe=%v excl=%v",
			IsReadOnly(ro), IsConcurrencySafe(ro), IsExclusive(ro))
	}
	rw := &stubTool{name: "rw", mutates: true}
	if IsReadOnly(rw) || IsConcurrencySafe(rw) {
		t.Error("mutating tool must not be read-only or concurrency-safe")
	}
	ex := &stubTool{name: "ex", mutates: false, excl: true}
	if !IsExclusive(ex) {
		t.Error("exclusive flag not honoured")
	}
	_ = Base{}
}

func TestBaseDefaultsAreConservative(t *testing.T) {
	// A tool that does not implement Concurrency must be treated as unsafe.
	b := &stubTool{name: "bare", mutates: true}
	if IsReadOnly(b) || IsConcurrencySafe(b) || IsExclusive(b) {
		t.Error("Base defaults must be conservative")
	}
}

func TestSortedNames(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "z"})
	r.Register(&stubTool{name: "a"})
	got := r.SortedNames()
	if len(got) != 2 || got[0] != "a" || got[1] != "z" {
		t.Errorf("SortedNames = %v", got)
	}
}
