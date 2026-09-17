package events

import (
	"reflect"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// The expected column below is the reference's own output, dumped by running
// nanobot.bus.notification_delivery.notification_is_deliverable over the full
// cross product of event type x channel x publish_lifecycle. It is transcribed
// rather than recomputed so that a change to the Go policy has to disagree with
// the recorded Python answer.

func TestNotificationAudienceTable(t *testing.T) {
	want := map[reflect.Type]NotificationAudience{
		reflect.TypeOf(ProgressEvent{}):          AudienceLifecycle,
		reflect.TypeOf(FileEditEvent{}):          AudienceLifecycle,
		reflect.TypeOf(StreamDeltaEvent{}):       AudienceChannel,
		reflect.TypeOf(StreamEndEvent{}):         AudienceChannel,
		reflect.TypeOf(ContextCompactionEvent{}): AudienceChannel,
		reflect.TypeOf(RetryWaitEvent{}):         AudienceLifecycle,
		reflect.TypeOf(RecoveryStateEvent{}):     AudienceInteractive,
		reflect.TypeOf(RetryStatusEvent{}):       AudienceInteractive,
	}
	if len(notificationAudiences) != len(want) {
		t.Fatalf("table has %d entries, reference has %d", len(notificationAudiences), len(want))
	}
	for typ, audience := range want {
		got, ok := notificationAudiences[typ]
		if !ok {
			t.Errorf("%s is missing from the table", typ)
			continue
		}
		if got != audience {
			t.Errorf("%s -> %q, want %q", typ, got, audience)
		}
	}
}

func TestNotificationIsDeliverable(t *testing.T) {
	cases := []struct {
		eventType        core.AgentEvent
		channel          string
		publishLifecycle bool
		want             bool
	}{
		// lifecycle audience: follows publish_lifecycle on every channel.
		{ProgressEvent{}, "telegram", false, false},
		{ProgressEvent{}, "telegram", true, true},
		{ProgressEvent{}, "websocket", false, false},
		{ProgressEvent{}, "websocket", true, true},
		{ProgressEvent{}, "slack", false, false},
		{ProgressEvent{}, "slack", true, true},
		{FileEditEvent{}, "telegram", false, false},
		{FileEditEvent{}, "telegram", true, true},
		{FileEditEvent{}, "websocket", false, false},
		{FileEditEvent{}, "websocket", true, true},
		{FileEditEvent{}, "slack", false, false},
		{FileEditEvent{}, "slack", true, true},
		{RetryWaitEvent{}, "telegram", false, false},
		{RetryWaitEvent{}, "telegram", true, true},
		{RetryWaitEvent{}, "websocket", false, false},
		{RetryWaitEvent{}, "websocket", true, true},
		{RetryWaitEvent{}, "slack", false, false},
		{RetryWaitEvent{}, "slack", true, true},

		// channel audience: always deliverable.
		{StreamDeltaEvent{}, "telegram", false, true},
		{StreamDeltaEvent{}, "telegram", true, true},
		{StreamDeltaEvent{}, "websocket", false, true},
		{StreamDeltaEvent{}, "websocket", true, true},
		{StreamDeltaEvent{}, "slack", false, true},
		{StreamDeltaEvent{}, "slack", true, true},
		{StreamEndEvent{}, "telegram", false, true},
		{StreamEndEvent{}, "telegram", true, true},
		{StreamEndEvent{}, "websocket", false, true},
		{StreamEndEvent{}, "websocket", true, true},
		{StreamEndEvent{}, "slack", false, true},
		{StreamEndEvent{}, "slack", true, true},
		{ContextCompactionEvent{}, "telegram", false, true},
		{ContextCompactionEvent{}, "telegram", true, true},
		{ContextCompactionEvent{}, "websocket", false, true},
		{ContextCompactionEvent{}, "websocket", true, true},
		{ContextCompactionEvent{}, "slack", false, true},
		{ContextCompactionEvent{}, "slack", true, true},

		// interactive audience: websocket only.
		{RecoveryStateEvent{}, "telegram", false, false},
		{RecoveryStateEvent{}, "telegram", true, false},
		{RecoveryStateEvent{}, "websocket", false, true},
		{RecoveryStateEvent{}, "websocket", true, true},
		{RecoveryStateEvent{}, "slack", false, false},
		{RecoveryStateEvent{}, "slack", true, false},

		// RetryStatusEvent is interactive AND gated on publish_lifecycle.
		{RetryStatusEvent{}, "telegram", false, false},
		{RetryStatusEvent{}, "telegram", true, false},
		{RetryStatusEvent{}, "websocket", false, false},
		{RetryStatusEvent{}, "websocket", true, true},
		{RetryStatusEvent{}, "slack", false, false},
		{RetryStatusEvent{}, "slack", true, false},

		// Absent from the table: denied everywhere, both settings.
		{StreamedResponseEvent{}, "telegram", false, false},
		{StreamedResponseEvent{}, "telegram", true, false},
		{StreamedResponseEvent{}, "websocket", false, false},
		{StreamedResponseEvent{}, "websocket", true, false},
		{StreamedResponseEvent{}, "slack", false, false},
		{StreamedResponseEvent{}, "slack", true, false},
	}

	for _, tc := range cases {
		got := NotificationIsDeliverable(reflect.TypeOf(tc.eventType), tc.channel, tc.publishLifecycle)
		if got != tc.want {
			t.Errorf("%s on %q publish_lifecycle=%v = %v, want %v",
				tc.eventType.EventName(), tc.channel, tc.publishLifecycle, got, tc.want)
		}
	}
}

// TestNotificationDefaultIsDeny guards the safe direction of failure.
func TestNotificationDefaultIsDeny(t *testing.T) {
	if NotificationIsDeliverable(nil, "websocket", true) {
		t.Error("a nil event type was admitted")
	}
	if NotificationIsDeliverable(reflect.TypeOf(StreamedResponseEvent{}), "websocket", true) {
		t.Error("an event absent from the table was admitted")
	}
}

// TestDeliverableMatchesPointerAndValue pins the normalisation that keeps the
// generic helper from disagreeing with the reflect-based entry point.
func TestDeliverableMatchesPointerAndValue(t *testing.T) {
	if !Deliverable[ProgressEvent]("telegram", true) {
		t.Error("Deliverable[ProgressEvent] should be true")
	}
	if Deliverable[ProgressEvent]("telegram", false) {
		t.Error("Deliverable[ProgressEvent] should be false without publish_lifecycle")
	}
	// Pointer form must agree with the value form.
	if !NotificationIsDeliverable(reflect.TypeOf(&ProgressEvent{}), "telegram", true) {
		t.Error("*ProgressEvent was not normalised to ProgressEvent")
	}
	if Deliverable[*StreamDeltaEvent]("telegram", false) != Deliverable[StreamDeltaEvent]("telegram", false) {
		t.Error("pointer and value forms disagree")
	}
}
