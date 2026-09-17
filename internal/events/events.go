// Package events ports the typed runtime notifications of the Python
// reference: nanobot/events.py and nanobot/bus/outbound_events.py.
//
// SCOPE — what is here and what is deliberately not.
//
// Ported: ContextCompactionEvent, RetryWaitEvent, RetryStatusEvent,
// RecoveryStateEvent, ProgressEvent, FileEditEvent, StreamDeltaEvent,
// StreamEndEvent, StreamedResponseEvent, and the three helpers that build and
// rewrite an OutboundMessage for an event.
//
// NOT ported, because the subsystems that produce them do not exist in this
// port yet: TurnEndEvent, GoalStatusEvent, GoalStateSyncEvent,
// SessionUpdatedEvent, UserInputEvent, RuntimeModelUpdatedEvent,
// TurnModelUpdatedEvent, and EventSink/NO_EVENTS. They are listed here rather
// than silently omitted so the gap is visible; adding them is part of the
// WebUI, goal-state and injection work, not of the event plumbing itself.
//
// The one behavioural consequence of the omission is in eventContent: upstream
// also matches UserInputEvent there, which cannot be constructed in this port.
package events

import "github.com/adrianolimagarcia/nanobot-go/internal/core"

// ContextCompactionPhase is the phase of a context-compaction lifecycle.
type ContextCompactionPhase string

const (
	CompactionStarted   ContextCompactionPhase = "started"
	CompactionSucceeded ContextCompactionPhase = "succeeded"
	CompactionFailed    ContextCompactionPhase = "failed"
	CompactionCancelled ContextCompactionPhase = "cancelled"
)

// ContextCompactionEvent reports progress of a context-compaction operation.
type ContextCompactionEvent struct {
	CompactionID string                 `json:"compaction_id"`
	Phase        ContextCompactionPhase `json:"phase"`
}

// EventName implements core.AgentEvent.
func (ContextCompactionEvent) EventName() string { return "ContextCompactionEvent" }

// RetryWaitEvent announces that a retry delay is about to be waited out.
type RetryWaitEvent struct {
	Content string `json:"content"`
}

// EventName implements core.AgentEvent.
func (RetryWaitEvent) EventName() string { return "RetryWaitEvent" }

// RetryState is the lifecycle state of one model-request retry chain.
type RetryState string

const (
	RetryWaiting   RetryState = "waiting"
	RetryRecovered RetryState = "recovered"
	RetryCleared   RetryState = "cleared"
	RetryExhausted RetryState = "exhausted"
)

// RetryStatusEvent is a sanitized retry lifecycle for one model request chain.
//
// "Sanitized" is load-bearing upstream: the event carries a classified
// error_kind rather than the provider's raw error text, so a channel or UI
// cannot leak credentials or internal URLs that appear in provider errors.
type RetryStatusEvent struct {
	State       RetryState `json:"state"`
	Attempt     int        `json:"attempt"`
	MaxAttempts *int       `json:"max_attempts"`
	ErrorKind   string     `json:"error_kind"`
	NextRetryAt *float64   `json:"next_retry_at"`
}

// EventName implements core.AgentEvent.
func (RetryStatusEvent) EventName() string { return "RetryStatusEvent" }

// RecoveryStateEvent reports a content-recovery lifecycle transition.
type RecoveryStateEvent struct {
	Status      string  `json:"status"`
	RecoveryID  string  `json:"recovery_id"`
	Reason      *string `json:"reason"`
	Attempts    int     `json:"attempts"`
	CanContinue *bool   `json:"can_continue"`
}

// EventName implements core.AgentEvent.
func (RecoveryStateEvent) EventName() string { return "RecoveryStateEvent" }

// Compile-time proof that every event satisfies the shared interface.
var (
	_ core.AgentEvent = ContextCompactionEvent{}
	_ core.AgentEvent = RetryWaitEvent{}
	_ core.AgentEvent = RetryStatusEvent{}
	_ core.AgentEvent = RecoveryStateEvent{}
)
