// Package tools defines the tool boundary and registry.
//
// Mirrors nanobot/agent/tools/base.py:Tool and
// nanobot/agent/tools/registry.py at upstream 1bb712d3.
//
// The concurrency flags are load-bearing, not advisory: the runner uses
// ConcurrencySafe to decide which calls may run in parallel and Exclusive to
// force a call to run alone.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/filediff"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
)

// Result is the outcome of a tool execution.
// Mirrors nanobot/agent/tools/base.py:ToolResult (a str subclass carrying
// is_error), so failures travel as content rather than as a Go error.
type Result struct {
	Content string
	IsError bool
	// FileDiffs carries the line-level alignment of every file a mutating
	// filesystem tool changed, keyed by the resolved absolute path.
	//
	// It mirrors the file_diffs attribute of the reference's FileEditResult
	// (utils/file_edit_events.py:135-147), the str subclass that write-capable
	// tools return so the file-edit tracker can build its activity event
	// without recomputing the diff. Go has no str subclass, so the payload
	// rides alongside the content instead of on it.
	//
	// nil means "this tool produced no diffs" — including every read-only tool,
	// every failing call, and apply_patch's dry-run arm, which the reference
	// also returns as a plain str with no diffs attached.
	FileDiffs map[string]filediff.FileDiff
}

// OK returns a successful result.
func OK(content string) Result { return Result{Content: content} }

// Errf returns an error result.
func Errf(format string, a ...any) Result {
	return Result{Content: fmt.Sprintf(format, a...), IsError: true}
}

// Tool is a capability the model can invoke.
type Tool interface {
	// Name is the tool identifier used by the model.
	Name() string
	// Description explains the tool to the model.
	Description() string
	// Parameters returns a JSON Schema object describing the arguments.
	Parameters() json.RawMessage
	// Execute runs the tool with raw JSON arguments.
	//
	// A returned error aborts the call; a Result with IsError reports a
	// failure the model should see and recover from. Prefer the latter for
	// expected failures (bad path, non-zero exit).
	Execute(ctx context.Context, args json.RawMessage) (Result, error)
}

// Concurrency describes whether a tool may run alongside others.
// Mirrors Tool.read_only / Tool.concurrency_safe / Tool.exclusive.
type Concurrency interface {
	// ReadOnly reports that the tool does not mutate state.
	ReadOnly() bool
	// ConcurrencySafe reports that parallel invocation is safe.
	ConcurrencySafe() bool
	// Exclusive reports that the tool must run alone.
	Exclusive() bool
}

// Base provides conservative defaults for the optional interfaces.
// Embed it in a tool to inherit them.
type Base struct{}

// ReadOnly reports false by default (conservative).
func (Base) ReadOnly() bool { return false }

// ConcurrencySafe reports false by default (conservative).
func (Base) ConcurrencySafe() bool { return false }

// Exclusive reports false by default.
func (Base) Exclusive() bool { return false }

// ReadOnlyBase provides defaults for tools that only read state.
type ReadOnlyBase struct{}

func (ReadOnlyBase) ReadOnly() bool        { return true }
func (ReadOnlyBase) ConcurrencySafe() bool { return true }
func (ReadOnlyBase) Exclusive() bool       { return false }

// IsReadOnly reports whether a tool declares itself read-only.
func IsReadOnly(t Tool) bool {
	if c, ok := t.(Concurrency); ok {
		return c.ReadOnly()
	}
	return false
}

// IsConcurrencySafe reports whether a tool may run in parallel.
func IsConcurrencySafe(t Tool) bool {
	if c, ok := t.(Concurrency); ok {
		return c.ConcurrencySafe()
	}
	return false
}

// IsExclusive reports whether a tool must run alone.
func IsExclusive(t Tool) bool {
	if c, ok := t.(Concurrency); ok {
		return c.Exclusive()
	}
	return false
}

type sessionKeyContextKey struct{}

// WithSessionKey attaches the current agent session to tool execution without
// exposing it as a model-controlled argument.
func WithSessionKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, sessionKeyContextKey{}, key)
}

// SessionKeyFromContext returns the active session supplied by the runner.
func SessionKeyFromContext(ctx context.Context) string {
	key, _ := ctx.Value(sessionKeyContextKey{}).(string)
	return key
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// ErrUnknownTool is returned when the model calls a tool that is not registered.
var ErrUnknownTool = errors.New("tools: unknown tool")

// ErrInvalidToolName is returned for a degenerate tool name. Such calls must
// never be executed or persisted: replaying one makes upstream APIs reject the
// whole request and permanently wedges the session
// (see ToolCallRequest.has_valid_name, base.py:73).
var ErrInvalidToolName = errors.New("tools: invalid tool name")

// Registry holds the available tools.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	order []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register adds a tool. Registering a duplicate name replaces the previous
// tool but keeps the original advertisement order.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := t.Name()
	if _, exists := r.tools[name]; !exists {
		r.order = append(r.order, name)
	}
	r.tools[name] = t
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names returns registered tool names in registration order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Len returns the number of registered tools.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// Schemas returns the tool definitions to advertise to the model.
//
// Mirrors get_definitions (agent/tools/registry.py:86-108): built-in tools are
// sorted by name as a stable prefix, then MCP tools (names prefixed "mcp_") are
// sorted and appended. The reference does this so the prompt prefix stays
// cacheable — sorting is what makes it deterministic, NOT registration order.
//
// This was previously registration order, which diverged from the reference for
// every request: builtin.Register registers
// apply_patch, read_file, write_file, edit_file, list_dir, exec, so the port
// advertised that order where the reference advertises the sorted
// apply_patch, edit_file, exec, list_dir, read_file, write_file. Verified by
// running the reference's ToolRegistry.get_definitions with the port's exact
// registration order.
//
// Python's str.sort() orders by code point; sort.Strings orders by byte. UTF-8
// preserves code-point order, so the two agree for every input.
func (r *Registry) Schemas() []provider.ToolSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()

	builtins := make([]provider.ToolSchema, 0, len(r.order))
	mcpTools := make([]provider.ToolSchema, 0)
	for _, name := range r.order {
		t := r.tools[name]
		params := t.Parameters()
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		schema := provider.ToolSchema{
			Name:        name,
			Description: t.Description(),
			Parameters:  params,
		}
		// The reference partitions on the schema name, which is the tool name.
		if strings.HasPrefix(name, "mcp_") {
			mcpTools = append(mcpTools, schema)
		} else {
			builtins = append(builtins, schema)
		}
	}

	sort.Slice(builtins, func(i, j int) bool { return builtins[i].Name < builtins[j].Name })
	sort.Slice(mcpTools, func(i, j int) bool { return mcpTools[i].Name < mcpTools[j].Name })

	return append(builtins, mcpTools...)
}

// Execute runs a tool call and always returns a core.ToolResult. Unknown tools,
// invalid names and execution errors are converted into error results so the
// model can observe them, matching the Python behavior of surfacing tool
// failures as content.
func (r *Registry) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	if !call.HasValidName() {
		return core.ToolResult{
			CallID:  call.ID,
			Content: "Error: tool call has an empty or invalid name and was not executed.",
			IsError: true,
		}
	}
	t, ok := r.Get(call.Name)
	if !ok {
		return core.ToolResult{
			CallID:  call.ID,
			Content: fmt.Sprintf("Error: unknown tool %q. Available tools: %s", call.Name, strings.Join(r.Names(), ", ")),
			IsError: true,
		}
	}

	args := NormalizeArguments(call.Arguments)
	if args.err != nil {
		return core.ToolResult{
			CallID:  call.ID,
			Content: fmt.Sprintf("Error: invalid arguments for tool %q: %v", call.Name, args.err),
			IsError: true,
		}
	}

	res, err := t.Execute(ctx, args.json)
	if err != nil {
		// Cancellation must propagate so the turn can abort cleanly.
		if ctx.Err() != nil {
			return core.ToolResult{
				CallID:  call.ID,
				Content: fmt.Sprintf("Error: tool %q canceled: %v", call.Name, ctx.Err()),
				IsError: true,
			}
		}
		return core.ToolResult{
			CallID:  call.ID,
			Content: fmt.Sprintf("Error: tool %q failed: %v", call.Name, err),
			IsError: true,
		}
	}
	return core.ToolResult{
		CallID:    call.ID,
		Content:   res.Content,
		IsError:   res.IsError,
		FileDiffs: res.FileDiffs,
	}
}

// normalizedArgs is the result of argument normalization.
type normalizedArgs struct {
	json json.RawMessage
	err  error
}

// NormalizeArguments coerces provider-supplied tool arguments into a JSON
// object, mirroring parse_tool_arguments (base.py:112):
//
//   - absent/empty arguments become {}
//   - a JSON object passes through unchanged
//   - a JSON array or scalar is rejected, because it cannot be a valid
//     parameter object and executing it would be guessing
//   - malformed JSON is rejected
//
// Unlike the Python version, which preserves malformed values for the registry
// to reject later, this returns the error directly: the outcome is the same
// (the call is refused before execution) and it removes a failure mode where a
// non-object silently reaches a tool.
func NormalizeArguments(raw json.RawMessage) normalizedArgs {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return normalizedArgs{json: json.RawMessage(`{}`)}
	}
	if trimmed[0] != '{' {
		return normalizedArgs{err: fmt.Errorf("arguments must be a JSON object, got %s", describeJSON(trimmed))}
	}
	if !json.Valid(raw) {
		return normalizedArgs{err: errors.New("arguments are not valid JSON")}
	}
	return normalizedArgs{json: raw}
}

func describeJSON(s string) string {
	if s == "" {
		return "empty"
	}
	switch s[0] {
	case '[':
		return "an array"
	case '"':
		return "a string"
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	}
	if s[0] == '-' || (s[0] >= '0' && s[0] <= '9') {
		return "a number"
	}
	return "a non-object value"
}

// SortedNames returns registered names sorted, for deterministic diagnostics.
func (r *Registry) SortedNames() []string {
	names := r.Names()
	sort.Strings(names)
	return names
}
