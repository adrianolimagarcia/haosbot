// ChannelManager outbound path (P0.5).
//
// Port of nanobot/channels/manager.py at upstream 1bb712d3:
//
//	_should_suppress_outbound  :698-719
//	_fingerprint_content       :683-686
//	_remember_origin_reply_fingerprint :688-696
//	_cancel_outbound           :721-726
//	_queue_outbound            :728-759
//	_dispatch_outbound         :761-765
//	_dispatch_outbound_loop    :767-850
//	_send_reasoning_delta      :852-863
//	_send_reasoning_end        :865-875
//	_send_stream_event         :877-906
//	_send_once                 :908-931
//	_coalesce_stream_deltas    :933-996
//	_send_with_retry           :998-1056
//
// CONCURRENCY — how asyncio maps onto goroutines. See also Appendix A of
// docs/spec-channels.md.
//
//   - The dispatcher loop is one goroutine reading the bus. Python's
//     `asyncio.wait_for(self.bus.consume_outbound(), timeout=1.0)` becomes a
//     1s child deadline; an expired deadline is a housekeeping tick, not a
//     failure, so it continues the loop (manager.py:847-848).
//   - `_outbound_slots` (256) and `_outbound_sends` (32) are buffered channels
//     used as semaphores, which is the standard Go spelling of
//     asyncio.Semaphore.
//   - Each `_queue_outbound` call spawns one goroutine for `send()`. It first
//     waits for its predecessor for the same (channel, chat_id), then takes a
//     send slot. This reproduces Python's per-destination FIFO with global
//     concurrency.
//   - `asyncio.shield` (manager.py:743) has NO Go analogue. Its purpose there
//     is to stop cancellation of message N from propagating into message N-1,
//     which N is merely waiting on. This port gets the same guarantee
//     structurally: the predecessor runs in its own goroutine and is cancelled
//     only by an explicit cancelOutbound, never by a successor's cancellation.
//     The one observable difference is that cancelling a WAITING successor in
//     Python also cancels the shield future; here the successor simply returns
//     from its select. Both leave the predecessor running to completion.
//   - `asyncio.CancelledError` becomes context cancellation. Because Go's
//     cancellation is cooperative, every await point of the reference has an
//     explicit select on the task context: the FIFO wait, the send-slot
//     acquisition and the retry sleep.
//   - `task.add_done_callback(finished)` (manager.py:751-759) becomes a deferred
//     cleanup that runs on every exit path, including a panic in a channel.
package channels

import (
	"container/list"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/events"
	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// ErrNilChannel is returned when an operation is handed a nil channel.
//
// It has no Python counterpart — the reference would raise AttributeError on the
// first attribute access — and exists so a wiring mistake is reported instead of
// panicking inside a dispatch goroutine.
var ErrNilChannel = errors.New("channels: nil channel")

// ---------------------------------------------------------------------------
// Retry schedule (manager.py:60)
// ---------------------------------------------------------------------------

// SendRetryDelays returns the backoff schedule for a failed send. Mirrors the
// module constant _SEND_RETRY_DELAYS = (1, 2, 4) at manager.py:60.
//
// A fresh slice is returned so a caller cannot mutate the schedule.
func SendRetryDelays() []time.Duration {
	return []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
}

func defaultRetryDelaysNanos() []int64 {
	d := SendRetryDelays()
	nanos := make([]int64, len(d))
	for i, v := range d {
		nanos[i] = int64(v)
	}
	return nanos
}

// SendRetryDelay returns the delay that precedes retry attempt n (1-based).
//
// This is `_SEND_RETRY_DELAYS[min(attempt - 1, len(_SEND_RETRY_DELAYS) - 1)]`
// (manager.py:1043): attempt 1 -> 1s, 2 -> 2s, 3 -> 4s, 4 -> 4s, ... The clamp
// is by INDEX, so the last entry repeats rather than the schedule growing.
func SendRetryDelay(attempt int) time.Duration {
	return retryDelayAt(SendRetryDelays(), attempt)
}

func retryDelayAt(delays []time.Duration, attempt int) time.Duration {
	if len(delays) == 0 {
		return 0
	}
	i := attempt - 1
	if i < 0 {
		i = 0
	}
	if i >= len(delays) {
		i = len(delays) - 1
	}
	return delays[i]
}

func (m *ChannelManager) retryDelay(attempt int) time.Duration {
	if len(m.retryDelays) == 0 {
		return 0
	}
	i := attempt - 1
	if i < 0 {
		i = 0
	}
	if i >= len(m.retryDelays) {
		i = len(m.retryDelays) - 1
	}
	return time.Duration(m.retryDelays[i])
}

// ---------------------------------------------------------------------------
// Outbound task bookkeeping (_outbound_tasks, _outbound_tails, semaphores)
// ---------------------------------------------------------------------------

// outboundKey is the `(msg.channel, msg.chat_id)` FIFO key (manager.py:738).
type outboundKey struct {
	channel string
	chatID  string
}

// outboundTask is one `send()` task: Python's asyncio.Task plus what Go needs
// to cancel it and to let its successor wait for it.
type outboundTask struct {
	key    outboundKey
	cancel context.CancelFunc
	// done closes when the task body has finished, which is when Python's
	// `task.done()` becomes true and when `asyncio.gather(previous)` resolves.
	done chan struct{}
}

type outboundState struct {
	mu    sync.Mutex
	tasks map[*outboundTask]struct{}
	tails map[outboundKey]*outboundTask

	// slots is `_outbound_slots` (manager.py:144) and sends is
	// `_outbound_sends` (:145). Both are buffered channels used as semaphores.
	slots chan struct{}
	sends chan struct{}
}

func (o *outboundState) init(pending, concurrency int) {
	o.tasks = map[*outboundTask]struct{}{}
	o.tails = map[outboundKey]*outboundTask{}
	o.slots = make(chan struct{}, pending)
	o.sends = make(chan struct{}, concurrency)
}

// outboundTaskCount reports the number of live outbound tasks. Python:
// `len(manager._outbound_tasks)`.
func (m *ChannelManager) outboundTaskCount() int {
	m.outbound.mu.Lock()
	defer m.outbound.mu.Unlock()
	return len(m.outbound.tasks)
}

// outboundTailCount reports the number of live FIFO tails. Python:
// `manager._outbound_tails`.
func (m *ChannelManager) outboundTailCount() int {
	m.outbound.mu.Lock()
	defer m.outbound.mu.Unlock()
	return len(m.outbound.tails)
}

// OutboundTaskCount reports how many outbound deliveries are currently
// registered, in flight or waiting on their destination's FIFO predecessor.
//
// It is the exported form of `len(manager._outbound_tails)`'s sibling
// `len(manager._outbound_tasks)` (manager.py:745), and exists so a gateway
// health endpoint — and the differential harness — can observe that shutdown
// really drained the outbound path.
func (m *ChannelManager) OutboundTaskCount() int { return m.outboundTaskCount() }

// OutboundTailCount reports how many destinations have a pending FIFO tail.
// Python: `len(manager._outbound_tails)`.
func (m *ChannelManager) OutboundTailCount() int { return m.outboundTailCount() }

// ---------------------------------------------------------------------------
// _queue_outbound (manager.py:728-759)
// ---------------------------------------------------------------------------

// QueueOutbound schedules one outbound message for delivery. Port of
// _queue_outbound (manager.py:728-759).
//
// Ordering and backpressure, exactly as the reference describes them:
//
//   - one of OutboundPendingLimit admission slots is taken first, so saturation
//     PAUSES the dispatcher rather than growing unbounded goroutines or dropping
//     messages;
//   - liveness is re-checked after admission: a message for a channel that is
//     stopping, or whose runtime has been replaced since it was dequeued, is
//     dropped and its slot released (manager.py:735-737);
//   - FIFO is per (channel, chat_id), NOT global: a slow destination cannot
//     block another one;
//   - at most OutboundConcurrency sends are in flight at once, and a retry wait
//     occupies only its own destination plus one send slot.
//
// Returns ctx.Err() if the caller is cancelled while waiting for an admission
// slot; the reference raises CancelledError at the same point.
func (m *ChannelManager) QueueOutbound(ctx context.Context, ch Channel, msg core.OutboundMessage) error {
	if ch == nil {
		return ErrNilChannel
	}

	// await self._outbound_slots.acquire()
	select {
	case m.outbound.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}

	// Re-check liveness after acquiring (manager.py:735-737).
	if m.isStopping(msg.Channel) || !m.isCurrentChannel(msg.Channel, ch) {
		<-m.outbound.slots
		return nil
	}

	key := outboundKey{channel: msg.Channel, chatID: msg.ChatID}

	// The task context is NOT derived from the caller's: Python creates the
	// task with `asyncio.create_task` (manager.py:747), so it survives the
	// dispatcher's cancellation and is stopped only by _cancel_outbound. That is
	// why _dispatch_outbound's finally block has to sweep explicitly (:765).
	taskCtx, cancel := context.WithCancel(context.Background())
	task := &outboundTask{key: key, cancel: cancel, done: make(chan struct{})}

	m.outbound.mu.Lock()
	previous := m.outbound.tails[key]
	m.outbound.tasks[task] = struct{}{}
	m.outbound.tails[key] = task
	m.outbound.mu.Unlock()

	go func() {
		// The done-callback (manager.py:751-759): release the slot, unlink the
		// tail, and drop the task. It must run on every exit path, including a
		// panic inside a channel's Send, so it is a defer.
		defer func() {
			m.outbound.mu.Lock()
			delete(m.outbound.tasks, task)
			if m.outbound.tails[key] == task {
				delete(m.outbound.tails, key)
			}
			m.outbound.mu.Unlock()
			<-m.outbound.slots
			// Closed last: Python's done-callback runs before the successor's
			// gather resolves, because `finished` was registered first.
			close(task.done)
		}()

		if previous != nil {
			// await asyncio.shield(asyncio.gather(previous, ...)). Waiting on the
			// predecessor must not be able to cancel it — see the package comment.
			select {
			case <-previous.done:
			case <-taskCtx.Done():
				return
			}
		}

		// async with self._outbound_sends:
		select {
		case m.outbound.sends <- struct{}{}:
		case <-taskCtx.Done():
			return
		}
		defer func() { <-m.outbound.sends }()

		m.sendWithRetry(taskCtx, ch, msg, nil)
	}()

	return nil
}

// isStopping reports `msg.channel in self._stopping_channels` (manager.py:735).
func (m *ChannelManager) isStopping(channel string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopping[channel]
}

// isCurrentChannel reports `self.channels.get(msg.channel) is channel`
// (manager.py:735).
//
// Python compares identity with `is`. Go compares the interface value, which is
// identity for the pointer-typed channels this port uses; a channel implemented
// as a non-pointer struct with value receivers would compare by value instead.
func (m *ChannelManager) isCurrentChannel(channel string, ch Channel) bool {
	current, ok := m.channel(channel)
	if !ok {
		return false
	}
	return sameChannel(current, ch)
}

// sameChannel is an identity comparison that cannot panic.
//
// `==` on interface values panics when the dynamic type is not comparable (a
// struct containing a slice or map). Python's `is` never fails, so the guard is
// the faithful translation: an uncomparable channel is treated as "not the same
// runtime" and the message is dropped, which fails in the safe direction.
func sameChannel(a, b Channel) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

// ---------------------------------------------------------------------------
// _cancel_outbound (manager.py:721-726)
// ---------------------------------------------------------------------------

// CancelOutbound cancels in-flight sends, for one channel or for all when
// channel is empty, and waits for them to finish. Port of _cancel_outbound
// (manager.py:721-726).
func (m *ChannelManager) CancelOutbound(ctx context.Context, channel string) {
	m.cancelOutbound(ctx, channel)
}

func (m *ChannelManager) cancelOutbound(_ context.Context, channel string) {
	m.outbound.mu.Lock()
	tasks := make([]*outboundTask, 0, len(m.outbound.tasks))
	for task := range m.outbound.tasks {
		if channel == "" || task.key.channel == channel {
			tasks = append(tasks, task)
		}
	}
	m.outbound.mu.Unlock()

	for _, task := range tasks {
		task.cancel()
	}
	// `await asyncio.gather(*tasks, return_exceptions=True)` — the reference
	// waits for every cancelled task, and swallows their errors.
	for _, task := range tasks {
		<-task.done
	}
}

// ---------------------------------------------------------------------------
// _dispatch_outbound / _dispatch_outbound_loop (manager.py:761-850)
// ---------------------------------------------------------------------------

// dispatchOutbound runs the loop and then cancels every outbound task, which is
// what _dispatch_outbound's finally block does (manager.py:761-765).
func (m *ChannelManager) dispatchOutbound(ctx context.Context) {
	defer m.cancelOutbound(context.Background(), "")
	m.dispatchOutboundLoop(ctx)
}

func (m *ChannelManager) dispatchOutboundLoop(ctx context.Context) {
	m.loggerOf().Info("Outbound dispatcher started")

	// Buffer for messages that could not be processed during delta coalescing,
	// because asyncio.Queue has no push_front (manager.py:771-773). It is FIFO:
	// `pending.pop(0)` takes from the front, so a boundary message pulled out of
	// the bus during coalescing is dispatched BEFORE anything that arrives after
	// it, preserving bus order.
	var pending []core.OutboundMessage

	for {
		// Python: `except asyncio.CancelledError: break` (manager.py:849-850).
		if ctx.Err() != nil {
			return
		}

		var msg core.OutboundMessage
		if len(pending) > 0 {
			msg = pending[0]
			pending = pending[1:]
		} else {
			pollCtx, cancel := context.WithTimeout(ctx, outboundPollInterval)
			next, err := m.bus.ConsumeOutbound(pollCtx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if errors.Is(err, context.DeadlineExceeded) {
					// `except asyncio.TimeoutError: continue` (manager.py:847-848).
					continue
				}
				// The Go bus can be closed, which the Python bus cannot. Exiting
				// the loop is the only sensible translation, and the deferred
				// sweep still cancels in-flight sends.
				m.loggerOf().Debug("Outbound dispatcher stopping", "reason", err)
				return
			}
			msg = next
		}

		next, err := m.dispatchOne(ctx, msg, pending)
		pending = next
		if err != nil {
			return
		}
	}
}

// dispatchOne applies every gate of _dispatch_outbound_loop's body to one
// message and queues it when it survives. It returns the updated pending buffer.
func (m *ChannelManager) dispatchOne(
	ctx context.Context,
	msg core.OutboundMessage,
	pending []core.OutboundMessage,
) ([]core.OutboundMessage, error) {
	event := normalizeEvent(msg.Event)

	// Reasoning gate (manager.py:786-801). Reasoning rides its own plugin
	// channel: it is delivered only when the destination channel exists and opts
	// in via show_reasoning. Channels without a low-emphasis UI affordance keep
	// the base no-op and the content silently drops here.
	if progress, ok := events.ProgressOf(event); ok &&
		(progress.ReasoningDelta || progress.ReasoningEnd || progress.Reasoning) {
		if ch, found := m.channel(msg.Channel); found {
			if _, _, reasoning := deliveryPolicy(ch); reasoning {
				if err := m.QueueOutbound(ctx, ch, msg); err != nil {
					return pending, err
				}
			}
		}
		return pending, nil
	}

	// Progress gate (manager.py:803-811).
	if progress, ok := events.ProgressOf(event); ok {
		if progress.ToolHint {
			if !m.shouldSendProgress(msg.Channel, true) {
				return pending, nil
			}
		} else if !m.shouldSendProgress(msg.Channel, false) {
			return pending, nil
		}
	}

	// RetryWaitEvent is an internal provider heartbeat and never reaches a
	// channel (manager.py:813-814).
	if _, isRetryWait := event.(events.RetryWaitEvent); isRetryWait {
		return pending, nil
	}

	// RuntimeModelUpdatedEvent addressed to a websocket channel that is not
	// registered (manager.py:816-821).
	if isRuntimeModelUpdatedEvent(event) &&
		msg.Channel == websocketChannelName {
		if _, found := m.channel(websocketChannelName); !found {
			return pending, nil
		}
	}

	// Delta coalescing (manager.py:823-828).
	if _, isDelta := event.(events.StreamDeltaEvent); isDelta {
		var extra []core.OutboundMessage
		msg, extra = m.CoalesceStreamDeltas(msg)
		pending = append(pending, extra...)
		event = normalizeEvent(msg.Event)
	}

	ch, found := m.channel(msg.Channel)
	if !found {
		// Unknown channel (manager.py:844-845): logged, dropped, and the
		// dispatcher stays alive.
		m.loggerOf().Warn("Unknown channel", "channel", msg.Channel)
		return pending, nil
	}

	// Duplicate suppression is scoped to a known source message so repeated
	// content from separate turns is still delivered (manager.py:832-842).
	switch event.(type) {
	case events.StreamDeltaEvent, events.StreamEndEvent, events.StreamedResponseEvent:
	default:
		if m.ShouldSuppressOutbound(msg) {
			m.loggerOf().Info("Suppressing duplicate outbound message",
				"channel", msg.Channel, "chat_id", msg.ChatID)
			return pending, nil
		}
	}

	if err := m.QueueOutbound(ctx, ch, msg); err != nil {
		return pending, err
	}
	return pending, nil
}

// runtimeModelUpdatedEventName is the EventName of RuntimeModelUpdatedEvent,
// which is NOT ported (see internal/events' package comment).
//
// The reference drops that event when it is addressed to a "websocket" channel
// that is not registered (manager.py:816-821). Implementing the gate by name
// keeps the behaviour for the day the event type is added — its EventName is
// its class name, which is the identity Python dispatches on — instead of
// leaving a silently missing branch. A Go-only divergence: a SUBCLASS of that
// event would satisfy Python's isinstance and not this string comparison, but
// no such subclass can exist before the base type does.
const runtimeModelUpdatedEventName = "RuntimeModelUpdatedEvent"

func isRuntimeModelUpdatedEvent(event core.AgentEvent) bool {
	return event != nil && event.EventName() == runtimeModelUpdatedEventName
}

// normalizeEvent maps a pointer-to-event onto its value form.
//
// Go callers may hold *T where Python holds T; both satisfy core.AgentEvent
// because the EventName methods have value receivers. Every dispatch decision
// below must treat the two identically, or a pointer event would silently fall
// through to the "plain message" arm and be delivered with `send` instead of
// the streaming/reasoning primitive. events.NotificationIsDeliverable
// normalises pointers for the same reason.
func normalizeEvent(event core.AgentEvent) core.AgentEvent {
	switch e := event.(type) {
	case *events.ProgressEvent:
		if e == nil {
			return nil
		}
		return *e
	case *events.FileEditEvent:
		if e == nil {
			return nil
		}
		return *e
	case *events.StreamDeltaEvent:
		if e == nil {
			return nil
		}
		return *e
	case *events.StreamEndEvent:
		if e == nil {
			return nil
		}
		return *e
	case *events.StreamedResponseEvent:
		if e == nil {
			return nil
		}
		return *e
	case *events.RetryWaitEvent:
		if e == nil {
			return nil
		}
		return *e
	}
	return event
}

// ---------------------------------------------------------------------------
// _send_once (manager.py:908-931)
// ---------------------------------------------------------------------------

// SendOnce performs one delivery attempt with no retry policy. Port of
// _send_once (manager.py:908-931).
//
// The dispatch table, in the reference's exact elif order:
//
//	ProgressEvent.reasoning_end    -> send_reasoning_end(chat_id, metadata, stream_id)
//	ProgressEvent.reasoning_delta  -> send_reasoning_delta(chat_id, content, metadata, stream_id)
//	ProgressEvent.reasoning        -> send_reasoning(msg)
//	ProgressEvent.file_edit_events -> send_file_edit_events(chat_id, events, metadata)
//	StreamDeltaEvent               -> send_delta(..., stream_end=False, resuming=False)
//	StreamEndEvent                 -> send_delta(..., stream_end=True, resuming=, merge_next=)
//	StreamedResponseEvent          -> nothing at all
//	anything else, including nil   -> send(msg)
//
// A ProgressEvent with none of the four flags set falls through to send(msg),
// which is what the reference's elif chain does.
//
// The `merge_next` signature probe (manager.py:888-899) has no Go analogue: the
// DeltaOptions set is fixed, so every DeltaSender receives MergeNext. That is a
// documented divergence in base.go (DeltaSender) and it is asserted by count in
// the differential test.
func (m *ChannelManager) SendOnce(ctx context.Context, ch Channel, msg core.OutboundMessage) error {
	if ch == nil {
		return ErrNilChannel
	}
	event := normalizeEvent(msg.Event)
	// Hand the normalised event on, so a channel reading msg.Event — including
	// Base.SendReasoning, which resolves the stream id through EventStreamID —
	// sees the same value form the dispatch table decided on. This is the
	// identity for every value event, which is what a Python producer builds.
	msg.Event = event

	if progress, ok := events.ProgressOf(event); ok {
		switch {
		case progress.ReasoningEnd:
			if sender, ok := ch.(ReasoningEndSender); ok {
				return sender.SendReasoningEnd(ctx, msg.ChatID, msg.Metadata, progress.StreamID)
			}
			return nil
		case progress.ReasoningDelta:
			if sender, ok := ch.(ReasoningDeltaSender); ok {
				return sender.SendReasoningDelta(ctx, msg.ChatID, msg.Content, msg.Metadata, progress.StreamID)
			}
			return nil
		case progress.Reasoning:
			return sendReasoning(ctx, ch, msg)
		case len(progress.FileEditEvents) > 0:
			if sender, ok := ch.(FileEditSender); ok {
				return sender.SendFileEditEvents(ctx, msg.ChatID, progress.FileEditEvents, msg.Metadata)
			}
			return nil
		}
	}

	switch e := event.(type) {
	case events.StreamDeltaEvent:
		return m.sendStreamEvent(ctx, ch, msg, DeltaOptions{
			StreamID:  e.StreamID,
			StreamEnd: false,
			Resuming:  false,
		})
	case events.StreamEndEvent:
		return m.sendStreamEvent(ctx, ch, msg, DeltaOptions{
			StreamID:  e.StreamID,
			StreamEnd: true,
			Resuming:  e.Resuming,
			MergeNext: e.MergeNext,
		})
	case events.StreamedResponseEvent:
		// The text already reached the user as deltas; sending it again would
		// duplicate it.
		return nil
	}

	return ch.Send(ctx, msg)
}

// ReasoningSender is implemented by every channel that embeds Base: it is
// BaseChannel.send_reasoning (base.py:201-223), the bridge that turns one-shot
// reasoning into a single delta plus an end marker.
//
// It is declared here rather than beside the other capability interfaces in
// base.go only because the manager is its first consumer. Unlike the other
// capability interfaces, Base DOES implement it, so a channel that embeds Base
// satisfies it without doing anything.
type ReasoningSender interface {
	SendReasoning(ctx context.Context, msg core.OutboundMessage) error
}

// sendReasoning is the send_reasoning bridge (base.py:201-223).
//
// A channel that embeds Base gets Base.SendReasoning promoted, so the interface
// assertion succeeds. A channel that does not is given the same bridge inline,
// because in Python every channel inherits it from BaseChannel.
func sendReasoning(ctx context.Context, ch Channel, msg core.OutboundMessage) error {
	if sender, ok := ch.(ReasoningSender); ok {
		return sender.SendReasoning(ctx, msg)
	}
	if msg.Content == "" {
		return nil
	}
	streamID := EventStreamID(msg.Event)
	if sender, ok := ch.(ReasoningDeltaSender); ok {
		if err := sender.SendReasoningDelta(ctx, msg.ChatID, msg.Content, msg.Metadata, streamID); err != nil {
			return err
		}
	}
	if sender, ok := ch.(ReasoningEndSender); ok {
		return sender.SendReasoningEnd(ctx, msg.ChatID, msg.Metadata, streamID)
	}
	return nil
}

// sendStreamEvent is _send_stream_event (manager.py:877-906).
//
// A channel without the SendDelta capability keeps BaseChannel's no-op, so the
// call is a no-op rather than an error.
func (m *ChannelManager) sendStreamEvent(
	ctx context.Context,
	ch Channel,
	msg core.OutboundMessage,
	opts DeltaOptions,
) error {
	sender, ok := ch.(DeltaSender)
	if !ok {
		return nil
	}
	return sender.SendDelta(ctx, msg.ChatID, msg.Content, msg.Metadata, opts)
}

// ---------------------------------------------------------------------------
// _coalesce_stream_deltas (manager.py:933-996)
// ---------------------------------------------------------------------------

// streamKey is the `(channel, chat_id, stream_id)` coalescing target
// (manager.py:946). The stream id is compared by VALUE, not by pointer, because
// Python compares `None` and `str` with ==.
type streamKey struct {
	channel  string
	chatID   string
	streamID *string
}

func sameStreamID(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (k streamKey) equals(channel, chatID string, streamID *string) bool {
	return k.channel == channel && k.chatID == chatID && sameStreamID(k.streamID, streamID)
}

// CoalesceStreamDeltas merges consecutive stream deltas for the same
// (channel, chat_id, stream_id). Port of _coalesce_stream_deltas
// (manager.py:933-996).
//
// It pulls messages off the bus with get_nowait until one does not belong to the
// same stream; that first non-matching message is returned as the boundary and
// the dispatcher re-queues it in its FIFO `pending` buffer, so bus order is
// preserved.
//
// The merge stops at:
//
//   - a message for another (channel, chat_id) or stream id;
//   - a StreamEndEvent, which is merged ONLY when its content is non-empty —
//     an empty end event is the boundary instead (manager.py:978), which is why
//     an end marker with no text does not swallow the deltas before it;
//   - any non-delta event, including a ProgressEvent or a plain message.
//
// The merged message keeps the FIRST message's channel, chat_id, metadata,
// media, buttons and reply_to, and takes the accumulated content. Its event is
// the first event, upgraded to the StreamEndEvent when one was absorbed. The
// returned message is a copy; metadata is shared, matching
// dataclasses.replace (manager.py:995).
func (m *ChannelManager) CoalesceStreamDeltas(first core.OutboundMessage) (core.OutboundMessage, []core.OutboundMessage) {
	firstEvent := normalizeEvent(first.Event)

	var firstStreamID *string
	if delta, ok := firstEvent.(events.StreamDeltaEvent); ok {
		firstStreamID = delta.StreamID
	}
	target := streamKey{channel: first.Channel, chatID: first.ChatID, streamID: firstStreamID}

	combined := first.Content
	var finalEvent core.AgentEvent
	if delta, ok := firstEvent.(events.StreamDeltaEvent); ok {
		finalEvent = delta
	} else {
		// The reference falls back to a StreamDeltaEvent carrying the same
		// (nil) stream id when the first message is not a delta. Reachable only
		// by calling the helper directly: the dispatcher guards with isinstance.
		finalEvent = events.StreamDeltaEvent{StreamID: firstStreamID}
	}

	var nonMatching []core.OutboundMessage
	for {
		next, ok := m.bus.TryConsumeOutbound()
		if !ok {
			break
		}
		nextEvent := normalizeEvent(next.Event)

		var nextStreamID *string
		switch e := nextEvent.(type) {
		case events.StreamDeltaEvent:
			nextStreamID = e.StreamID
		case events.StreamEndEvent:
			nextStreamID = e.StreamID
		}

		_, isDelta := nextEvent.(events.StreamDeltaEvent)
		endEvent, isEnd := nextEvent.(events.StreamEndEvent)
		sameTarget := target.equals(next.Channel, next.ChatID, nextStreamID)

		if sameTarget && (isDelta || (isEnd && next.Content != "")) {
			combined += next.Content
			if isEnd {
				finalEvent = events.StreamEndEvent{
					StreamID:  nextStreamID,
					Resuming:  endEvent.Resuming,
					MergeNext: endEvent.MergeNext,
				}
				break
			}
			continue
		}

		// The first non-matching message defines the coalescing boundary.
		nonMatching = append(nonMatching, next)
		break
	}

	merged := events.ReplaceOutboundEvent(first, finalEvent, &combined)
	return merged, nonMatching
}

// ---------------------------------------------------------------------------
// _send_with_retry (manager.py:998-1056)
// ---------------------------------------------------------------------------

// SendWithRetry delivers one message, retrying with exponential backoff. Port
// of _send_with_retry (manager.py:998-1056).
//
//	max_attempts = max(config.channels.send_max_retries, 1)
//	delays       = (1s, 2s, 4s), clamped by index
//
// Every failure consults channel.should_retry_send_error; a false return logs
// and gives up WITHOUT retrying (manager.py:1023-1030). Exhausting the budget
// also returns rather than raising (manager.py:1037-1042), so a failed delivery
// is a logged event, never a broken dispatcher.
//
// deadline, when set, replaces the attempt cap with a wall-clock bound: retry
// until that instant, and clamp each delay to the remaining time
// (manager.py:1032-1036, :1044-1045). The reference's only caller is the
// restart-notice path, which is not ported; the arm is implemented because it is
// part of this function's contract.
//
// Python re-raises CancelledError from the send and from the sleep; here a
// cancelled task context returns from both.
func (m *ChannelManager) SendWithRetry(
	ctx context.Context,
	ch Channel,
	msg core.OutboundMessage,
	deadline *time.Time,
) {
	maxAttempts := m.config.Channels.SendMaxRetries
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	attempt := 0
	for {
		attempt++

		err := m.SendOnce(ctx, ch, msg)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			// `except asyncio.CancelledError: raise`.
			return
		}
		if !shouldRetrySendError(ch, err) {
			m.loggerOf().Error("Send failed with a non-retryable error",
				"channel", msg.Channel, "error_type", typeName(err), "error", err)
			return
		}

		var exhausted bool
		if deadline == nil {
			exhausted = attempt >= maxAttempts
		} else {
			exhausted = !time.Now().Before(*deadline)
		}
		if exhausted {
			m.loggerOf().Error("Failed to send after all attempts",
				"channel", msg.Channel, "attempts", attempt, "error", err)
			return
		}

		delay := m.retryDelay(attempt)
		if deadline != nil {
			remaining := time.Until(*deadline)
			if remaining < 0 {
				remaining = 0
			}
			if delay > remaining {
				delay = remaining
			}
		}
		attemptLabel := strconv.Itoa(attempt)
		if deadline == nil {
			attemptLabel = strconv.Itoa(attempt) + "/" + strconv.Itoa(maxAttempts)
		}
		m.loggerOf().Warn("Send failed; retrying",
			"channel", msg.Channel,
			"attempt", attemptLabel,
			"error_type", typeName(err),
			"delay_ms", delay.Milliseconds())

		if !sleepContext(ctx, delay) {
			// `except asyncio.CancelledError: raise` around the sleep.
			return
		}
	}
}

// sendWithRetry is the internal entry point used by the outbound task, which
// already holds a send slot and never passes a deadline.
func (m *ChannelManager) sendWithRetry(
	ctx context.Context,
	ch Channel,
	msg core.OutboundMessage,
	deadline *time.Time,
) {
	m.SendWithRetry(ctx, ch, msg, deadline)
}

// sleepContext waits for d, or returns false when ctx is cancelled first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// typeName is the analogue of Python's `type(e).__name__`, which the retry log
// lines use (manager.py:1027, :1051).
func typeName(err error) string {
	if err == nil {
		return ""
	}
	t := reflect.TypeOf(err)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}

// ---------------------------------------------------------------------------
// _should_suppress_outbound (manager.py:698-719)
// ---------------------------------------------------------------------------

// ShouldSuppressOutbound reports whether a message duplicates one already sent
// for the same source message. Port of _should_suppress_outbound
// (manager.py:698-719).
//
// The predicate, in order:
//
//   - a ProgressEvent (FileEditEvent included, because it subclasses
//     ProgressEvent in Python) is NEVER suppressed;
//   - a content fingerprint that normalizes to nothing — empty or all
//     whitespace — is never suppressed, so an empty message is always delivered;
//   - with a non-empty STRING metadata["origin_message_id"], a repeat of the
//     fingerprint recorded for (channel, chat_id, origin_message_id) is
//     suppressed; otherwise the fingerprint is remembered;
//   - a non-empty STRING metadata["message_id"] records the fingerprint but
//     NEVER suppresses on its own.
//
// Note the asymmetry: only origin_message_id can suppress. A non-string or
// empty id is ignored entirely, which is what the reference's
// `isinstance(x, str) and x` test does.
func (m *ChannelManager) ShouldSuppressOutbound(msg core.OutboundMessage) bool {
	if _, isProgress := events.ProgressOf(normalizeEvent(msg.Event)); isProgress {
		return false
	}
	fingerprint := FingerprintContent(msg.Content)
	if fingerprint == "" {
		return false
	}

	if originID, ok := stringMetadata(msg.Metadata, "origin_message_id"); ok {
		key := fingerprintKey{channel: msg.Channel, chatID: msg.ChatID, id: originID}
		if m.fingerprints.get(key) == fingerprint {
			m.fingerprints.touch(key)
			return true
		}
		m.fingerprints.remember(key, fingerprint)
	}

	if messageID, ok := stringMetadata(msg.Metadata, "message_id"); ok {
		m.fingerprints.remember(
			fingerprintKey{channel: msg.Channel, chatID: msg.ChatID, id: messageID},
			fingerprint,
		)
	}

	return false
}

// stringMetadata reads a non-empty string value, reproducing
// `isinstance(metadata.get(k), str) and metadata.get(k)`.
func stringMetadata(metadata map[string]any, key string) (string, bool) {
	if metadata == nil {
		return "", false
	}
	value, ok := metadata[key].(string)
	if !ok || value == "" {
		return "", false
	}
	return value, true
}

// FingerprintContent is _fingerprint_content (manager.py:683-686):
//
//	normalized = " ".join(content.split())
//	sha1(normalized.encode("utf-8")).hexdigest() if normalized else ""
//
// `str.split()` with no argument splits on RUNS of Python whitespace and drops
// leading and trailing ones, so "  a \n b " and "a b" share a fingerprint. The
// split is done with textutil.PyIsSpace rather than strings.Fields: Go's
// unicode.IsSpace omits U+001C..U+001F, which Python's str.isspace() accepts,
// and a mismatch there would make two different texts share (or fail to share)
// a fingerprint. NBSP (U+00A0) is whitespace in both, and is covered by the
// table.
func FingerprintContent(content string) string {
	normalized := joinPySplit(content)
	if normalized == "" {
		return ""
	}
	sum := sha1.Sum([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// joinPySplit is `" ".join(s.split())`.
func joinPySplit(s string) string {
	var b strings.Builder
	first := true
	start := -1
	for i, r := range s {
		if textutil.PyIsSpace(r) {
			if start >= 0 {
				if !first {
					b.WriteByte(' ')
				}
				b.WriteString(s[start:i])
				first = false
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		if !first {
			b.WriteByte(' ')
		}
		b.WriteString(s[start:])
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Bounded fingerprint memory (_origin_reply_fingerprints)
// ---------------------------------------------------------------------------

// fingerprintKey is the OrderedDict key: `(channel, chat_id, id)`.
type fingerprintKey struct {
	channel string
	chatID  string
	id      string
}

// fingerprintStore is a bounded FIFO map: Python's
// `OrderedDict[tuple[str, str, str], str]` with
// `move_to_end` on access and `popitem(last=False)` on overflow
// (manager.py:148, :688-696).
//
// container/list is the stdlib ordered container; the map holds the list
// element so lookup and move-to-end are both O(1).
type fingerprintStore struct {
	mu    sync.Mutex
	max   int
	order *list.List
	index map[fingerprintKey]*list.Element
}

type fingerprintEntry struct {
	key         fingerprintKey
	fingerprint string
}

func newFingerprintStore(max int) *fingerprintStore {
	return &fingerprintStore{
		max:   max,
		order: list.New(),
		index: map[fingerprintKey]*list.Element{},
	}
}

// get returns the stored fingerprint, or "" when absent.
func (s *fingerprintStore) get(key fingerprintKey) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.index[key]
	if !ok {
		return ""
	}
	return el.Value.(*fingerprintEntry).fingerprint
}

// touch moves an existing key to the most-recent end (OrderedDict.move_to_end).
func (s *fingerprintStore) touch(key fingerprintKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.index[key]; ok {
		s.order.MoveToBack(el)
	}
}

// remember stores a fingerprint, moves the key to the end, and evicts from the
// front until the store is within its bound.
func (s *fingerprintStore) remember(key fingerprintKey, fingerprint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.index[key]; ok {
		el.Value.(*fingerprintEntry).fingerprint = fingerprint
		s.order.MoveToBack(el)
	} else {
		s.index[key] = s.order.PushBack(&fingerprintEntry{key: key, fingerprint: fingerprint})
	}
	for s.max > 0 && s.order.Len() > s.max {
		front := s.order.Front()
		if front == nil {
			break
		}
		s.order.Remove(front)
		delete(s.index, front.Value.(*fingerprintEntry).key)
	}
}

// len reports the number of remembered fingerprints.
func (s *fingerprintStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.order.Len()
}

// keys returns the keys oldest-first, which is the OrderedDict iteration order.
func (s *fingerprintStore) keys() []fingerprintKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]fingerprintKey, 0, s.order.Len())
	for el := s.order.Front(); el != nil; el = el.Next() {
		out = append(out, el.Value.(*fingerprintEntry).key)
	}
	return out
}

// Compile-time proof that *bus.Bus satisfies the manager's bus surface.
var _ OutboundBus = (*bus.Bus)(nil)
