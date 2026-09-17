package compat

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/events"
)

// This file is deliberately self-contained: its own dumper
// (compat/python/dump_outbound_events.py) and its own loader. It shares no
// declarations with differential_test.go beyond repoRoot, because
// dump_reference.py and differential_test.go are edited concurrently by several
// agents and a read-modify-write race there would destroy work.
//
// Every expectation below comes from executing the frozen reference. Nothing is
// transcribed from documentation.

// outboundEventsDumperPath is the dumper this file drives.
func outboundEventsDumperPath(root string) string {
	return filepath.Join(root, "compat", "python", "dump_outbound_events.py")
}

// loadOutboundEventsDump runs the dumper and returns its parsed document.
func loadOutboundEventsDump(t *testing.T) map[string]any {
	t.Helper()
	root := repoRoot(t)
	python := filepath.Join(root, ".tools", "venv", "bin", "python")
	script := outboundEventsDumperPath(root)

	if _, err := os.Stat(python); err != nil {
		t.Skipf("SKIP: reference venv not present at %s — differential check not run", python)
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("SKIP: dumper missing at %s", script)
	}

	cmd := exec.Command(python, script)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("dumper failed: %v\nstderr:\n%s", err, stderr)
	}

	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("dumper output is not valid JSON: %v", err)
	}
	return doc
}

// outboundEventByName builds the Go event corresponding to a dumper case name.
func outboundEventByName(t *testing.T, name string) core.AgentEvent {
	t.Helper()
	switch name {
	case "ProgressEvent":
		return events.ProgressEvent{Content: "working"}
	case "ProgressEvent.empty":
		return events.ProgressEvent{}
	case "FileEditEvent":
		return events.FileEditEvent{ProgressEvent: events.ProgressEvent{
			Content:        "edited",
			FileEditEvents: []map[string]any{{"path": "a"}},
		}}
	case "FileEditEvent.empty":
		return events.FileEditEvent{}
	case "RetryWaitEvent":
		return events.RetryWaitEvent{Content: "retrying"}
	case "RetryWaitEvent.empty":
		return events.RetryWaitEvent{}
	case "RetryStatusEvent":
		return events.RetryStatusEvent{
			State: "waiting", Attempt: 1, MaxAttempts: nil, ErrorKind: "rate_limit",
		}
	case "RecoveryStateEvent":
		return events.RecoveryStateEvent{Status: "recovered", RecoveryID: "r1"}
	case "StreamDeltaEvent":
		return events.StreamDeltaEvent{Content: "chunk", StreamID: ptrString("s1")}
	case "StreamEndEvent":
		return events.StreamEndEvent{
			Content: "final", StreamID: ptrString("s1"), Resuming: true, MergeNext: true,
		}
	case "StreamedResponseEvent":
		return events.StreamedResponseEvent{}
	case "ContextCompactionEvent.started":
		return events.ContextCompactionEvent{CompactionID: "c1", Phase: events.CompactionStarted}
	case "ContextCompactionEvent.succeeded":
		return events.ContextCompactionEvent{CompactionID: "c1", Phase: events.CompactionSucceeded}
	case "ContextCompactionEvent.failed":
		return events.ContextCompactionEvent{CompactionID: "c1", Phase: events.CompactionFailed}
	case "ContextCompactionEvent.cancelled":
		return events.ContextCompactionEvent{CompactionID: "c1", Phase: events.CompactionCancelled}
	}
	t.Fatalf("no Go event mapped for dumper case %q", name)
	return nil
}

func ptrString(s string) *string { return &s }

// TestOutboundEventContentMatchesPython compares the text fallback that
// _event_content supplies for events a channel cannot render natively.
//
// The Go accessor is exercised through OutboundMessageForEvent with a nil
// content pointer, which is exactly the arm a non-streaming channel takes.
func TestOutboundEventContentMatchesPython(t *testing.T) {
	doc := loadOutboundEventsDump(t)
	raw, ok := doc["event_content"].([]any)
	if !ok {
		t.Fatal("event_content missing or not a list")
	}
	if len(raw) == 0 {
		t.Fatal("event_content is empty — a comparison over zero cases is not a pass")
	}

	for _, item := range raw {
		tc, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("event_content entry is not an object: %#v", item)
		}
		name, _ := tc["name"].(string)
		wantContent, _ := tc["content"].(string)
		wantName, _ := tc["event_name"].(string)

		ev := outboundEventByName(t, name)

		if got := ev.EventName(); got != wantName {
			t.Errorf("%s: EventName() = %q, want %q", name, got, wantName)
		}

		msg := events.OutboundMessageForEvent("telegram", "42", ev, nil, nil)
		if msg.Content != wantContent {
			t.Errorf("%s: derived content = %q, want %q", name, msg.Content, wantContent)
		}
	}
	t.Logf("event_content: %d cases compared", len(raw))
}

// TestOutboundEventContentSingleEllipsis pins the one non-ASCII literal in the
// fallback strings: U+2026, not three periods.
func TestOutboundEventContentSingleEllipsis(t *testing.T) {
	doc := loadOutboundEventsDump(t)
	raw, _ := doc["event_content"].([]any)
	for _, item := range raw {
		tc, _ := item.(map[string]any)
		content, _ := tc["content"].(string)
		if content == "Compressing context..." {
			t.Errorf("%s: reference uses U+2026, this is three ASCII periods", tc["name"])
		}
	}
	msg := events.OutboundMessageForEvent("telegram", "42",
		events.ContextCompactionEvent{CompactionID: "c1", Phase: events.CompactionStarted}, nil, nil)
	const want = "Compressing context\u2026"
	if msg.Content != want {
		t.Errorf("compaction content = %q, want %q", msg.Content, want)
	}
	if n := len([]rune(msg.Content)); n != 20 {
		t.Errorf("compaction content is %d runes, want 20", n)
	}
}

// TestProgressIsinstanceMatchesPython is the important one.
//
// FileEditEvent subclasses ProgressEvent in Python, so isinstance() accepts it.
// Go has no inheritance, so a plain type assertion to ProgressEvent would
// REJECT a FileEditEvent and silently route it down the wrong branch. This test
// asserts both halves: that ProgressOf accepts the subclass, and that the naive
// assertion does not — so nobody "simplifies" ProgressOf away later.
func TestProgressIsinstanceMatchesPython(t *testing.T) {
	doc := loadOutboundEventsDump(t)
	info, ok := doc["progress_isinstance"].(map[string]any)
	if !ok {
		t.Fatal("progress_isinstance missing")
	}

	wantFileEdit, _ := info["file_edit_is_progress"].(bool)
	if !wantFileEdit {
		t.Fatal("reference says FileEditEvent is NOT a ProgressEvent — the premise of this test changed")
	}

	// Half 1: the correct accessor accepts the subclass.
	if _, ok := events.ProgressOf(events.FileEditEvent{}); !ok {
		t.Error("ProgressOf(FileEditEvent) = false, want true (Python isinstance says True)")
	}
	if _, ok := events.ProgressOf(events.ProgressEvent{}); !ok {
		t.Error("ProgressOf(ProgressEvent) = false, want true")
	}

	// Half 2: the naive assertion does NOT. If this ever starts passing, Go
	// gained something inheritance-like and ProgressOf can be revisited.
	var ev core.AgentEvent = events.FileEditEvent{}
	if _, naive := ev.(events.ProgressEvent); naive {
		t.Error("a direct assertion to ProgressEvent now succeeds; ProgressOf's marker is no longer load-bearing")
	}

	// StreamEndEvent is not a ProgressEvent in Python, and must not be in Go.
	if wantStreamEnd, _ := info["stream_end_is_progress"].(bool); wantStreamEnd {
		t.Error("reference says StreamEndEvent IS a ProgressEvent — premise changed")
	}
	if _, ok := events.ProgressOf(events.StreamEndEvent{}); ok {
		t.Error("ProgressOf(StreamEndEvent) = true, want false")
	}

	// The MRO recorded by the reference is the reason this is needed at all.
	mro, _ := info["file_edit_mro"].([]any)
	if len(mro) < 3 {
		t.Errorf("expected FileEditEvent MRO to include its base, got %v", mro)
	}
	t.Logf("reference FileEditEvent MRO: %v", mro)
}

// TestOutboundMessageForEventMatchesPython covers the tri-state content
// argument: omitted, an explicit string, and an explicit empty string.
func TestOutboundMessageForEventMatchesPython(t *testing.T) {
	doc := loadOutboundEventsDump(t)
	raw, ok := doc["outbound_message_for_event"].([]any)
	if !ok {
		t.Fatal("outbound_message_for_event missing")
	}
	if len(raw) == 0 {
		t.Fatal("outbound_message_for_event is empty — zero cases is not a pass")
	}

	event := events.ProgressEvent{Content: "derived text"}
	for _, item := range raw {
		tc, _ := item.(map[string]any)
		name, _ := tc["name"].(string)
		wantContent, _ := tc["content"].(string)
		wantChannel, _ := tc["channel"].(string)
		wantChatID, _ := tc["chat_id"].(string)
		wantMeta, _ := tc["metadata"].(map[string]any)

		var contentPtr *string
		var meta map[string]any
		switch name {
		case "omitted":
		case "explicit":
			contentPtr = ptrString("explicit text")
		case "explicit_empty":
			contentPtr = ptrString("")
		case "metadata":
			meta = map[string]any{"message_id": "7"}
		default:
			t.Fatalf("unmapped arm %q", name)
		}

		msg := events.OutboundMessageForEvent("telegram", "42", event, contentPtr, meta)

		if msg.Content != wantContent {
			t.Errorf("%s: content = %q, want %q", name, msg.Content, wantContent)
		}
		if msg.Channel != wantChannel {
			t.Errorf("%s: channel = %q, want %q", name, msg.Channel, wantChannel)
		}
		if msg.ChatID != wantChatID {
			t.Errorf("%s: chat_id = %q, want %q", name, msg.ChatID, wantChatID)
		}
		if len(msg.Metadata) != len(wantMeta) {
			t.Errorf("%s: metadata = %v, want %v", name, msg.Metadata, wantMeta)
		}
		for k, v := range wantMeta {
			if got := msg.Metadata[k]; got != v {
				t.Errorf("%s: metadata[%q] = %v, want %v", name, k, got, v)
			}
		}
	}
	t.Logf("outbound_message_for_event: %d arms compared", len(raw))
}

// TestReplaceOutboundEventMatchesPython records the metadata aliasing and the
// explicit-empty-content behaviour.
func TestReplaceOutboundEventMatchesPython(t *testing.T) {
	doc := loadOutboundEventsDump(t)
	want, ok := doc["replace_outbound_event"].(map[string]any)
	if !ok {
		t.Fatal("replace_outbound_event missing")
	}

	base := core.OutboundMessage{
		Channel: "slack", ChatID: "9", Content: "old", Metadata: map[string]any{"k": "v"},
	}
	replaced := events.ReplaceOutboundEvent(base, events.StreamEndEvent{Content: "new"}, nil)
	explicitEmpty := events.ReplaceOutboundEvent(base, events.StreamEndEvent{Content: "new"}, ptrString(""))

	if got, w := replaced.Content, want["content"]; got != w {
		t.Errorf("content = %q, want %v", got, w)
	}
	if got, w := replaced.Channel, want["channel"]; got != w {
		t.Errorf("channel = %q, want %v", got, w)
	}
	if got, w := explicitEmpty.Content, want["explicit_empty_content"]; got != w {
		t.Errorf("explicit empty content = %q, want %v", got, w)
	}
	if got, w := base.Content, want["base_content_unchanged"]; got != w {
		t.Errorf("base content mutated: %q, want %v", got, w)
	}
	if wantShared, _ := want["metadata_is_shared"].(bool); wantShared {
		if replaced.Metadata == nil || replaced.Metadata["k"] != "v" {
			t.Error("metadata not carried over")
		}
	}
}

// TestNotificationDeliveryMatchesPython compares the full cross product of
// event type x channel x publish_lifecycle.
func TestNotificationDeliveryMatchesPython(t *testing.T) {
	doc := loadOutboundEventsDump(t)
	section, ok := doc["notification_delivery"].(map[string]any)
	if !ok {
		t.Fatal("notification_delivery missing")
	}
	cases, _ := section["cases"].([]any)
	if len(cases) == 0 {
		t.Fatal("notification_delivery has zero cases — not a pass")
	}

	byName := map[string]core.AgentEvent{
		"ProgressEvent":          events.ProgressEvent{},
		"FileEditEvent":          events.FileEditEvent{},
		"StreamDeltaEvent":       events.StreamDeltaEvent{},
		"StreamEndEvent":         events.StreamEndEvent{},
		"StreamedResponseEvent":  events.StreamedResponseEvent{},
		"ContextCompactionEvent": events.ContextCompactionEvent{},
		"RetryWaitEvent":         events.RetryWaitEvent{},
		"RetryStatusEvent":       events.RetryStatusEvent{},
		"RecoveryStateEvent":     events.RecoveryStateEvent{},
	}

	for _, item := range cases {
		tc, _ := item.(map[string]any)
		typeName, _ := tc["type"].(string)
		channel, _ := tc["channel"].(string)
		publish, _ := tc["publish_lifecycle"].(bool)
		want, _ := tc["deliverable"].(bool)

		ev, ok := byName[typeName]
		if !ok {
			t.Fatalf("no Go event for type %q", typeName)
		}
		got := events.NotificationIsDeliverable(reflect.TypeOf(ev), channel, publish)
		if got != want {
			t.Errorf("%s on %q publish_lifecycle=%v = %v, want %v",
				typeName, channel, publish, got, want)
		}
	}

	// The audience table itself must match key for key, including the fact that
	// FileEditEvent needs its own entry despite being a ProgressEvent subclass.
	audiences, _ := section["audiences"].(map[string]any)
	if len(audiences) == 0 {
		t.Fatal("audiences table is empty")
	}
	for name, want := range audiences {
		ev, ok := byName[name]
		if !ok {
			t.Fatalf("audience table names an unmapped type %q", name)
		}
		// Recover the audience by probing the policy at a channel that only
		// accepts one class, rather than exporting the table.
		gotLifecycle := events.NotificationIsDeliverable(reflect.TypeOf(ev), "telegram", true)
		gotNoLifecycle := events.NotificationIsDeliverable(reflect.TypeOf(ev), "telegram", false)
		switch want {
		case "lifecycle":
			if !gotLifecycle || gotNoLifecycle {
				t.Errorf("%s: audience %v does not follow publish_lifecycle (true=%v false=%v)",
					name, want, gotLifecycle, gotNoLifecycle)
			}
		case "channel":
			if !gotLifecycle || !gotNoLifecycle {
				t.Errorf("%s: audience %v should always be deliverable (true=%v false=%v)",
					name, want, gotLifecycle, gotNoLifecycle)
			}
		case "interactive":
			if gotLifecycle || gotNoLifecycle {
				t.Errorf("%s: audience %v should be denied on telegram (true=%v false=%v)",
					name, want, gotLifecycle, gotNoLifecycle)
			}
		default:
			t.Fatalf("unknown audience %q", want)
		}
	}
	t.Logf("notification_delivery: %d cases compared, %d audience entries", len(cases), len(audiences))
}
