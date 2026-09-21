package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
)

// This file implements the streaming half of the JSON-RPC binding:
// SendStreamingMessage and SubscribeToTask over Server-Sent Events.
//
// The agent loop exposes no intermediate deltas here — a turn runs to
// completion and publishes its result — so a stream carries the states the task
// actually passes through: WORKING when the turn is accepted, then the terminal
// state. That is exactly what the spec allows ("the stream MUST begin with the
// Task object, followed by zero or more update events"), and it means every
// stream for a task is fed from the same place the non-streaming handlers read:
// the published snapshots.

// streamSubscriberBuffer is how many events a slow subscriber may fall behind
// before it is dropped. A turn publishes two states, so this is generous; it
// exists so a subscriber that never reads cannot block the publisher.
const streamSubscriberBuffer = 16

// StreamResponse is the oneof a streaming JSON-RPC result carries. Only the
// Task member is produced: this agent reports a task's progress as whole task
// snapshots rather than as separate status/artifact deltas.
type StreamResponse struct {
	Task *Task `json:"task,omitempty"`
}

// taskEvent is one published snapshot with a monotonic sequence number. The
// sequence is what lets a subscriber that joined mid-flight skip the snapshots
// it has already emitted without comparing timestamps.
type taskEvent struct {
	seq  uint64
	task Task
}

// taskHub fans published snapshots out to every stream watching one task.
//
// It holds the latest snapshot as well as the subscribers so that subscribing is
// atomic with reading the current state: a stream that joins while a turn is
// finishing either sees the terminal state as its first event or is registered
// in time to receive it, and never both.
type taskHub struct {
	mu      sync.Mutex
	subs    map[*taskSub]struct{}
	last    Task
	hasLast bool
	seq     uint64
}

type taskSub struct {
	ch chan taskEvent
}

// hubFor returns the hub for a task, creating it on first use.
func (h *Handler) hubFor(id string) *taskHub {
	if val, ok := h.hubs.Load(id); ok {
		if hub, ok := val.(*taskHub); ok {
			return hub
		}
	}
	hub := &taskHub{subs: make(map[*taskSub]struct{})}
	actual, _ := h.hubs.LoadOrStore(id, hub)
	return actual.(*taskHub)
}

// broadcast delivers a published snapshot to every stream watching the task.
//
// A subscriber whose buffer is full is dropped rather than allowed to block the
// publisher: one stalled client must not hold up the turn, and the spec requires
// that closing one stream does not affect the others.
func (h *Handler) broadcast(task Task) {
	val, ok := h.hubs.Load(task.ID)
	if !ok {
		return
	}
	hub, ok := val.(*taskHub)
	if !ok {
		return
	}

	hub.mu.Lock()
	defer hub.mu.Unlock()

	hub.seq++
	hub.last = task
	hub.hasLast = true
	event := taskEvent{seq: hub.seq, task: task}

	for sub := range hub.subs {
		select {
		case sub.ch <- event:
		default:
			close(sub.ch)
			delete(hub.subs, sub)
		}
	}
}

// subscribe registers a stream for a task and returns the state to emit first.
// It reports false when the task is unknown, which is what SubscribeToTask turns
// into TaskNotFoundError.
func (h *Handler) subscribe(id string) (*taskSub, taskEvent, bool) {
	hub := h.hubFor(id)

	hub.mu.Lock()
	defer hub.mu.Unlock()

	sub := &taskSub{ch: make(chan taskEvent, streamSubscriberBuffer)}
	hub.subs[sub] = struct{}{}

	if hub.hasLast {
		return sub, taskEvent{seq: hub.seq, task: hub.last}, true
	}

	// A task stored without going through publish — a seed written straight into
	// the store — has no hub history, so fall back to the store rather than
	// reporting a task that exists as unknown.
	if task, ok := h.loadTask(id); ok {
		hub.seq++
		hub.last, hub.hasLast = *task, true
		return sub, taskEvent{seq: hub.seq, task: *task}, true
	}

	return sub, taskEvent{}, false
}

// unsubscribe removes a stream. It is safe to call after broadcast has already
// dropped the subscriber, which is why the membership check guards the close.
func (h *Handler) unsubscribe(id string, sub *taskSub) {
	val, ok := h.hubs.Load(id)
	if !ok {
		return
	}
	hub, ok := val.(*taskHub)
	if !ok {
		return
	}

	hub.mu.Lock()
	defer hub.mu.Unlock()

	if _, live := hub.subs[sub]; live {
		delete(hub.subs, sub)
		close(sub.ch)
	}
}

// ---------------------------------------------------------------------------
// Server-Sent Events
// ---------------------------------------------------------------------------

// sseWriter frames JSON-RPC responses as Server-Sent Events.
type sseWriter struct {
	w  http.ResponseWriter
	fl http.Flusher
	id any
}

// newSSEWriter commits the streaming response headers. It fails when the
// ResponseWriter cannot flush, because a stream that never flushes is not a
// stream: the client would see nothing until the turn finished.
func newSSEWriter(w http.ResponseWriter, id any) (*sseWriter, error) {
	fl, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("the response writer does not support flushing")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Ask intermediaries not to buffer; without it a proxy can hold every event
	// until the stream closes, which defeats the point of streaming.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	return &sseWriter{w: w, fl: fl, id: id}, nil
}

// writeTask emits one task snapshot as an SSE event.
func (s *sseWriter) writeTask(task Task) error {
	payload, err := json.Marshal(JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      s.id,
		Result:  StreamResponse{Task: &task},
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", payload); err != nil {
		return err
	}
	s.fl.Flush()
	return nil
}

// relayTask forwards published snapshots to the client until the task reaches a
// terminal state or the client goes away. Events newer than lastSeq are the only
// ones sent, so the snapshot a subscriber emitted on joining is never repeated.
func (h *Handler) relayTask(ctx context.Context, sse *sseWriter, sub *taskSub, lastSeq uint64) {
	for {
		select {
		case event, ok := <-sub.ch:
			if !ok {
				return
			}
			if event.seq <= lastSeq {
				continue
			}
			lastSeq = event.seq
			if err := sse.writeTask(event.task); err != nil {
				return
			}
			if event.task.terminal() {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// handleSendStreamingMessage runs a turn and streams the task states it passes
// through.
//
// Request validation happens before the stream is opened, so a bad request is
// answered with a plain JSON-RPC error rather than a stream the client cannot
// interpret. The subscription is taken before the first publish, so the WORKING
// state is delivered through the same path as every later one.
func (h *Handler) handleSendStreamingMessage(ctx context.Context, w http.ResponseWriter, req JSONRPCRequest) {
	plan, failure := h.prepareSend(req)
	if failure != nil {
		failure.write(w, req.ID)
		return
	}

	sub, first, _ := h.subscribe(plan.taskID)
	defer h.unsubscribe(plan.taskID, sub)

	sse, err := newSSEWriter(w, req.ID)
	if err != nil {
		http.Error(w, "Streaming is not supported", http.StatusInternalServerError)
		return
	}

	// The turn runs concurrently with the relay: it publishes states, the relay
	// forwards them. Waiting for it before relaying would buffer the whole turn
	// and send nothing until it ended.
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.runTurn(ctx, plan)
	}()

	// first is the snapshot taken while subscribing, which is empty here because
	// the task has not been published yet; the relay would still be correct
	// without it, and using it keeps the two streaming handlers identical.
	_ = first
	h.relayTask(ctx, sse, sub, 0)
	<-done
}

// handleSubscribeToTask streams the remaining states of an existing task.
func (h *Handler) handleSubscribeToTask(ctx context.Context, w http.ResponseWriter, req JSONRPCRequest) {
	taskID, failure := h.parseTaskID(req)
	if failure != nil {
		failure.write(w, req.ID)
		return
	}

	sub, first, ok := h.subscribe(taskID)
	if !ok {
		h.unsubscribe(taskID, sub)
		h.taskNotFound(w, req.ID, taskID)
		return
	}
	defer h.unsubscribe(taskID, sub)

	// A task that already finished has nothing left to stream, and the spec
	// reserves UnsupportedOperationError for subscribing to one.
	if first.task.terminal() {
		h.writeError(w, req.ID, CodeUnsupportedOperationError,
			"Operation not supported for a task in a terminal state",
			[]any{errorInfo("UNSUPPORTED_OPERATION", map[string]string{
				"taskId": taskID,
				"state":  first.task.Status.State,
			})})
		return
	}

	sse, err := newSSEWriter(w, req.ID)
	if err != nil {
		http.Error(w, "Streaming is not supported", http.StatusInternalServerError)
		return
	}

	// The current state is the first event of a subscription, by requirement.
	if err := sse.writeTask(first.task); err != nil {
		return
	}
	h.relayTask(ctx, sse, sub, first.seq)
}
