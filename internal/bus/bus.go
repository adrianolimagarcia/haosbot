// Package bus decouples chat channels from the agent core.
//
// Mirrors nanobot/bus/queue.py:MessageBus at upstream 1bb712d3.
//
// Reference semantics worth noting: the Python bus uses unbounded
// asyncio.Queues, so producers never block. That is a real memory risk on the
// small devices this port targets, so the Go bus reproduces unbounded behavior
// by default (compatibility first) while offering an explicit capacity limit
// for constrained deployments.
package bus

import (
	"context"
	"errors"
	"sync"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// ErrClosed is returned when operating on a closed bus.
var ErrClosed = errors.New("bus: closed")

// ErrFull is returned when a bounded queue rejects a publish.
var ErrFull = errors.New("bus: queue full")

// Event is a typed runtime event published to local subscribers.
//
// The bus transports events without inspecting their fields; subscribers
// filter by type. Mirrors the AgentEvent base type in the Python bus.
//
// This is an ALIAS of core.AgentEvent, not a separate interface. Two identical
// interfaces would let an event satisfy one and not the other depending on
// which package a caller imported, which is a compile-time distinction with no
// meaning — the same event would be accepted by the bus and rejected by
// OutboundMessage.Event, or vice versa.
type Event = core.AgentEvent

// EventHandler receives a published event.
type EventHandler func(ctx context.Context, event Event)

// Options configures a Bus.
type Options struct {
	// MaxInbound caps pending inbound messages. Zero means unbounded,
	// matching the Python default. Set this on memory-constrained devices.
	MaxInbound int
	// MaxOutbound caps pending outbound messages. Zero means unbounded.
	MaxOutbound int
}

// Bus is an async message bus between channels and the agent core.
//
// The queue is mutex-protected rather than channel-backed because the
// reference semantics are unbounded and Go channels cannot be unbounded.
// Waiters are woken by closing and replacing a broadcast channel, which
// avoids the missed-wakeup hazard of a buffered signal channel shared by
// multiple consumers.
type Bus struct {
	mu       sync.Mutex
	inbound  []core.InboundMessage
	outbound []core.OutboundMessage
	wait     chan struct{}
	closed   bool

	maxInbound  int
	maxOutbound int

	handlers []*subscription
}

type subscription struct {
	active  bool
	handler EventHandler
}

// New creates a bus.
func New(opts Options) *Bus {
	return &Bus{
		wait:        make(chan struct{}),
		maxInbound:  opts.MaxInbound,
		maxOutbound: opts.MaxOutbound,
	}
}

// broadcastLocked wakes every waiter. Callers must hold b.mu.
func (b *Bus) broadcastLocked() {
	close(b.wait)
	b.wait = make(chan struct{})
}

// wake returns a channel closed on the next state change.
func (b *Bus) wake() (<-chan struct{}, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrClosed
	}
	return b.wait, nil
}

// ---------------------------------------------------------------------------
// Inbound
// ---------------------------------------------------------------------------

// PublishInbound queues a message from a channel to the agent.
func (b *Bus) PublishInbound(ctx context.Context, msg core.InboundMessage) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrClosed
	}
	if b.maxInbound > 0 && len(b.inbound) >= b.maxInbound {
		b.mu.Unlock()
		return ErrFull
	}
	b.inbound = append(b.inbound, msg)
	b.broadcastLocked()
	b.mu.Unlock()
	return ctx.Err()
}

// ConsumeInbound blocks until a message is available, ctx is done, or the bus
// closes.
func (b *Bus) ConsumeInbound(ctx context.Context) (core.InboundMessage, error) {
	for {
		b.mu.Lock()
		if len(b.inbound) > 0 {
			msg := b.inbound[0]
			b.inbound = b.inbound[1:]
			b.mu.Unlock()
			return msg, nil
		}
		if b.closed {
			b.mu.Unlock()
			return core.InboundMessage{}, ErrClosed
		}
		w := b.wait
		b.mu.Unlock()

		select {
		case <-w:
		case <-ctx.Done():
			return core.InboundMessage{}, ctx.Err()
		}
	}
}

// InboundSize reports pending inbound messages.
func (b *Bus) InboundSize() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.inbound)
}

// ---------------------------------------------------------------------------
// Outbound
// ---------------------------------------------------------------------------

// PublishOutbound queues a routed message for its channel.
func (b *Bus) PublishOutbound(ctx context.Context, msg core.OutboundMessage) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrClosed
	}
	if b.maxOutbound > 0 && len(b.outbound) >= b.maxOutbound {
		b.mu.Unlock()
		return ErrFull
	}
	b.outbound = append(b.outbound, msg)
	b.broadcastLocked()
	b.mu.Unlock()
	return ctx.Err()
}

// ConsumeOutbound blocks until a message is available, ctx is done, or the bus
// closes.
func (b *Bus) ConsumeOutbound(ctx context.Context) (core.OutboundMessage, error) {
	for {
		b.mu.Lock()
		if len(b.outbound) > 0 {
			msg := b.outbound[0]
			b.outbound = b.outbound[1:]
			b.mu.Unlock()
			return msg, nil
		}
		if b.closed {
			b.mu.Unlock()
			return core.OutboundMessage{}, ErrClosed
		}
		w := b.wait
		b.mu.Unlock()

		select {
		case <-w:
		case <-ctx.Done():
			return core.OutboundMessage{}, ctx.Err()
		}
	}
}

// TryConsumeOutbound takes the next outbound message without blocking.
//
// It is the port of the manager's direct `self.bus.outbound.get_nowait()` call
// (channels/manager.py:959), which delta coalescing uses to drain the messages
// that piled up behind a stream delta. `MessageBus.consume_outbound` has no
// non-blocking sibling upstream because Python reaches into the asyncio.Queue;
// a Go caller cannot, so the operation is exposed here.
//
// ok is false when the queue is empty OR the bus is closed. The reference
// distinguishes the two (QueueEmpty vs. a bus that never closes), but a closed
// bus has no pending messages to coalesce either way, so coalescing stops.
func (b *Bus) TryConsumeOutbound() (core.OutboundMessage, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.outbound) == 0 {
		return core.OutboundMessage{}, false
	}
	msg := b.outbound[0]
	b.outbound = b.outbound[1:]
	return msg, true
}

// OutboundSize reports pending outbound messages.
func (b *Bus) OutboundSize() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.outbound)
}

// ---------------------------------------------------------------------------
// Local event dispatch
// ---------------------------------------------------------------------------

// Subscribe registers an ordered, synchronously-invoked handler and returns an
// idempotent unsubscribe function. Mirrors MessageBus.subscribe.
func (b *Bus) Subscribe(handler EventHandler) func() {
	sub := &subscription{active: true, handler: handler}
	b.mu.Lock()
	b.handlers = append(b.handlers, sub)
	b.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			sub.active = false
			for i, h := range b.handlers {
				if h == sub {
					b.handlers = append(b.handlers[:i], b.handlers[i+1:]...)
					break
				}
			}
		})
	}
}

// Publish dispatches an event to local subscribers in registration order,
// awaiting each. A panicking handler is isolated so one bad subscriber cannot
// break delivery to the others; the Python version logs and continues, and
// this preserves that containment without a logger dependency.
func (b *Bus) Publish(ctx context.Context, event Event) {
	b.mu.Lock()
	handlers := make([]*subscription, len(b.handlers))
	copy(handlers, b.handlers)
	b.mu.Unlock()

	for _, h := range handlers {
		if !h.active {
			continue
		}
		func() {
			defer func() { _ = recover() }()
			h.handler(ctx, event)
		}()
	}
}

// Close stops the bus and unblocks all waiters.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	b.handlers = nil
	b.broadcastLocked()
}

// Closed reports whether the bus has been closed.
func (b *Bus) Closed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}
