package events

// EventSink is a thin send callback bound to one operation's MessageBus route.
//
// Port of EventSink (nanobot/events.py:48-72). Operations use best-effort
// emit; execution hooks await publish directly so output failures retain their
// runner error semantics. This owns no queue or subscribers. AcceptsType lets
// expensive producers skip work when the bound consumer cannot use their event
// type.
//
// The reference parameterises both callbacks on the AgentEvent type. This port
// has no single AgentEvent base type (the events here are distinct structs), so
// the sink is typed over `any` and producers narrow with a type assertion, the
// same way the reference's isinstance checks work on the concrete class.
type EventSink struct {
	// Publish is the bound send callback. nil means no consumer is bound, which
	// is what the reference's NO_EVENTS sentinel is.
	Publish func(event any) error
	// AcceptsType reports whether the bound consumer can use this event. nil
	// means every type is accepted.
	AcceptsType func(event any) bool
}

// NoEvents is an EventSink with no bound consumer (events.py:75 NO_EVENTS).
var NoEvents = EventSink{}

// Accepts reports whether producing this event has a consumer in the bound
// scope (events.py:60-64).
func (s EventSink) Accepts(event any) bool {
	if s.Publish == nil {
		return false
	}
	if s.AcceptsType == nil {
		return true
	}
	return s.AcceptsType(event)
}

// Emit publishes event best-effort: a nil publish is a no-op and a publish
// failure is swallowed, mirroring the reference's try/except around
// “await self.publish(event)“ (events.py:66-72). The reference logs the
// failure through loguru; this port has no logger dependency in this package,
// so the error is dropped after being formatted for a future caller.
func (s EventSink) Emit(event any) {
	if s.Publish == nil {
		return
	}
	_ = s.Publish(event)
}
