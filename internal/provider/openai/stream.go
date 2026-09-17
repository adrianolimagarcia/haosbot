package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
)

// errStreamEndedEarly mirrors the reference's
// ConnectionError("Model stream ended before a finish reason was received")
// (openai_compat_provider.py:2188-2189).
const errStreamEndedEarly = "Model stream ended before a finish reason was received"

// ChatStream starts a streamed completion.
//
// The returned channel is closed by the provider when the stream ends. A final
// StreamDone event carries the aggregated response, matching
// OpenAICompatProvider.chat_stream -> _parse_chunks
// (openai_compat_provider.py:2015-2201, 1688-1846).
//
// The channel never blocks a consumer that stopped reading: every send selects
// on ctx, and a cancelled ctx tears the HTTP body down, so the goroutine always
// exits and the channel always closes.
func (c *Client) ChatStream(ctx context.Context, req provider.ChatRequest) (<-chan core.StreamEvent, error) {
	body, err := c.buildBody(req, true)
	if err != nil {
		return nil, err
	}

	// readCtx bounds the HTTP request: it is cancelled when the stream
	// finishes, when the idle timer fires, or when the caller's context is
	// done. Event delivery is gated on the caller's context instead, so a
	// timeout report can still reach a consumer that is still reading.
	readCtx, cancel := context.WithCancel(ctx)
	resp, err := c.post(readCtx, body)
	if err != nil {
		cancel()
		return nil, err
	}

	events := make(chan core.StreamEvent)
	go c.consumeStreamWithFormat(ctx, readCtx, cancel, resp.Body, resolveStreamIdleTimeout(), events, c.toolCallFormatForModel(req.Model))
	return events, nil
}

// consumeStream reads the SSE body, emits deltas, and closes the channel.
func (c *Client) consumeStreamWithFormat(
	ctx context.Context,
	readCtx context.Context,
	cancel context.CancelFunc,
	body io.ReadCloser,
	idle time.Duration,
	events chan<- core.StreamEvent,
	format ToolCallFormat,
) {
	defer close(events)
	defer cancel()
	defer body.Close()

	// Stream idle timeout: the reference wraps every chunk read in
	// asyncio.wait_for(..., timeout=idle_timeout_s)
	// (openai_compat_provider.py:2134-2139). Here the timer cancels the request
	// context, which unblocks the body read.
	var stalled atomic.Bool
	timer := time.AfterFunc(idle, func() {
		stalled.Store(true)
		cancel()
	})
	defer timer.Stop()

	aggregate := newStreamAggregator()
	reader := bufio.NewReader(body)
	var sse sseReader
	var failure *streamFailure
	var readErr error
	done := false

	for !done && failure == nil {
		line, err := reader.ReadString('\n')
		if line != "" {
			timer.Reset(idle)
			if payload, ok := sse.push(line); ok {
				done, failure = dispatchChunk(payload, aggregate, ctx, events)
			}
		}
		if err != nil {
			readErr = err
			break
		}
	}

	if !done && failure == nil {
		if payload, ok := sse.flush(); ok {
			// The loop has already exited, so the returned `done` is never read
			// again — only failure feeds the switch below.
			_, failure = dispatchChunk(payload, aggregate, ctx, events)
		}
	}

	switch {
	case stalled.Load():
		// Mirrors the asyncio.TimeoutError branch
		// (openai_compat_provider.py:2191-2199).
		sendEvent(ctx, events, core.StreamEvent{
			Kind: core.StreamDone,
			Response: &core.Response{
				Content: fmt.Sprintf(
					"Error calling LLM: stream stalled for more than %s seconds",
					formatSeconds(idle),
				),
				FinishReason: core.FinishError,
				ErrorKind:    "timeout",
			},
			Err: errors.New("openai: stream idle timeout"),
		})
	case failure != nil:
		sendEvent(ctx, events, core.StreamEvent{
			Kind:     core.StreamDone,
			Response: &core.Response{Content: failure.content, FinishReason: core.FinishError, ErrorKind: failure.kind},
			Err:      failure.err,
		})
	case ctx.Err() != nil:
		// The caller cancelled: the runner has already stopped reading and
		// reports its own context error, so nothing is emitted here.
	case readErr != nil && !errors.Is(readErr, io.EOF):
		sendEvent(ctx, events, core.StreamEvent{
			Kind: core.StreamDone,
			Response: &core.Response{
				Content:      "Error calling LLM: " + readErr.Error(),
				FinishReason: core.FinishError,
				ErrorKind:    "connection",
			},
			Err: readErr,
		})
	case !aggregate.completed:
		sendEvent(ctx, events, core.StreamEvent{
			Kind: core.StreamDone,
			Response: &core.Response{
				Content:      "Error calling LLM: " + errStreamEndedEarly,
				FinishReason: core.FinishError,
				ErrorKind:    "connection",
			},
			Err: errors.New(errStreamEndedEarly),
		})
	default:
		sendEvent(ctx, events, core.StreamEvent{Kind: core.StreamDone, Response: aggregate.response(format)})
	}
}

// streamFailure is a terminal, reference-worded stream error.
type streamFailure struct {
	content string
	kind    string
	err     error
}

// formatSeconds renders a duration the way Python's f"{value:g}" does, so the
// stall message matches the reference wording.
func formatSeconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'g', -1, 64)
}

// sendEvent delivers one event, giving up when the consumer's context is done.
func sendEvent(ctx context.Context, events chan<- core.StreamEvent, event core.StreamEvent) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

// ---------------------------------------------------------------------------
// SSE framing
// ---------------------------------------------------------------------------

// sseReader turns SSE lines into event payloads.
//
// Events are delimited by a blank line, as the spec and the reference SDK
// require (SDK: openai/_streaming.py:339-360). Two deliberate allowances:
//
//   - a pending buffer that is already complete JSON is dispatched when the
//     next data line arrives. Several OpenAI-compatible servers (and hand
//     written test servers) omit the trailing blank line; a complete JSON
//     payload can never be the prefix of a longer one, so this cannot merge two
//     real events.
//   - a trailing unterminated event is dispatched at EOF. The SDK drops it,
//     which would turn a well-formed answer into "stream ended before a finish
//     reason was received".
type sseReader struct {
	pending []string
}

func (r *sseReader) push(line string) (string, bool) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return r.take()
	}
	if strings.HasPrefix(line, ":") {
		// SSE comment, commonly used as a keep-alive.
		return "", false
	}
	field, value, found := strings.Cut(line, ":")
	if !found || field != "data" {
		return "", false
	}
	value = strings.TrimPrefix(value, " ")

	if len(r.pending) > 0 && json.Valid([]byte(strings.Join(r.pending, "\n"))) {
		payload := strings.Join(r.pending, "\n")
		r.pending = append(r.pending[:0], value)
		return payload, true
	}
	r.pending = append(r.pending, value)
	return "", false
}

// flush returns a trailing event that no blank line terminated.
func (r *sseReader) flush() (string, bool) { return r.take() }

func (r *sseReader) take() (string, bool) {
	if len(r.pending) == 0 {
		return "", false
	}
	payload := strings.Join(r.pending, "\n")
	r.pending = r.pending[:0]
	return payload, true
}

// dispatchChunk processes one SSE payload. It reports whether the stream is
// finished ([DONE]) and any terminal failure.
func dispatchChunk(payload string, aggregate *streamAggregator, ctx context.Context, events chan<- core.StreamEvent) (bool, *streamFailure) {
	if strings.TrimSpace(payload) == "[DONE]" {
		return true, nil
	}

	// A 200 response whose chunk carries a truthy "error" object is a failure
	// (SDK: openai/_streaming.py:82-95); the reference surfaces it through
	// _handle_error, which prefixes the body with "Error: ".
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return false, &streamFailure{
			content: "Error calling LLM: " + err.Error(),
			err:     err,
		}
	}
	if rawErr, ok := envelope["error"]; ok && truthy(rawErr) {
		return false, &streamFailure{
			// The reference stringifies the error dict with Python's repr; this
			// uses compact JSON instead (wording divergence, same information).
			content: "Error: " + compactJSON(rawErr),
			err:     errors.New(compactJSON(rawErr)),
		}
	}

	var chunk wireStreamChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return false, &streamFailure{
			content: "Error calling LLM: " + err.Error(),
			err:     err,
		}
	}
	aggregate.add(chunk, ctx, events)
	return false, nil
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// ---------------------------------------------------------------------------
// Stream chunks
// ---------------------------------------------------------------------------

type wireStreamDelta struct {
	Content          json.RawMessage   `json:"content"`
	ReasoningContent json.RawMessage   `json:"reasoning_content"`
	Reasoning        json.RawMessage   `json:"reasoning"`
	ToolCalls        []json.RawMessage `json:"tool_calls"`
	FunctionCall     json.RawMessage   `json:"function_call"`
}

type wireStreamChoice struct {
	Delta        wireStreamDelta `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
}

type wireStreamChunk struct {
	Choices    []wireStreamChoice `json:"choices"`
	Usage      json.RawMessage    `json:"usage"`
	Content    json.RawMessage    `json:"content"`
	OutputText json.RawMessage    `json:"output_text"`
}

// toolCallFragment is one streamed tool-call delta.
type toolCallFragment struct {
	index     int
	id        string
	name      string
	arguments string
}

// parseToolCallDelta mirrors the delta handling of _accum_tc and the
// on_tool_call_delta callback (openai_compat_provider.py:1696-1720, 2166-2187):
// a missing index falls back to the enumerate position and every string field
// defaults to "".
func parseToolCallDelta(raw json.RawMessage, fallbackIndex int) toolCallFragment {
	var parsed struct {
		Index    *int   `json:"index"`
		ID       string `json:"id"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	_ = json.Unmarshal(raw, &parsed)

	fragment := toolCallFragment{index: fallbackIndex, id: parsed.ID, name: parsed.Function.Name}
	if parsed.Index != nil {
		fragment.index = *parsed.Index
	}
	if len(parsed.Function.Arguments) > 0 {
		if isJSONString(parsed.Function.Arguments) {
			fragment.arguments = jsonString(parsed.Function.Arguments)
		} else {
			fragment.arguments = string(bytes.TrimSpace(parsed.Function.Arguments))
		}
	}
	return fragment
}

// ---------------------------------------------------------------------------
// Aggregation
// ---------------------------------------------------------------------------

type toolCallBuffer struct {
	id        string
	name      string
	arguments strings.Builder
}

// streamAggregator accumulates deltas into the final response, mirroring
// _parse_chunks (openai_compat_provider.py:1688-1846).
type streamAggregator struct {
	content      strings.Builder
	reasoning    strings.Builder
	order        []int
	buffers      map[int]*toolCallBuffer
	finishReason string
	completed    bool
	usage        *core.Usage
}

func newStreamAggregator() *streamAggregator {
	return &streamAggregator{buffers: map[int]*toolCallBuffer{}}
}

func (a *streamAggregator) add(chunk wireStreamChunk, ctx context.Context, events chan<- core.StreamEvent) {
	if len(chunk.Choices) == 0 {
		// Usage-only chunks, and the bare content/output_text shape some
		// gateways stream (openai_compat_provider.py:1744-1755).
		if usage := extractUsage(chunk.Usage); usage != nil {
			a.usage = usage
			sendEvent(ctx, events, core.StreamEvent{Kind: core.StreamUsage, Usage: usage})
		}
		if text := extractTextContent(firstTruthyRaw(chunk.Content, chunk.OutputText)); text != "" {
			a.content.WriteString(text)
			sendEvent(ctx, events, core.StreamEvent{Kind: core.StreamText, Text: text})
		}
		return
	}

	choice := chunk.Choices[0]
	if choice.FinishReason != nil && *choice.FinishReason != "" {
		a.finishReason = *choice.FinishReason
		a.completed = true
	}

	delta := choice.Delta
	if text := extractTextContent(delta.Content); text != "" {
		a.content.WriteString(text)
		sendEvent(ctx, events, core.StreamEvent{Kind: core.StreamText, Text: text})
	}

	reasoning := extractTextContent(delta.ReasoningContent)
	if reasoning == "" {
		reasoning = extractTextContent(delta.Reasoning)
	}
	if reasoning == "" {
		// Mistral streams thinking inside the content array.
		reasoning = extractThinkingContent(delta.Content)
	}
	if reasoning != "" {
		a.reasoning.WriteString(reasoning)
		sendEvent(ctx, events, core.StreamEvent{Kind: core.StreamReasoning, Text: reasoning})
	}

	for i, raw := range delta.ToolCalls {
		a.accumulate(parseToolCallDelta(raw, i), ctx, events)
	}
	if truthy(delta.FunctionCall) {
		var legacy struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(delta.FunctionCall, &legacy)
		fragment := toolCallFragment{index: 0, name: legacy.Name}
		if isJSONString(legacy.Arguments) {
			fragment.arguments = jsonString(legacy.Arguments)
		} else {
			fragment.arguments = string(bytes.TrimSpace(legacy.Arguments))
		}
		a.accumulate(fragment, ctx, events)
	}

	if usage := extractUsage(chunk.Usage); usage != nil {
		a.usage = usage
		sendEvent(ctx, events, core.StreamEvent{Kind: core.StreamUsage, Usage: usage})
	}
}

// accumulate folds one fragment into its buffer and emits the delta event.
func (a *streamAggregator) accumulate(fragment toolCallFragment, ctx context.Context, events chan<- core.StreamEvent) {
	buffer, ok := a.buffers[fragment.index]
	if !ok {
		buffer = &toolCallBuffer{}
		a.buffers[fragment.index] = buffer
		a.order = append(a.order, fragment.index)
	}
	if fragment.id != "" {
		buffer.id = fragment.id
	}
	if fragment.name != "" {
		buffer.name = fragment.name
	}
	buffer.arguments.WriteString(fragment.arguments)

	sendEvent(ctx, events, core.StreamEvent{
		Kind:           core.StreamToolCall,
		Index:          fragment.index,
		ToolCallID:     fragment.id,
		ToolCallName:   fragment.name,
		ArgumentsDelta: fragment.arguments,
	})
}

// response builds the aggregated response for the StreamDone event.
func (a *streamAggregator) response(formats ...ToolCallFormat) *core.Response {
	format := ToolCallFormatAuto
	if len(formats) > 0 {
		format = formats[0]
	}
	content := a.content.String()

	seen := make(map[string]bool, len(a.order))
	calls := make([]core.ToolCall, 0, len(a.order))
	for _, index := range a.order {
		buffer := a.buffers[index]
		id := buffer.id
		// Duplicate or missing ids get a fresh short id so replayed tool
		// results cannot collide (openai_compat_provider.py:1816-1823).
		if id == "" || seen[id] {
			id = shortToolID()
		}
		seen[id] = true
		calls = append(calls, core.ToolCall{
			ID:        id,
			Name:      buffer.name,
			Arguments: parseToolArguments(json.RawMessage(buffer.arguments.String())),
		})
	}
	if len(calls) == 0 {
		content, calls = extractTextToolCallsWithFormat(content, format)
	}

	finish := a.finishReason
	if finish == "" {
		finish = "stop"
	}
	return &core.Response{
		Content:          content,
		HasContent:       content != "",
		ToolCalls:        calls,
		FinishReason:     core.FinishReason(finish),
		Usage:            a.usage,
		ReasoningContent: a.reasoning.String(),
	}
}
