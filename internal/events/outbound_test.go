package events

import (
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

func strp(s string) *string { return &s }

// TestProgressOfMatchesPythonIsinstance is the regression test for the
// inheritance trap.
//
// Python: `class FileEditEvent(ProgressEvent)` means
// isinstance(FileEditEvent(...), ProgressEvent) is True. Go has no
// inheritance, so the natural translation — a type assertion — is FALSE for
// exactly the case the channel dispatcher cares about. Both halves are
// asserted here: that ProgressOf accepts the subclass, and that the naive
// assertion does not, so nobody "simplifies" ProgressOf away later.
func TestProgressOfMatchesPythonIsinstance(t *testing.T) {
	edit := FileEditEvent{ProgressEvent: ProgressEvent{
		Content:        "edited main.go",
		FileEditEvents: []map[string]any{{"path": "main.go"}},
	}}

	var ev core.AgentEvent = edit

	got, ok := ProgressOf(ev)
	if !ok {
		t.Fatal("ProgressOf(FileEditEvent) = false; Python's isinstance would be True")
	}
	if got.Content != "edited main.go" {
		t.Errorf("ProgressOf content = %q, want %q", got.Content, "edited main.go")
	}
	if len(got.FileEditEvents) != 1 {
		t.Errorf("ProgressOf lost FileEditEvents: %v", got.FileEditEvents)
	}

	// The naive translation must NOT work, which is why ProgressOf exists.
	if _, naive := ev.(ProgressEvent); naive {
		t.Error("plain type assertion matched FileEditEvent; the trap documented in outbound.go no longer applies and ProgressOf should be reconsidered")
	}

	// The base type still works through both paths.
	base := ProgressEvent{Content: "plain"}
	var baseEv core.AgentEvent = base
	if g, ok := ProgressOf(baseEv); !ok || g.Content != "plain" {
		t.Errorf("ProgressOf(ProgressEvent) = (%q, %v)", g.Content, ok)
	}

	// A non-progress event must be rejected.
	if _, ok := ProgressOf(StreamDeltaEvent{Content: "x"}); ok {
		t.Error("ProgressOf accepted a StreamDeltaEvent")
	}
	if _, ok := ProgressOf(RetryWaitEvent{Content: "x"}); ok {
		t.Error("ProgressOf accepted a RetryWaitEvent")
	}
}

// TestEventContent pins _event_content (outbound_events.py:148) case by case.
func TestEventContent(t *testing.T) {
	cases := []struct {
		name  string
		event core.AgentEvent
		want  string
	}{
		{"progress", ProgressEvent{Content: "working"}, "working"},
		{"progress empty", ProgressEvent{}, ""},
		{"file edit uses embedded content", FileEditEvent{ProgressEvent{Content: "edited"}}, "edited"},
		{"retry wait", RetryWaitEvent{Content: "retrying"}, "retrying"},
		{"stream delta", StreamDeltaEvent{Content: "chunk"}, "chunk"},
		{"stream end", StreamEndEvent{Content: "final"}, "final"},
		{"streamed response", StreamedResponseEvent{}, ""},
		{"compaction started", ContextCompactionEvent{Phase: CompactionStarted}, "Compressing context\u2026"},
		{"compaction failed", ContextCompactionEvent{Phase: CompactionFailed}, "Unable to compact context."},
		{"compaction cancelled", ContextCompactionEvent{Phase: CompactionCancelled}, "Context compaction cancelled."},
		{"compaction succeeded", ContextCompactionEvent{Phase: CompactionSucceeded}, "Context compacted."},
		{"compaction unknown phase", ContextCompactionEvent{Phase: "weird"}, "Context compacted."},
		{"retry status has no content", RetryStatusEvent{State: RetryWaiting, Attempt: 1}, ""},
		{"recovery has no content", RecoveryStateEvent{Status: "s"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventContent(tc.event); got != tc.want {
				t.Errorf("eventContent = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEventContentUsesSingleEllipsis guards a specific byte.
//
// "Compressing context…" ends in U+2026, not three U+002E. A channel rendering
// this to a user would show a different string, and a test written with "..."
// would pass against the wrong implementation.
func TestEventContentUsesSingleEllipsis(t *testing.T) {
	got := eventContent(ContextCompactionEvent{Phase: CompactionStarted})
	want := "Compressing context\u2026"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if len([]rune(got)) != 20 {
		t.Errorf("rune length = %d, want 20 (a three-dot version would be 22)", len([]rune(got)))
	}
}

func TestOutboundMessageForEventDerivesContent(t *testing.T) {
	msg := OutboundMessageForEvent("telegram", "42", ProgressEvent{Content: "thinking"}, nil, nil)
	if msg.Content != "thinking" {
		t.Errorf("derived content = %q, want %q", msg.Content, "thinking")
	}
	if msg.Channel != "telegram" || msg.ChatID != "42" {
		t.Errorf("routing lost: %q/%q", msg.Channel, msg.ChatID)
	}
	if msg.Event == nil {
		t.Fatal("Event was not set")
	}
	if _, ok := ProgressOf(msg.Event); !ok {
		t.Error("Event did not round-trip as a progress event")
	}
	// dict(metadata or {}) never yields None.
	if msg.Metadata == nil {
		t.Error("Metadata is nil; the reference always produces a dict")
	}
}

func TestOutboundMessageForEventHonoursExplicitEmptyContent(t *testing.T) {
	// An explicit "" must NOT be replaced by the derived content: that is the
	// difference between "" and None in Python.
	msg := OutboundMessageForEvent("telegram", "42", ProgressEvent{Content: "derived"}, strp(""), nil)
	if msg.Content != "" {
		t.Errorf("content = %q, want an explicit empty string", msg.Content)
	}
}

func TestOutboundMessageForEventCopiesMetadata(t *testing.T) {
	src := map[string]any{"message_id": "1"}
	msg := OutboundMessageForEvent("telegram", "42", ProgressEvent{}, nil, src)
	src["message_id"] = "mutated"
	if msg.Metadata["message_id"] != "1" {
		t.Errorf("metadata was not copied: %v", msg.Metadata["message_id"])
	}
}

func TestReplaceOutboundEvent(t *testing.T) {
	original := core.OutboundMessage{
		Channel:  "telegram",
		ChatID:   "42",
		Content:  "old",
		Metadata: map[string]any{"k": "v"},
	}
	replaced := ReplaceOutboundEvent(original, StreamEndEvent{Content: "new"}, nil)
	if replaced.Content != "new" {
		t.Errorf("content = %q, want %q", replaced.Content, "new")
	}
	if _, ok := replaced.Event.(StreamEndEvent); !ok {
		t.Errorf("event = %T, want StreamEndEvent", replaced.Event)
	}
	// The original is untouched — Go structs are values.
	if original.Content != "old" || original.Event != nil {
		t.Errorf("original was mutated: %+v", original)
	}
	// dataclasses.replace shares the metadata reference, so this must too.
	replaced.Metadata["k"] = "changed"
	if original.Metadata["k"] != "changed" {
		t.Error("metadata was copied; the reference shares it via dataclasses.replace")
	}

	// An explicit content overrides the derived one.
	explicit := ReplaceOutboundEvent(original, StreamEndEvent{Content: "derived"}, strp("explicit"))
	if explicit.Content != "explicit" {
		t.Errorf("content = %q, want %q", explicit.Content, "explicit")
	}
}

// TestEventNameValues documents that nothing dispatches on these strings.
//
// The Python events carry no name attribute at all; the values exist so Go
// diagnostics and the bus subscription filter have something to match on.
func TestEventNameValues(t *testing.T) {
	cases := map[string]core.AgentEvent{
		"ProgressEvent":          ProgressEvent{},
		"FileEditEvent":          FileEditEvent{},
		"StreamDeltaEvent":       StreamDeltaEvent{},
		"StreamEndEvent":         StreamEndEvent{},
		"StreamedResponseEvent":  StreamedResponseEvent{},
		"ContextCompactionEvent": ContextCompactionEvent{},
		"RetryWaitEvent":         RetryWaitEvent{},
		"RetryStatusEvent":       RetryStatusEvent{},
		"RecoveryStateEvent":     RecoveryStateEvent{},
	}
	for want, ev := range cases {
		if got := ev.EventName(); got != want {
			t.Errorf("EventName = %q, want %q", got, want)
		}
	}
}
