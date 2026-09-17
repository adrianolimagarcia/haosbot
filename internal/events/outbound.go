package events

import "github.com/adrianolimagarcia/nanobot-go/internal/core"

// ProgressEvent is a progress notification for a channel or UI.
//
// Mirrors ProgressEvent (bus/outbound_events.py:24). All fields are optional
// upstream and the zero value here is therefore a valid, empty progress event.
type ProgressEvent struct {
	Content        string           `json:"content"`
	ToolHint       bool             `json:"tool_hint"`
	Reasoning      bool             `json:"reasoning"`
	ReasoningDelta bool             `json:"reasoning_delta"`
	ReasoningEnd   bool             `json:"reasoning_end"`
	StreamID       *string          `json:"stream_id"`
	ToolEvents     []map[string]any `json:"tool_events"`
	FileEditEvents []map[string]any `json:"file_edit_events"`
}

// EventName implements core.AgentEvent.
func (ProgressEvent) EventName() string { return "ProgressEvent" }

// progressView is the marker that stands in for Python subclassing.
//
// It has a VALUE receiver on purpose. FileEditEvent embeds ProgressEvent by
// value, so a value receiver is promoted into the method set of both
// FileEditEvent and *FileEditEvent; a pointer receiver would only reach the
// pointer form and the dispatch would silently miss the value case.
//
// Returning a copy is faithful, not sloppy: the Python events are frozen
// dataclasses, so a caller can never observe a difference between the original
// and the view.
func (e ProgressEvent) progressView() ProgressEvent { return e }

// FileEditEvent is file activity whose snapshot collection requires an
// interested consumer.
//
// TRAP — Python declares `class FileEditEvent(ProgressEvent)`, so
// `isinstance(e, ProgressEvent)` is TRUE for it. Go has no inheritance and a
// plain type assertion `e.(ProgressEvent)` would be FALSE, which would send
// every file-edit event down the "not a progress event" branch of the channel
// dispatcher without any error being raised. ProgressOf is the replacement and
// exists so that no call site has to remember this.
type FileEditEvent struct {
	ProgressEvent
}

// EventName implements core.AgentEvent.
//
// It reports the concrete type rather than the embedded one, so diagnostics
// name what was actually produced. Python has no equivalent method; this is a
// Go-side addition and nothing dispatches on its value.
func (FileEditEvent) EventName() string { return "FileEditEvent" }

// ProgressOf returns the ProgressEvent view of e.
//
// It matches Python's `isinstance(e, ProgressEvent)`, subclasses included, and
// is the ONLY correct way to ask that question in this port. Use it instead of
// a type assertion.
func ProgressOf(e core.AgentEvent) (ProgressEvent, bool) {
	if c, ok := e.(interface{ progressView() ProgressEvent }); ok {
		return c.progressView(), true
	}
	return ProgressEvent{}, false
}

// StreamDeltaEvent carries one incremental chunk of assistant text.
type StreamDeltaEvent struct {
	Content  string  `json:"content"`
	StreamID *string `json:"stream_id"`
}

// EventName implements core.AgentEvent.
func (StreamDeltaEvent) EventName() string { return "StreamDeltaEvent" }

// StreamEndEvent closes a stream.
//
// Resuming and MergeNext are not cosmetic: they tell the channel whether the
// final text continues a previously interrupted stream or starts a new message,
// which decides whether the client edits its existing message or sends another.
type StreamEndEvent struct {
	Content   string  `json:"content"`
	StreamID  *string `json:"stream_id"`
	Resuming  bool    `json:"resuming"`
	MergeNext bool    `json:"merge_next"`
}

// EventName implements core.AgentEvent.
func (StreamEndEvent) EventName() string { return "StreamEndEvent" }

// StreamedResponseEvent marks a response already delivered incrementally.
//
// The channel dispatcher uses it to decide NOT to send the message again: the
// text has already reached the user as deltas, so sending the final content
// would duplicate it.
type StreamedResponseEvent struct{}

// EventName implements core.AgentEvent.
func (StreamedResponseEvent) EventName() string { return "StreamedResponseEvent" }

var (
	_ core.AgentEvent = ProgressEvent{}
	_ core.AgentEvent = FileEditEvent{}
	_ core.AgentEvent = StreamDeltaEvent{}
	_ core.AgentEvent = StreamEndEvent{}
	_ core.AgentEvent = StreamedResponseEvent{}
)

// OutboundMessageForEvent builds an OutboundMessage carrying a typed event.
// Mirrors outbound_message_for_event (outbound_events.py:114).
//
// content is a pointer so that "caller supplied an explicit empty string" stays
// distinguishable from "derive the content from the event" — the same
// distinction Python draws between "" and None. Passing nil derives it.
func OutboundMessageForEvent(
	channel, chatID string,
	event core.AgentEvent,
	content *string,
	metadata map[string]any,
) core.OutboundMessage {
	text := eventContent(event)
	if content != nil {
		text = *content
	}
	return core.OutboundMessage{
		Channel: channel,
		ChatID:  chatID,
		Content: text,
		Event:   event,
		// Upstream writes dict(metadata or {}), which always yields a NEW dict
		// and never nil. The copy is reproduced because a caller that mutates
		// its own map afterwards must not retroactively change the message.
		Metadata: copyMetadata(metadata),
	}
}

// ReplaceOutboundEvent returns msg with a new event and content.
// Mirrors replace_outbound_event (outbound_events.py:133).
//
// The Metadata map is shared with msg rather than copied, matching
// dataclasses.replace, which carries the same reference across.
func ReplaceOutboundEvent(msg core.OutboundMessage, event core.AgentEvent, content *string) core.OutboundMessage {
	msg.Event = event
	if content != nil {
		msg.Content = *content
	} else {
		msg.Content = eventContent(event)
	}
	return msg
}

func copyMetadata(metadata map[string]any) map[string]any {
	out := make(map[string]any, len(metadata))
	for k, v := range metadata {
		out[k] = v
	}
	return out
}

// eventContent is _event_content (outbound_events.py:148).
//
// The "Compressing context…" string contains U+2026 HORIZONTAL ELLIPSIS, not
// three periods. It is user-visible text, so the exact code point matters.
func eventContent(event core.AgentEvent) string {
	// ProgressEvent first, because upstream's union test includes it and
	// ProgressOf covers the FileEditEvent subclass.
	if p, ok := ProgressOf(event); ok {
		return p.Content
	}
	switch e := event.(type) {
	case RetryWaitEvent:
		return e.Content
	case StreamDeltaEvent:
		return e.Content
	case StreamEndEvent:
		return e.Content
	case ContextCompactionEvent:
		switch e.Phase {
		case CompactionStarted:
			return "Compressing context\u2026"
		case CompactionFailed:
			return "Unable to compact context."
		case CompactionCancelled:
			return "Context compaction cancelled."
		}
		return "Context compacted."
	}
	// Upstream also matches UserInputEvent here. That event is not ported, so
	// this branch is unreachable rather than missing.
	return ""
}
