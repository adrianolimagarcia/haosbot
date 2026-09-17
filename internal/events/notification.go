package events

import (
	"reflect"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// NotificationAudience is where a notification is allowed to go.
type NotificationAudience string

const (
	// AudienceChannel means every channel may receive it.
	AudienceChannel NotificationAudience = "channel"
	// AudienceLifecycle means only channels that opted into lifecycle output
	// may receive it.
	AudienceLifecycle NotificationAudience = "lifecycle"
	// AudienceInteractive means only an interactive surface (the WebUI
	// websocket) may receive it.
	AudienceInteractive NotificationAudience = "interactive"
)

// notificationAudiences is NOTIFICATION_AUDIENCES
// (bus/notification_delivery.py:16).
//
// The key is the concrete type, not the EventName string, because that is what
// upstream keys on and because the distinction matters here: FileEditEvent
// needs its own entry even though Python treats it as a ProgressEvent subclass.
// A dict lookup on the class does not follow the MRO, and neither does this
// map — so the two agree, including on the fact that an event absent from the
// table is not deliverable at all.
var notificationAudiences = map[reflect.Type]NotificationAudience{
	reflect.TypeOf(ProgressEvent{}):          AudienceLifecycle,
	reflect.TypeOf(FileEditEvent{}):          AudienceLifecycle,
	reflect.TypeOf(StreamDeltaEvent{}):       AudienceChannel,
	reflect.TypeOf(StreamEndEvent{}):         AudienceChannel,
	reflect.TypeOf(ContextCompactionEvent{}): AudienceChannel,
	reflect.TypeOf(RetryWaitEvent{}):         AudienceLifecycle,
	reflect.TypeOf(RecoveryStateEvent{}):     AudienceInteractive,
	reflect.TypeOf(RetryStatusEvent{}):       AudienceInteractive,
}

// NotificationIsDeliverable admits operation notifications to a channel only by
// explicit policy. Mirrors notification_is_deliverable
// (bus/notification_delivery.py:28).
//
// The default is DENY. An event type that is not in the table is not
// deliverable, so adding a new event does not silently start broadcasting it to
// every channel — the omission is visible as "it never arrives", which is the
// safe direction to fail in.
func NotificationIsDeliverable(eventType reflect.Type, channel string, publishLifecycle bool) bool {
	// Go callers may hold a pointer to an event where Python would hold the
	// instance itself. Normalising the pointer away keeps the table's
	// value-typed keys authoritative, so Deliverable[*ProgressEvent] and
	// Deliverable[ProgressEvent] cannot disagree.
	for eventType != nil && eventType.Kind() == reflect.Pointer {
		eventType = eventType.Elem()
	}
	audience, ok := notificationAudiences[eventType]
	if !ok {
		return false
	}
	switch audience {
	case AudienceLifecycle:
		return publishLifecycle
	case AudienceInteractive:
		// RetryStatusEvent additionally requires publish_lifecycle, so an
		// interactive surface can suppress retry noise without losing the rest
		// of the interactive events.
		return channel == "websocket" &&
			(eventType != reflect.TypeOf(RetryStatusEvent{}) || publishLifecycle)
	default:
		return true
	}
}

// Deliverable is the type-safe form of NotificationIsDeliverable, for callers
// that know the event type statically.
//
// It exists because the reference signature takes a type rather than an
// instance: producers use it to skip EXPENSIVE WORK before building an event
// nobody can receive (see FileEditEvent, whose snapshot collection is the whole
// reason the check exists). Constructing the event first and testing it
// afterwards would defeat the purpose.
func Deliverable[T core.AgentEvent](channel string, publishLifecycle bool) bool {
	var zero T
	return NotificationIsDeliverable(reflect.TypeOf(zero), channel, publishLifecycle)
}
