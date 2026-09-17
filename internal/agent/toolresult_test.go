package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// echoBigTool returns a fixed oversized payload.
type echoBigTool struct {
	tools.ReadOnlyBase
	out string
}

func (t *echoBigTool) Name() string                { return "echo_big" }
func (t *echoBigTool) Description() string         { return "returns a large payload" }
func (t *echoBigTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *echoBigTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{Content: t.out}, nil
}

// emptyTool returns nothing at all.
type emptyTool struct{ tools.ReadOnlyBase }

func (t *emptyTool) Name() string                { return "empty_tool" }
func (t *emptyTool) Description() string         { return "returns nothing" }
func (t *emptyTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *emptyTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return tools.Result{Content: ""}, nil
}

// toolCallProvider issues one tool call, then answers.
type toolCallProvider struct {
	toolName string
	round    int
	seen     [][]core.Message
}

func (p *toolCallProvider) Name() string { return "toolcall" }

func (p *toolCallProvider) Chat(_ context.Context, req provider.ChatRequest) (*core.Response, error) {
	p.seen = append(p.seen, append([]core.Message(nil), req.Messages...))
	p.round++
	if p.round == 1 {
		return &core.Response{
			FinishReason: core.FinishToolCalls,
			ToolCalls: []core.ToolCall{{
				ID: "call_1", Name: p.toolName, Arguments: json.RawMessage(`{}`),
			}},
		}, nil
	}
	return &core.Response{Content: "done", FinishReason: core.FinishStop}, nil
}

// TestOversizedToolResultIsOffloadedToWorkspace is the core behaviour: the
// payload lands on disk and the model receives a reference it can follow,
// instead of a silently truncated blob.
func TestOversizedToolResultIsOffloadedToWorkspace(t *testing.T) {
	workspace := t.TempDir()
	payload := strings.Repeat("Z", 4000)

	registry := tools.NewRegistry()
	registry.Register(&echoBigTool{out: payload})

	p := &toolCallProvider{toolName: "echo_big"}
	runner := NewRunner()
	_, err := runner.Run(context.Background(), RunSpec{
		Messages:   []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Tools:      registry,
		Provider:   p,
		Workspace:  workspace,
		SessionKey: "cli:1",
		// The real default. A limit smaller than the rendered reference makes
		// the reference collapse to "[truncated: <path>]", which is correct
		// behaviour but hides the fields this test checks.
		MaxToolResultChars:  2500,
		ContextWindowTokens: 200000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The tool message from the SECOND request is the one carrying the result.
	if len(p.seen) < 2 {
		t.Fatalf("provider called %d times, want 2", len(p.seen))
	}
	var toolMsg *core.Message
	for i := range p.seen[1] {
		if p.seen[1][i].Role == core.RoleTool {
			toolMsg = &p.seen[1][i]
		}
	}
	if toolMsg == nil {
		t.Fatal("no tool message was sent")
	}

	text := toolMsg.Content.Text
	if !strings.Contains(text, "[tool output persisted]") {
		t.Errorf("result was not offloaded; message = %q", text)
	}
	if len(text) > 2500 {
		t.Errorf("reference exceeds max_tool_result_chars: %d", len(text))
	}

	// The full payload must exist on disk, under the documented location.
	expectedDir := filepath.Join(workspace, ".nanobot", "tool-results", "cli_1")
	entries, err := os.ReadDir(expectedDir)
	if err != nil {
		t.Fatalf("tool-results bucket missing: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("bucket has %d entries, want 1", len(entries))
	}
	saved, err := os.ReadFile(filepath.Join(expectedDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read saved payload: %v", err)
	}
	if string(saved) != payload {
		t.Errorf("saved payload is %d bytes, want %d", len(saved), len(payload))
	}

	// The path in the message must be absolute and must be the file we wrote.
	if !strings.Contains(text, filepath.Join(expectedDir, entries[0].Name())) {
		t.Errorf("message does not name the saved file:\n%s", text)
	}
	if !strings.Contains(text, "Original size: 4000 chars") {
		t.Errorf("message does not state the original size:\n%s", text)
	}
}

// TestOversizedToolResultWithoutWorkspaceIsTruncated covers the no-workspace
// path: the reference cannot offer a path, so it truncates in place using its
// own marker.
func TestOversizedToolResultWithoutWorkspaceIsTruncated(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(&echoBigTool{out: strings.Repeat("Z", 5000)})

	p := &toolCallProvider{toolName: "echo_big"}
	runner := NewRunner()
	_, err := runner.Run(context.Background(), RunSpec{
		Messages:            []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Tools:               registry,
		Provider:            p,
		MaxToolResultChars:  200,
		ContextWindowTokens: 200000,
		// Workspace deliberately unset.
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var text string
	for _, m := range p.seen[1] {
		if m.Role == core.RoleTool {
			text = m.Content.Text
		}
	}
	// Bound is max_chars plus the suffix the reference appends after cutting.
	if len([]rune(text)) > 200+len(textutil.TruncatedSuffix) {
		t.Errorf("result not bounded: %d chars", len([]rune(text)))
	}
	if !strings.Contains(text, "... (truncated)") {
		t.Errorf("missing the reference truncation marker: %q", text)
	}
}

// TestEmptyToolResultBecomesMarker guards the ambiguity that makes models loop:
// an empty result must not be sent as an empty string.
func TestEmptyToolResultBecomesMarker(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(&emptyTool{})

	p := &toolCallProvider{toolName: "empty_tool"}
	runner := NewRunner()
	_, err := runner.Run(context.Background(), RunSpec{
		Messages:            []core.Message{*core.NewMessage(core.RoleUser, "go")},
		Tools:               registry,
		Provider:            p,
		ContextWindowTokens: 200000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var text string
	for _, m := range p.seen[1] {
		if m.Role == core.RoleTool {
			text = m.Content.Text
		}
	}
	if text != "(empty_tool completed with no output)" {
		t.Errorf("empty result = %q, want the reference marker", text)
	}
}

// TestReadFileIsNeverOffloaded pins the exemption: read_file has its own bound,
// and offloading it would create a persist -> read -> persist loop.
func TestReadFileIsNeverOffloaded(t *testing.T) {
	workspace := t.TempDir()
	payload := strings.Repeat("Q", 4000)

	got := NormalizeToolResult(workspace, "cli:1", "c1", "read_file",
		core.TextContent(payload), 2500)

	if !got.IsText() || got.Text != payload {
		t.Errorf("read_file result was modified: %d chars, want %d", len(got.Text), len(payload))
	}
	if _, err := os.Stat(filepath.Join(workspace, ".nanobot")); !os.IsNotExist(err) {
		t.Error("read_file result was written to the workspace despite the exemption")
	}
}

// TestBlockContentOffloadsOnlyTextBlocks verifies image blocks pass through:
// replacing them would change what the model sees, not merely where it reads.
func TestBlockContentOffloadsOnlyTextBlocks(t *testing.T) {
	workspace := t.TempDir()
	blocks := []core.ContentBlock{
		{Type: "text", Text: strings.Repeat("T", 4000)},
		{Type: "image_url", ImageURL: json.RawMessage(`{"url":"http://x/y.png"}`)},
	}

	got := NormalizeToolResult(workspace, "cli:1", "c1", "some_tool",
		core.BlockContent(blocks), 2500)

	if got.IsText() {
		t.Fatal("block content was collapsed to a string")
	}
	if len(got.Blocks) != 2 {
		t.Fatalf("block count = %d, want 2", len(got.Blocks))
	}
	if !strings.Contains(got.Blocks[0].Text, "[tool output persisted]") {
		t.Errorf("text block was not offloaded: %q", got.Blocks[0].Text)
	}
	if string(got.Blocks[1].ImageURL) != `{"url":"http://x/y.png"}` {
		t.Errorf("image block was modified: %s", got.Blocks[1].ImageURL)
	}
}

// TestToolResultPathIsStableAcrossCalls verifies the same call id maps to the
// same file, so a retried turn does not accumulate duplicates.
func TestToolResultPathIsStableAcrossCalls(t *testing.T) {
	workspace := t.TempDir()
	payload := strings.Repeat("R", 4000)

	first := NormalizeToolResult(workspace, "cli:1", "c1", "t", core.TextContent(payload), 2500)
	second := NormalizeToolResult(workspace, "cli:1", "c1", "t", core.TextContent(payload), 2500)

	if first.Text != second.Text {
		t.Errorf("same call id produced different references:\n  %q\n  %q", first.Text, second.Text)
	}
	bucket := filepath.Join(workspace, ".nanobot", "tool-results", "cli_1")
	entries, err := os.ReadDir(bucket)
	if err != nil {
		t.Fatalf("read bucket: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("bucket has %d files, want 1 (a retry must not duplicate)", len(entries))
	}
}

// TestReferenceCollapsesWhenLimitIsTiny pins the reference's fallback: when the
// rendered reference itself would exceed max_chars, it is replaced by a bare
// "[truncated: <path>]" marker (helpers.py:529). Without this the "bounded"
// result could itself be unbounded.
func TestReferenceCollapsesWhenLimitIsTiny(t *testing.T) {
	workspace := t.TempDir()
	got := NormalizeToolResult(workspace, "cli:1", "c1", "t",
		core.TextContent(strings.Repeat("Z", 5000)), 200)

	if !got.IsText() {
		t.Fatal("expected text content")
	}
	if !strings.HasPrefix(got.Text, "[truncated: ") {
		t.Errorf("expected the collapsed marker, got %q", got.Text)
	}
	if len(got.Text) > 200 {
		t.Errorf("collapsed marker still exceeds the limit: %d", len(got.Text))
	}
}
