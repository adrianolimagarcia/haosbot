package channels

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/events"
)

// ---------------------------------------------------------------------------
// Test doubles
//
// Every name here is prefixed so it cannot collide with the P0.4 doubles in
// base_test.go, which live in the same package.
// ---------------------------------------------------------------------------

// mgrCall is one recorded outbound primitive invocation.
type mgrCall struct {
	op       string
	chatID   string
	content  string
	streamID *string
	opts     DeltaOptions
	edits    []map[string]any
	metadata map[string]any
}

// mgrChannel is a channel double with every optional hook implemented.
//
// It embeds *Base so the always-available interfaces (DeliveryPolicy,
// SendErrorPolicy, RunningState, ReasoningSender) are the real implementations,
// exactly as a production channel gets them by subclassing BaseChannel.
//
// Every field is read and written under mu, because Send/Start/Stop run on
// manager goroutines and the tests mutate the double concurrently.
type mgrChannel struct {
	*Base

	mu    sync.Mutex
	calls []mgrCall

	blockChat string
	blockAll  bool
	block     chan struct{}
	signal    chan struct{}
	once      sync.Once

	sendResult func(attempt int, msg core.OutboundMessage) error
	sendPolicy func(error) bool
	startFn    func(ctx context.Context) error
	stopFn     func(ctx context.Context) error

	startCount      int
	cancelObserved  bool
	stopSawCanceled bool
}

func newMgrChannel(name string) *mgrChannel {
	c := &mgrChannel{}
	c.Base = NewBase(c, NewMapSection(map[string]any{}), nil,
		WithName(name), WithDisplayName(name), WithLogger(discardLogger()))
	return c
}

// --- configuration (all under mu) ---

func (c *mgrChannel) blockSend(chatID string, gate chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blockChat, c.block, c.blockAll = chatID, gate, false
}

// blockEverySend blocks Send for every destination, which is what the
// concurrency-limit test needs.
func (c *mgrChannel) blockEverySend(gate chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blockChat, c.block, c.blockAll = "", gate, true
}

func (c *mgrChannel) unblockSend() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blockChat, c.block, c.blockAll = "", nil, false
}

func (c *mgrChannel) setSendResult(fn func(attempt int, msg core.OutboundMessage) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sendResult = fn
}

func (c *mgrChannel) setSendPolicy(fn func(error) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sendPolicy = fn
}

func (c *mgrChannel) setStart(fn func(ctx context.Context) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.startFn = fn
}

func (c *mgrChannel) setStop(fn func(ctx context.Context) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopFn = fn
}

// --- Channel implementation ---

func (c *mgrChannel) Start(ctx context.Context) error {
	c.mu.Lock()
	c.startCount++
	fn := c.startFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	c.SetRunning(true)
	<-ctx.Done()
	c.SetRunning(false)
	return ctx.Err()
}

func (c *mgrChannel) Stop(ctx context.Context) error {
	c.SetRunning(false)
	c.mu.Lock()
	fn := c.stopFn
	cancelObserved := c.cancelObserved
	if fn != nil {
		c.stopSawCanceled = cancelObserved
	}
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return nil
}

func (c *mgrChannel) Send(ctx context.Context, msg core.OutboundMessage) error {
	c.mu.Lock()
	c.calls = append(c.calls, mgrCall{
		op: "send", chatID: msg.ChatID, content: msg.Content, metadata: msg.Metadata,
	})
	attempt := len(c.calls)
	gate, blockChat, blockAll, signal := c.block, c.blockChat, c.blockAll, c.signal
	result := c.sendResult
	c.mu.Unlock()

	if signal != nil {
		c.once.Do(func() { close(signal) })
	}
	if gate != nil && (blockAll || msg.ChatID == blockChat) {
		select {
		case <-gate:
		case <-ctx.Done():
			c.mu.Lock()
			c.cancelObserved = true
			c.mu.Unlock()
			return ctx.Err()
		}
	}
	if result != nil {
		return result(attempt, msg)
	}
	return nil
}

func (c *mgrChannel) SendDelta(
	_ context.Context, chatID, delta string, metadata map[string]any, opts DeltaOptions,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, mgrCall{
		op: "delta", chatID: chatID, content: delta, opts: opts,
		streamID: opts.StreamID, metadata: metadata,
	})
	if c.sendResult != nil {
		return c.sendResult(len(c.calls), core.OutboundMessage{})
	}
	return nil
}

func (c *mgrChannel) SendReasoningDelta(
	_ context.Context, chatID, delta string, metadata map[string]any, streamID *string,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, mgrCall{
		op: "reasoning_delta", chatID: chatID, content: delta,
		streamID: streamID, metadata: metadata,
	})
	return nil
}

func (c *mgrChannel) SendReasoningEnd(
	_ context.Context, chatID string, metadata map[string]any, streamID *string,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, mgrCall{
		op: "reasoning_end", chatID: chatID, streamID: streamID, metadata: metadata,
	})
	return nil
}

func (c *mgrChannel) SendFileEditEvents(
	_ context.Context, chatID string, edits []map[string]any, metadata map[string]any,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, mgrCall{
		op: "file_edit", chatID: chatID, edits: edits, metadata: metadata,
	})
	return nil
}

// ShouldRetrySendError shadows Base's, which is what a channel with a
// non-retryable error class does upstream.
func (c *mgrChannel) ShouldRetrySendError(err error) bool {
	c.mu.Lock()
	policy := c.sendPolicy
	c.mu.Unlock()
	if policy == nil {
		return true
	}
	return policy(err)
}

// --- observation ---

func (c *mgrChannel) recorded() []mgrCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]mgrCall(nil), c.calls...)
}

func (c *mgrChannel) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *mgrChannel) ops() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.calls))
	for _, call := range c.calls {
		out = append(out, call.op)
	}
	return out
}

func (c *mgrChannel) starts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.startCount
}

// noCapabilityChannel implements only the three required methods: no
// SendDelta, no reasoning pair, no file-edit surface.
type noCapabilityChannel struct {
	mu    sync.Mutex
	calls []mgrCall
}

func (c *noCapabilityChannel) Name() string        { return "plain" }
func (c *noCapabilityChannel) DisplayName() string { return "Plain" }
func (c *noCapabilityChannel) Start(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
func (c *noCapabilityChannel) Stop(context.Context) error { return nil }
func (c *noCapabilityChannel) Send(_ context.Context, msg core.OutboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, mgrCall{op: "send", chatID: msg.ChatID, content: msg.Content})
	return nil
}
func (c *noCapabilityChannel) recorded() []mgrCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]mgrCall(nil), c.calls...)
}

// startMessageChannel is a channel that supplies its own start error text.
type startMessageChannel struct {
	*mgrChannel
}

func (c *startMessageChannel) StartErrorMessage(error) string { return "install the SDK" }

// ---------------------------------------------------------------------------
// Construction helpers
// ---------------------------------------------------------------------------

func mgrTestConfig(maxRetries int) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Channels.SendMaxRetries = maxRetries
	return cfg
}

func newMgr(t *testing.T, opts ...ManagerOption) (*ChannelManager, *bus.Bus) {
	t.Helper()
	b := bus.New(bus.Options{})
	all := append([]ManagerOption{WithManagerLogger(discardLogger())}, opts...)
	return New(mgrTestConfig(3), b, all...), b
}

func strPtr(s string) *string { return &s }

// waitFor polls cond until it holds or the budget expires.
//
// The budget is deliberately generous because these are LIVENESS checks over
// goroutine scheduling, not performance assertions: `go test -race` slows
// everything down and CI runs the whole suite on a shared runner. A real hang
// still fails, just later.
//
// A generous budget is not a substitute for a correct condition, though: the
// flake this helper reported in TestCancelOutboundReturnsAdmissionPermits was a
// WRONG PRECONDITION in that test (a transient state that only sometimes
// existed), not a slow machine — see the blockEverySend comment there.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func derefStr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// ---------------------------------------------------------------------------
// Retry schedule (manager.py:60, :1043)
// ---------------------------------------------------------------------------

func TestSendRetryDelayClampsByIndex(t *testing.T) {
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second,
	}
	for i, w := range want {
		if got := SendRetryDelay(i + 1); got != w {
			t.Errorf("SendRetryDelay(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := SendRetryDelay(0); got != time.Second {
		t.Errorf("SendRetryDelay(0) = %v, want 1s (clamped index, not a negative index)", got)
	}
	if got := SendRetryDelay(-5); got != time.Second {
		t.Errorf("SendRetryDelay(-5) = %v, want 1s", got)
	}
	if d := SendRetryDelays(); len(d) != 3 || d[0] != time.Second || d[2] != 4*time.Second {
		t.Errorf("SendRetryDelays() = %v, want (1s, 2s, 4s)", d)
	}
}

// ---------------------------------------------------------------------------
// _send_once (manager.py:908-931)
// ---------------------------------------------------------------------------

func TestSendOnceDispatchTable(t *testing.T) {
	cases := []struct {
		name  string
		event core.AgentEvent
		want  []string
	}{
		{"nil_event", nil, []string{"send"}},
		{"progress_empty", events.ProgressEvent{}, []string{"send"}},
		{"progress_content", events.ProgressEvent{Content: "p"}, []string{"send"}},
		{"progress_tool_hint", events.ProgressEvent{Content: "p", ToolHint: true}, []string{"send"}},
		{"progress_reasoning", events.ProgressEvent{Content: "r", Reasoning: true},
			[]string{"reasoning_delta", "reasoning_end"}},
		{"progress_reasoning_delta",
			events.ProgressEvent{Content: "rd", ReasoningDelta: true, StreamID: strPtr("s")},
			[]string{"reasoning_delta"}},
		{"progress_reasoning_end",
			events.ProgressEvent{Content: "re", ReasoningEnd: true, StreamID: strPtr("s")},
			[]string{"reasoning_end"}},
		{"progress_file_edit",
			events.ProgressEvent{Content: "fe", FileEditEvents: []map[string]any{{"path": "a"}}},
			[]string{"file_edit"}},
		{"file_edit_event",
			events.FileEditEvent{ProgressEvent: events.ProgressEvent{
				Content: "fe", FileEditEvents: []map[string]any{{"path": "a"}}}},
			[]string{"file_edit"}},
		{"file_edit_empty", events.FileEditEvent{}, []string{"send"}},
		{"stream_delta", events.StreamDeltaEvent{Content: "d", StreamID: strPtr("s")},
			[]string{"delta"}},
		{"stream_end", events.StreamEndEvent{Content: "e", StreamID: strPtr("s"), Resuming: true, MergeNext: true},
			[]string{"delta"}},
		{"streamed_response", events.StreamedResponseEvent{}, nil},
		{"retry_wait", events.RetryWaitEvent{Content: "w"}, []string{"send"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newMgr(t)
			ch := newMgrChannel("rec")
			msg := core.OutboundMessage{
				Channel: "rec", ChatID: "chat", Content: "c", Metadata: map[string]any{},
				Event: tc.event,
			}
			if err := m.SendOnce(context.Background(), ch, msg); err != nil {
				t.Fatalf("SendOnce: %v", err)
			}
			if got := ch.ops(); !equalStrings(got, tc.want) {
				t.Errorf("ops = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSendOnceStreamEventOptions(t *testing.T) {
	m, _ := newMgr(t)

	ch := newMgrChannel("rec")
	if err := m.SendOnce(context.Background(), ch, core.OutboundMessage{
		Channel: "rec", ChatID: "chat", Content: "d",
		Event: events.StreamDeltaEvent{Content: "d", StreamID: strPtr("s")},
	}); err != nil {
		t.Fatal(err)
	}
	delta := ch.recorded()[0]
	if delta.opts.StreamEnd || delta.opts.Resuming || delta.opts.MergeNext {
		t.Errorf("delta opts = %+v, want all false", delta.opts)
	}
	if derefStr(delta.opts.StreamID) != "s" {
		t.Errorf("delta stream id = %v, want s", derefStr(delta.opts.StreamID))
	}

	ch2 := newMgrChannel("rec")
	if err := m.SendOnce(context.Background(), ch2, core.OutboundMessage{
		Channel: "rec", ChatID: "chat", Content: "e",
		Event: events.StreamEndEvent{Content: "e", StreamID: strPtr("s"), Resuming: true, MergeNext: true},
	}); err != nil {
		t.Fatal(err)
	}
	end := ch2.recorded()[0]
	if !end.opts.StreamEnd || !end.opts.Resuming || !end.opts.MergeNext {
		t.Errorf("end opts = %+v, want stream_end/resuming/merge_next all true", end.opts)
	}

	ch3 := newMgrChannel("rec")
	if err := m.SendOnce(context.Background(), ch3, core.OutboundMessage{
		Channel: "rec", ChatID: "chat", Content: "e",
		Event: events.StreamEndEvent{Content: "e"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := ch3.recorded()[0].opts; got.MergeNext || got.Resuming || !got.StreamEnd {
		t.Errorf("bare end opts = %+v, want only stream_end", got)
	}
}

// A ProgressEvent with several flags set takes the FIRST branch of the elif
// chain. Verified against the reference by probe: reasoning_end > reasoning_delta
// > reasoning > file_edit_events.
func TestSendOnceBranchPriority(t *testing.T) {
	cases := []struct {
		name  string
		event events.ProgressEvent
		want  []string
	}{
		{"end_beats_delta",
			events.ProgressEvent{Content: "x", ReasoningEnd: true, ReasoningDelta: true},
			[]string{"reasoning_end"}},
		{"delta_beats_reasoning",
			events.ProgressEvent{Content: "x", Reasoning: true, ReasoningDelta: true},
			[]string{"reasoning_delta"}},
		{"end_beats_file_edit",
			events.ProgressEvent{Content: "x", ReasoningEnd: true,
				FileEditEvents: []map[string]any{{"a": 1}}},
			[]string{"reasoning_end"}},
		{"reasoning_beats_file_edit",
			events.ProgressEvent{Content: "x", Reasoning: true,
				FileEditEvents: []map[string]any{{"a": 1}}},
			[]string{"reasoning_delta", "reasoning_end"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newMgr(t)
			ch := newMgrChannel("rec")
			if err := m.SendOnce(context.Background(), ch, core.OutboundMessage{
				Channel: "rec", ChatID: "chat", Content: "c", Event: tc.event,
			}); err != nil {
				t.Fatal(err)
			}
			if got := ch.ops(); !equalStrings(got, tc.want) {
				t.Errorf("ops = %v, want %v", got, tc.want)
			}
		})
	}
}

// A channel with no streaming capability keeps BaseChannel's no-op: the call is
// a no-op, not an error, and never falls back to Send.
func TestSendOnceWithoutCapabilitiesIsANoOp(t *testing.T) {
	m, _ := newMgr(t)
	ch := &noCapabilityChannel{}
	toTry := []core.AgentEvent{
		events.StreamDeltaEvent{Content: "d"},
		events.StreamEndEvent{Content: "e"},
		events.ProgressEvent{Content: "r", ReasoningDelta: true},
		events.ProgressEvent{Content: "r", ReasoningEnd: true},
		events.ProgressEvent{Content: "r", Reasoning: true},
		events.ProgressEvent{Content: "f", FileEditEvents: []map[string]any{{"a": 1}}},
	}
	for _, ev := range toTry {
		if err := m.SendOnce(context.Background(), ch, core.OutboundMessage{
			Channel: "plain", ChatID: "c", Content: "x", Event: ev,
		}); err != nil {
			t.Fatalf("%s: %v", ev.EventName(), err)
		}
	}
	if got := ch.recorded(); len(got) != 0 {
		t.Errorf("plain channel recorded %v, want nothing", got)
	}
	if err := m.SendOnce(context.Background(), ch, core.OutboundMessage{
		Channel: "plain", ChatID: "c", Content: "x",
	}); err != nil {
		t.Fatal(err)
	}
	if got := ch.recorded(); len(got) != 1 {
		t.Errorf("plain send recorded %v, want exactly one", got)
	}
}

// The pointer form of an event must dispatch exactly like the value form: Go
// has no inheritance or implicit boxing, so a producer that stores &T would
// otherwise be routed down the "plain message" arm with no error raised.
func TestSendOncePointerEventsDispatchLikeValues(t *testing.T) {
	m, _ := newMgr(t)
	ch := newMgrChannel("rec")
	if err := m.SendOnce(context.Background(), ch, core.OutboundMessage{
		Channel: "rec", ChatID: "c", Content: "d",
		Event: &events.StreamDeltaEvent{Content: "d", StreamID: strPtr("s")},
	}); err != nil {
		t.Fatal(err)
	}
	if got := ch.ops(); !equalStrings(got, []string{"delta"}) {
		t.Errorf("pointer StreamDeltaEvent ops = %v, want [delta]", got)
	}

	ch2 := newMgrChannel("rec")
	if err := m.SendOnce(context.Background(), ch2, core.OutboundMessage{
		Channel: "rec", ChatID: "c", Content: "e",
		Event: &events.StreamedResponseEvent{},
	}); err != nil {
		t.Fatal(err)
	}
	if got := ch2.ops(); len(got) != 0 {
		t.Errorf("pointer StreamedResponseEvent ops = %v, want none", got)
	}
}

// ---------------------------------------------------------------------------
// _coalesce_stream_deltas (manager.py:933-996)
// ---------------------------------------------------------------------------

func deltaMsg(content, chatID string, streamID *string) core.OutboundMessage {
	return events.OutboundMessageForEvent("mock", chatID,
		events.StreamDeltaEvent{Content: content, StreamID: streamID}, nil, nil)
}

func endMsg(content, chatID string, streamID *string, resuming, mergeNext bool) core.OutboundMessage {
	return events.OutboundMessageForEvent("mock", chatID,
		events.StreamEndEvent{Content: content, StreamID: streamID, Resuming: resuming, MergeNext: mergeNext},
		nil, nil)
}

type coalesceWant struct {
	content     string
	eventName   string
	streamID    *string
	resuming    bool
	mergeNext   bool
	pending     int
	pendingName []string
	queueLeft   int
}

func TestCoalesceStreamDeltas(t *testing.T) {
	cases := []struct {
		name   string
		first  core.OutboundMessage
		queued []core.OutboundMessage
		want   coalesceWant
	}{
		{
			name:  "empty_queue",
			first: deltaMsg("A", "chat1", nil),
			want:  coalesceWant{content: "A", eventName: "StreamDeltaEvent"},
		},
		{
			name:   "same_stream_id",
			first:  deltaMsg("A", "chat1", strPtr("s1")),
			queued: []core.OutboundMessage{deltaMsg("B", "chat1", strPtr("s1"))},
			want:   coalesceWant{content: "AB", eventName: "StreamDeltaEvent", streamID: strPtr("s1")},
		},
		{
			name:   "nil_stream_ids_match_each_other",
			first:  deltaMsg("A", "chat1", nil),
			queued: []core.OutboundMessage{deltaMsg("B", "chat1", nil)},
			want:   coalesceWant{content: "AB", eventName: "StreamDeltaEvent"},
		},
		{
			name:   "different_stream_ids_do_not_merge",
			first:  deltaMsg("A", "chat1", strPtr("s1")),
			queued: []core.OutboundMessage{deltaMsg("B", "chat1", strPtr("s2"))},
			want:   coalesceWant{content: "A", eventName: "StreamDeltaEvent", streamID: strPtr("s1"), pending: 1, pendingName: []string{"StreamDeltaEvent"}},
		},
		{
			name:   "nil_then_set_does_not_merge",
			first:  deltaMsg("A", "chat1", nil),
			queued: []core.OutboundMessage{deltaMsg("B", "chat1", strPtr("s2"))},
			want:   coalesceWant{content: "A", eventName: "StreamDeltaEvent", pending: 1, pendingName: []string{"StreamDeltaEvent"}},
		},
		{
			name:   "set_then_nil_does_not_merge",
			first:  deltaMsg("A", "chat1", strPtr("s1")),
			queued: []core.OutboundMessage{deltaMsg("B", "chat1", nil)},
			want:   coalesceWant{content: "A", eventName: "StreamDeltaEvent", streamID: strPtr("s1"), pending: 1, pendingName: []string{"StreamDeltaEvent"}},
		},
		{
			name:   "different_chat_does_not_merge",
			first:  deltaMsg("A", "chat1", nil),
			queued: []core.OutboundMessage{deltaMsg("B", "chat2", nil)},
			want:   coalesceWant{content: "A", eventName: "StreamDeltaEvent", pending: 1, pendingName: []string{"StreamDeltaEvent"}},
		},
		{
			name:   "end_with_content_merges_and_terminates",
			first:  deltaMsg("Hello", "chat1", strPtr("s1")),
			queued: []core.OutboundMessage{endMsg(" world", "chat1", strPtr("s1"), true, true)},
			want: coalesceWant{
				content: "Hello world", eventName: "StreamEndEvent", streamID: strPtr("s1"),
				resuming: true, mergeNext: true,
			},
		},
		{
			name:   "end_with_content_nil_stream_id",
			first:  deltaMsg("Hello", "chat1", nil),
			queued: []core.OutboundMessage{endMsg(" world", "chat1", nil, false, false)},
			want:   coalesceWant{content: "Hello world", eventName: "StreamEndEvent"},
		},
		{
			// The load-bearing edge case: an end event with EMPTY content is the
			// boundary, not a merge target, so the deltas are not swallowed.
			name:   "end_with_empty_content_is_the_boundary",
			first:  deltaMsg("Hello", "chat1", strPtr("s1")),
			queued: []core.OutboundMessage{endMsg("", "chat1", strPtr("s1"), false, false)},
			want:   coalesceWant{content: "Hello", eventName: "StreamDeltaEvent", streamID: strPtr("s1"), pending: 1, pendingName: []string{"StreamEndEvent"}},
		},
		{
			name:   "end_with_empty_content_nil_id",
			first:  deltaMsg("Hello", "chat1", nil),
			queued: []core.OutboundMessage{endMsg("", "chat1", nil, false, false)},
			want:   coalesceWant{content: "Hello", eventName: "StreamDeltaEvent", pending: 1, pendingName: []string{"StreamEndEvent"}},
		},
		{
			name:   "end_stops_before_the_next_delta",
			first:  deltaMsg("Hello", "chat1", strPtr("s1")),
			queued: []core.OutboundMessage{endMsg(" world", "chat1", strPtr("s1"), false, false), deltaMsg("C", "chat1", strPtr("s1"))},
			want:   coalesceWant{content: "Hello world", eventName: "StreamEndEvent", streamID: strPtr("s1"), queueLeft: 1},
		},
		{
			name:   "plain_message_is_the_boundary",
			first:  deltaMsg("A", "chat1", nil),
			queued: []core.OutboundMessage{{Channel: "mock", ChatID: "chat1", Content: "plain"}},
			want:   coalesceWant{content: "A", eventName: "StreamDeltaEvent", pending: 1, pendingName: []string{""}},
		},
		{
			name:   "progress_event_is_the_boundary",
			first:  deltaMsg("A", "chat1", nil),
			queued: []core.OutboundMessage{events.OutboundMessageForEvent("mock", "chat1", events.ProgressEvent{Content: "p"}, nil, nil)},
			want:   coalesceWant{content: "A", eventName: "StreamDeltaEvent", pending: 1, pendingName: []string{"ProgressEvent"}},
		},
		{
			name:   "retry_wait_is_the_boundary",
			first:  deltaMsg("A", "chat1", nil),
			queued: []core.OutboundMessage{events.OutboundMessageForEvent("mock", "chat1", events.RetryWaitEvent{Content: "w"}, nil, nil)},
			want:   coalesceWant{content: "A", eventName: "StreamDeltaEvent", pending: 1, pendingName: []string{"RetryWaitEvent"}},
		},
		{
			name:   "streamed_response_is_the_boundary",
			first:  deltaMsg("A", "chat1", nil),
			queued: []core.OutboundMessage{events.OutboundMessageForEvent("mock", "chat1", events.StreamedResponseEvent{}, nil, nil)},
			want:   coalesceWant{content: "A", eventName: "StreamDeltaEvent", pending: 1, pendingName: []string{"StreamedResponseEvent"}},
		},
		{
			name:   "many_deltas_then_boundary",
			first:  deltaMsg("a", "chat1", nil),
			queued: []core.OutboundMessage{deltaMsg("b", "chat1", nil), deltaMsg("c", "chat1", nil), {Channel: "mock", ChatID: "chat1", Content: "Z"}},
			want:   coalesceWant{content: "abc", eventName: "StreamDeltaEvent", pending: 1, pendingName: []string{""}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, b := newMgr(t)
			for _, q := range tc.queued {
				if err := b.PublishOutbound(context.Background(), q); err != nil {
					t.Fatal(err)
				}
			}
			merged, pending := m.CoalesceStreamDeltas(tc.first)

			if merged.Content != tc.want.content {
				t.Errorf("content = %q, want %q", merged.Content, tc.want.content)
			}
			gotName := ""
			if merged.Event != nil {
				gotName = merged.Event.EventName()
			}
			if gotName != tc.want.eventName {
				t.Errorf("event = %q, want %q", gotName, tc.want.eventName)
			}
			if !sameStreamID(EventStreamID(merged.Event), tc.want.streamID) {
				t.Errorf("stream id = %v, want %v",
					derefStr(EventStreamID(merged.Event)), derefStr(tc.want.streamID))
			}
			if end, ok := merged.Event.(events.StreamEndEvent); ok {
				if end.Resuming != tc.want.resuming || end.MergeNext != tc.want.mergeNext {
					t.Errorf("end flags = (resuming=%v merge_next=%v), want (%v, %v)",
						end.Resuming, end.MergeNext, tc.want.resuming, tc.want.mergeNext)
				}
			}
			if len(pending) != tc.want.pending {
				t.Fatalf("pending = %d (%v), want %d", len(pending), pendingNames(pending), tc.want.pending)
			}
			for i, wantName := range tc.want.pendingName {
				got := ""
				if pending[i].Event != nil {
					got = pending[i].Event.EventName()
				}
				if got != wantName {
					t.Errorf("pending[%d] event = %q, want %q", i, got, wantName)
				}
			}
			if left := b.OutboundSize(); left != tc.want.queueLeft {
				t.Errorf("queue left = %d, want %d", left, tc.want.queueLeft)
			}
		})
	}
}

// The merged message shares the first message's metadata map (dataclasses.replace
// semantics) and leaves the original untouched.
func TestCoalesceSharesMetadataAndCopiesTheMessage(t *testing.T) {
	m, b := newMgr(t)
	first := events.OutboundMessageForEvent("mock", "chat1",
		events.StreamDeltaEvent{Content: "A"}, nil, map[string]any{"k": "v"})
	if err := b.PublishOutbound(context.Background(), deltaMsg("B", "chat1", nil)); err != nil {
		t.Fatal(err)
	}
	merged, _ := m.CoalesceStreamDeltas(first)

	if merged.Content != "AB" {
		t.Errorf("merged content = %q, want AB", merged.Content)
	}
	if first.Content != "A" {
		t.Errorf("the input message was mutated: content = %q, want A", first.Content)
	}
	if len(merged.Metadata) != 1 || merged.Metadata["k"] != "v" {
		t.Errorf("merged metadata = %v, want {k: v}", merged.Metadata)
	}
	// dataclasses.replace is a SHALLOW copy, so the merged message shares the
	// first message's metadata dict rather than cloning it. Go maps are
	// reference types, so a write through one handle must be visible through the
	// other; a clone would silently break a channel that publishes state by
	// mutating the map it was handed.
	merged.Metadata["added"] = 1
	if first.Metadata["added"] != 1 {
		t.Error("the merged message's metadata was cloned; the reference shares it")
	}
}

func pendingNames(msgs []core.OutboundMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if m.Event == nil {
			out = append(out, "")
			continue
		}
		out = append(out, m.Event.EventName())
	}
	return out
}

// ---------------------------------------------------------------------------
// _should_suppress_outbound (manager.py:698-719)
// ---------------------------------------------------------------------------

func TestShouldSuppressOutbound(t *testing.T) {
	type step struct {
		content  string
		metadata map[string]any
		event    core.AgentEvent
	}
	cases := []struct {
		name  string
		steps []step
		want  []bool
	}{
		{
			name: "same_origin_message_id_is_suppressed",
			steps: []step{
				{"hello", map[string]any{"origin_message_id": "o1"}, nil},
				{"hello", map[string]any{"origin_message_id": "o1"}, nil},
			},
			want: []bool{false, true},
		},
		{
			name: "whitespace_is_normalized_before_hashing",
			steps: []step{
				{"hello  world", map[string]any{"origin_message_id": "o1"}, nil},
				{"hello\nworld", map[string]any{"origin_message_id": "o1"}, nil},
				{"  hello   world  ", map[string]any{"origin_message_id": "o1"}, nil},
			},
			want: []bool{false, true, true},
		},
		{
			name: "different_content_is_never_suppressed",
			steps: []step{
				{"hello", map[string]any{"origin_message_id": "o1"}, nil},
				{"other", map[string]any{"origin_message_id": "o1"}, nil},
				{"hello", map[string]any{"origin_message_id": "o1"}, nil},
			},
			want: []bool{false, false, false},
		},
		{
			name: "empty_content_is_never_suppressed",
			steps: []step{
				{"", map[string]any{"origin_message_id": "o1"}, nil},
				{"", map[string]any{"origin_message_id": "o1"}, nil},
			},
			want: []bool{false, false},
		},
		{
			name: "whitespace_only_content_is_never_suppressed",
			steps: []step{
				{"   ", map[string]any{"origin_message_id": "o1"}, nil},
				{"\t\n", map[string]any{"origin_message_id": "o1"}, nil},
			},
			want: []bool{false, false},
		},
		{
			name: "non_string_origin_id_is_ignored",
			steps: []step{
				{"hello", map[string]any{"origin_message_id": 7}, nil},
				{"hello", map[string]any{"origin_message_id": 7}, nil},
			},
			want: []bool{false, false},
		},
		{
			name: "empty_string_origin_id_is_ignored",
			steps: []step{
				{"hello", map[string]any{"origin_message_id": ""}, nil},
				{"hello", map[string]any{"origin_message_id": ""}, nil},
			},
			want: []bool{false, false},
		},
		{
			name: "message_id_alone_never_suppresses",
			steps: []step{
				{"hello", map[string]any{"message_id": "m1"}, nil},
				{"hello", map[string]any{"message_id": "m1"}, nil},
			},
			want: []bool{false, false},
		},
		{
			name: "progress_events_are_never_suppressed",
			steps: []step{
				{"hello", map[string]any{"origin_message_id": "o1"}, events.ProgressEvent{Content: "hello"}},
				{"hello", map[string]any{"origin_message_id": "o1"}, events.ProgressEvent{Content: "hello"}},
			},
			want: []bool{false, false},
		},
		{
			name: "file_edit_events_are_never_suppressed",
			steps: []step{
				{"hello", map[string]any{"origin_message_id": "o1"}, events.FileEditEvent{}},
				{"hello", map[string]any{"origin_message_id": "o1"}, events.FileEditEvent{}},
			},
			want: []bool{false, false},
		},
		{
			name: "nil_metadata_is_never_suppressed",
			steps: []step{
				{"hello", nil, nil},
				{"hello", nil, nil},
			},
			want: []bool{false, false},
		},
		{
			// NBSP is Python whitespace AND Go's unicode.IsSpace, but it is NOT
			// covered by strings.TrimSpace's ASCII set in older ports. The
			// fingerprint must treat it as a space.
			name: "nbsp_normalizes_like_a_space",
			steps: []step{
				{"a\u00a0b", map[string]any{"origin_message_id": "o1"}, nil},
				{"a b", map[string]any{"origin_message_id": "o1"}, nil},
			},
			want: []bool{false, true},
		},
		{
			// U+001C is Python whitespace and NOT Go's unicode.IsSpace.
			name: "unit_separator_normalizes_like_a_space",
			steps: []step{
				{"a\u001cb", map[string]any{"origin_message_id": "o1"}, nil},
				{"a b", map[string]any{"origin_message_id": "o1"}, nil},
			},
			want: []bool{false, true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newMgr(t)
			for i, s := range tc.steps {
				got := m.ShouldSuppressOutbound(core.OutboundMessage{
					Channel: "mock", ChatID: "chat1", Content: s.content,
					Metadata: s.metadata, Event: s.event,
				})
				if got != tc.want[i] {
					t.Errorf("step %d (%q): = %v, want %v", i, s.content, got, tc.want[i])
				}
			}
		})
	}
}

// The fingerprint memory is bounded at 1000 entries and evicts oldest-first
// (manager.py:65, :688-696).
func TestFingerprintStoreIsBoundedAndEvictsOldest(t *testing.T) {
	m, _ := newMgr(t)
	for i := 0; i < OriginReplyFingerprintsMaxSize+5; i++ {
		m.ShouldSuppressOutbound(core.OutboundMessage{
			Channel: "mock", ChatID: "chat1", Content: "hello",
			Metadata: map[string]any{"origin_message_id": fmt.Sprintf("o%d", i)},
		})
	}
	if got := m.fingerprints.len(); got != OriginReplyFingerprintsMaxSize {
		t.Fatalf("fingerprint store holds %d entries, want %d", got, OriginReplyFingerprintsMaxSize)
	}
	keys := m.fingerprints.keys()
	if keys[0].id != "o5" {
		t.Errorf("oldest key = %q, want o5 (the first five were evicted)", keys[0].id)
	}
	if last := keys[len(keys)-1].id; last != fmt.Sprintf("o%d", OriginReplyFingerprintsMaxSize+4) {
		t.Errorf("newest key = %q, want o%d", last, OriginReplyFingerprintsMaxSize+4)
	}
}

// A hit moves the key to the most-recent end, so it is not the next evicted.
func TestFingerprintStoreTouchPreventsEviction(t *testing.T) {
	store := newFingerprintStore(3)
	keys := []fingerprintKey{{channel: "c", chatID: "1", id: "a"}, {channel: "c", chatID: "1", id: "b"}}
	store.remember(keys[0], "fa")
	store.remember(keys[1], "fb")
	store.touch(keys[0]) // a is now newest
	store.remember(fingerprintKey{channel: "c", chatID: "1", id: "c"}, "fc")

	got := store.keys()
	if len(got) != 3 || got[0].id != "b" {
		t.Fatalf("keys = %v, want b evicted first", got)
	}
}

func TestFingerprintContent(t *testing.T) {
	// The digests below were produced by running the reference:
	//   hashlib.sha1(" ".join(s.split()).encode("utf-8")).hexdigest()
	// See compat/python/dump_manager.py, section "fingerprint".
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"\u00a0", ""},
		{"\u001c", ""},
		{"hello", "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d"},
		{"hello  world", "2aae6c35c94fcfb415dbe95f408b9ce91ee846ed"},
		{"hello\nworld", "2aae6c35c94fcfb415dbe95f408b9ce91ee846ed"},
		{"a b", "7dbde93504122a707f849f2c12bdd9de71b41929"},
		{"a\u00a0b", "7dbde93504122a707f849f2c12bdd9de71b41929"},
		{"a\u001cb", "7dbde93504122a707f849f2c12bdd9de71b41929"},
	}
	for _, tc := range cases {
		if got := FingerprintContent(tc.in); got != tc.want {
			t.Errorf("FingerprintContent(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// _queue_outbound (manager.py:728-759)
// ---------------------------------------------------------------------------

func TestQueueOutboundPreservesPerDestinationFIFO(t *testing.T) {
	m, _ := newMgr(t)
	ch := newMgrChannel("mock")
	m.AddChannel("mock", ch)

	release := make(chan struct{})
	ch.blockSend("0", release)

	// Ten destinations, one of which is blocked. Each gets "first" then "last".
	for _, content := range []string{"first", "last"} {
		for i := 0; i < 10; i++ {
			if err := m.QueueOutbound(context.Background(), ch, core.OutboundMessage{
				Channel: "mock", ChatID: fmt.Sprintf("%d", i), Content: content,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Twenty admissions all fit (the default pending limit is 256), so queueing
	// never blocks here; the tasks drain as they complete.
	perChat := func() map[string][]string {
		out := map[string][]string{}
		for _, c := range ch.recorded() {
			out[c.chatID] = append(out[c.chatID], c.content)
		}
		return out
	}

	// Every destination other than "0" completes while "0" is blocked.
	waitFor(t, "the nine healthy destinations", func() bool {
		got := perChat()
		for i := 1; i < 10; i++ {
			if len(got[fmt.Sprintf("%d", i)]) != 2 {
				return false
			}
		}
		return true
	})
	// Chat "0" entered Send exactly once and is still blocked there: the FIFO
	// head holds the destination and its successor waits.
	if got := perChat()["0"]; !equalStrings(got, []string{"first"}) {
		t.Fatalf("the blocked destination has %v, want exactly [first] (head only)", got)
	}
	for i := 1; i < 10; i++ {
		if got := perChat()[fmt.Sprintf("%d", i)]; !equalStrings(got, []string{"first", "last"}) {
			t.Errorf("chat %d = %v, want [first last]", i, got)
		}
	}

	close(release)
	waitFor(t, "the blocked destination to finish", func() bool {
		return equalStrings(perChat()["0"], []string{"first", "last"})
	})
	waitFor(t, "all tasks to drain", func() bool { return m.outboundTaskCount() == 0 })
	if n := m.outboundTailCount(); n != 0 {
		t.Errorf("outbound tails = %d, want 0", n)
	}
}

func TestQueueOutboundBoundsConcurrencyAndAdmission(t *testing.T) {
	m, _ := newMgr(t, WithOutboundLimits(4, 2))
	ch := newMgrChannel("mock")
	m.AddChannel("mock", ch)

	release := make(chan struct{})
	ch.blockEverySend(release)

	for i := 0; i < 4; i++ {
		if err := m.QueueOutbound(context.Background(), ch, core.OutboundMessage{
			Channel: "mock", ChatID: fmt.Sprintf("%d", i), Content: "a",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// The concurrency limit is 2, so exactly two sends may enter Send().
	waitFor(t, "two sends to start", func() bool { return ch.callCount() == 2 })
	time.Sleep(50 * time.Millisecond)
	if got := ch.callCount(); got != 2 {
		t.Fatalf("concurrent sends = %d, want 2", got)
	}
	if got := m.outboundTaskCount(); got != 4 {
		t.Errorf("outbound tasks = %d, want 4", got)
	}

	// The fifth admission blocks: the pending limit is 4.
	extra := make(chan error, 1)
	go func() {
		extra <- m.QueueOutbound(context.Background(), ch, core.OutboundMessage{
			Channel: "mock", ChatID: "extra", Content: "a",
		})
	}()
	select {
	case err := <-extra:
		t.Fatalf("the fifth admission returned %v, want it to block", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	waitFor(t, "the four tasks to drain", func() bool { return m.outboundTaskCount() == 0 })
	if err := <-extra; err != nil {
		t.Fatalf("the fifth admission returned %v", err)
	}
	if got := ch.callCount(); got != 5 {
		t.Errorf("total sends = %d, want 5", got)
	}
}

func TestQueueOutboundDropsForAStoppingOrReplacedChannel(t *testing.T) {
	m, _ := newMgr(t)
	ch := newMgrChannel("mock")
	m.AddChannel("mock", ch)

	// Stopping: the mark makes admission drop the message (manager.py:735).
	m.mu.Lock()
	m.stopping["mock"] = true
	m.mu.Unlock()
	if err := m.QueueOutbound(context.Background(), ch, core.OutboundMessage{
		Channel: "mock", ChatID: "c", Content: "dropped",
	}); err != nil {
		t.Fatal(err)
	}
	if got := m.outboundTaskCount(); got != 0 {
		t.Errorf("queued %d tasks for a stopping channel, want 0", got)
	}
	m.mu.Lock()
	delete(m.stopping, "mock")
	m.mu.Unlock()

	// Replaced: a different runtime object under the same name.
	other := newMgrChannel("mock")
	m.AddChannel("mock", other)
	if err := m.QueueOutbound(context.Background(), ch, core.OutboundMessage{
		Channel: "mock", ChatID: "c", Content: "dropped",
	}); err != nil {
		t.Fatal(err)
	}
	if got := m.outboundTaskCount(); got != 0 {
		t.Errorf("queued %d tasks for a replaced channel, want 0", got)
	}
	if got := other.callCount(); got != 0 {
		t.Errorf("the replacement channel received %d calls, want 0", got)
	}
}

func TestCancelOutboundReturnsAdmissionPermits(t *testing.T) {
	m, _ := newMgr(t, WithOutboundLimits(4, 2))
	ch := newMgrChannel("mock")
	m.AddChannel("mock", ch)

	release := make(chan struct{})
	// EVERY destination must block, not just chat "0". blockSend("0", ...) left
	// the other three sends free to complete, so outboundTaskCount() settled at 1
	// and the "tasks to be registered" wait below could only ever be satisfied by
	// catching a TRANSIENT moment before those three finished — it failed under
	// `go test -race -count=25 ./internal/channels/`. Holding all four is also
	// what the test is actually about: the four admission permits must be
	// returned by cancellation.
	ch.blockEverySend(release)

	for i := 0; i < 4; i++ {
		if err := m.QueueOutbound(context.Background(), ch, core.OutboundMessage{
			Channel: "mock", ChatID: fmt.Sprintf("%d", i), Content: "a",
		}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "the tasks to be registered", func() bool { return m.outboundTaskCount() == 4 })

	m.CancelOutbound(context.Background(), "")
	if got := m.outboundTaskCount(); got != 0 {
		t.Fatalf("outbound tasks after cancel = %d, want 0", got)
	}
	if got := m.outboundTailCount(); got != 0 {
		t.Errorf("outbound tails after cancel = %d, want 0", got)
	}

	// Permits came back: four fresh admissions succeed without blocking.
	ch.unblockSend()
	for i := 0; i < 4; i++ {
		done := make(chan error, 1)
		go func(i int) {
			done <- m.QueueOutbound(context.Background(), ch, core.OutboundMessage{
				Channel: "mock", ChatID: fmt.Sprintf("fresh-%d", i), Content: "b",
			})
		}(i)
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("admission blocked: cancellation did not return the permits")
		}
	}
	waitFor(t, "the fresh tasks to drain", func() bool { return m.outboundTaskCount() == 0 })
}

// ---------------------------------------------------------------------------
// _dispatch_outbound_loop (manager.py:767-850)
// ---------------------------------------------------------------------------

func TestDispatchLoopPreservesOrderPerDestination(t *testing.T) {
	m, b := newMgr(t)
	ch := newMgrChannel("mock")
	m.AddChannel("mock", ch)

	for i := 0; i < 3; i++ {
		if err := b.PublishOutbound(context.Background(), core.OutboundMessage{
			Channel: "mock", ChatID: "c1", Content: fmt.Sprintf("a%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := b.PublishOutbound(context.Background(), core.OutboundMessage{
			Channel: "mock", ChatID: "c2", Content: fmt.Sprintf("b%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.dispatchOutbound(ctx)

	waitFor(t, "six deliveries", func() bool { return ch.callCount() == 6 })

	byChat := map[string][]string{}
	for _, c := range ch.recorded() {
		byChat[c.chatID] = append(byChat[c.chatID], c.content)
	}
	if !equalStrings(byChat["c1"], []string{"a0", "a1", "a2"}) {
		t.Errorf("c1 = %v, want [a0 a1 a2]", byChat["c1"])
	}
	if !equalStrings(byChat["c2"], []string{"b0", "b1", "b2"}) {
		t.Errorf("c2 = %v, want [b0 b1 b2]", byChat["c2"])
	}
}

func TestDispatchLoopSurvivesAnUnknownChannel(t *testing.T) {
	m, b := newMgr(t)
	ch := newMgrChannel("mock")
	m.AddChannel("mock", ch)

	if err := b.PublishOutbound(context.Background(), core.OutboundMessage{
		Channel: "ghost", ChatID: "c", Content: "x",
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.PublishOutbound(context.Background(), core.OutboundMessage{
		Channel: "mock", ChatID: "c", Content: "real",
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.dispatchOutbound(ctx) }()

	waitFor(t, "the known channel to be served", func() bool { return ch.callCount() == 1 })
	if got := ch.recorded()[0].content; got != "real" {
		t.Errorf("delivered %q, want real", got)
	}
	waitFor(t, "the queue to drain", func() bool { return b.OutboundSize() == 0 })
	cancel()
	<-done
}

func TestDispatchLoopGates(t *testing.T) {
	cases := []struct {
		name          string
		sendProgress  bool
		sendToolHints bool
		showReasoning bool
		event         core.AgentEvent
		wantOps       []string
	}{
		{"progress_on", true, true, true, events.ProgressEvent{Content: "p"}, []string{"send"}},
		{"progress_off", false, true, true, events.ProgressEvent{Content: "p"}, nil},
		{"tool_hint_on", true, true, true, events.ProgressEvent{Content: "p", ToolHint: true}, []string{"send"}},
		{"tool_hint_off", true, false, true, events.ProgressEvent{Content: "p", ToolHint: true}, nil},
		{"tool_hint_uses_hints_not_progress", false, true, true,
			events.ProgressEvent{Content: "p", ToolHint: true}, []string{"send"}},
		{"reasoning_on", true, true, true,
			events.ProgressEvent{Content: "r", ReasoningDelta: true, StreamID: strPtr("s")},
			[]string{"reasoning_delta"}},
		{"reasoning_off", true, true, false,
			events.ProgressEvent{Content: "r", ReasoningDelta: true, StreamID: strPtr("s")}, nil},
		{"retry_wait_dropped", true, true, true, events.RetryWaitEvent{Content: "w"}, nil},
		{"streamed_response_silent", true, true, true, events.StreamedResponseEvent{}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, b := newMgr(t)
			ch := newMgrChannel("mock")
			ch.SetSendProgress(tc.sendProgress)
			ch.SetSendToolHints(tc.sendToolHints)
			ch.SetShowReasoning(tc.showReasoning)
			m.AddChannel("mock", ch)

			if err := b.PublishOutbound(context.Background(), events.OutboundMessageForEvent(
				"mock", "c", tc.event, nil, nil,
			)); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); m.dispatchOutbound(ctx) }()
			waitFor(t, "the queue to drain", func() bool { return b.OutboundSize() == 0 })
			time.Sleep(20 * time.Millisecond)
			cancel()
			<-done

			if got := ch.ops(); !equalStrings(got, tc.wantOps) {
				t.Errorf("ops = %v, want %v", got, tc.wantOps)
			}
		})
	}
}

// A progress event for an unregistered channel is dropped, not warned about and
// not delivered (manager.py:349-353).
func TestDispatchLoopDropsReasoningForAnUnknownChannel(t *testing.T) {
	m, b := newMgr(t)
	if err := b.PublishOutbound(context.Background(), events.OutboundMessageForEvent(
		"ghost", "c",
		events.ProgressEvent{Content: "r", ReasoningDelta: true, StreamID: strPtr("s")},
		nil, nil,
	)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.dispatchOutbound(ctx) }()
	waitFor(t, "the queue to drain", func() bool { return b.OutboundSize() == 0 })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the dispatcher did not survive a reasoning event for an unknown channel")
	}
}

func TestDispatchLoopSuppressesDuplicateOriginReplies(t *testing.T) {
	m, b := newMgr(t)
	ch := newMgrChannel("mock")
	m.AddChannel("mock", ch)

	for i := 0; i < 2; i++ {
		if err := b.PublishOutbound(context.Background(), core.OutboundMessage{
			Channel: "mock", ChatID: "c", Content: "dup",
			Metadata: map[string]any{"origin_message_id": "o1"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.dispatchOutbound(ctx) }()
	waitFor(t, "the first delivery", func() bool { return ch.callCount() == 1 })
	waitFor(t, "the queue to drain", func() bool { return b.OutboundSize() == 0 })
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	if got := ch.callCount(); got != 1 {
		t.Errorf("delivered %d messages, want 1 (the duplicate is suppressed)", got)
	}
}

// Coalescing keeps bus order: the merged delta is delivered before the plain
// message that bounded it.
func TestDispatchLoopCoalescesStreamDeltas(t *testing.T) {
	m, b := newMgr(t)
	ch := newMgrChannel("mock")
	m.AddChannel("mock", ch)

	if err := b.PublishOutbound(context.Background(), deltaMsg("A", "c", nil)); err != nil {
		t.Fatal(err)
	}
	if err := b.PublishOutbound(context.Background(), deltaMsg("B", "c", nil)); err != nil {
		t.Fatal(err)
	}
	if err := b.PublishOutbound(context.Background(), core.OutboundMessage{
		Channel: "mock", ChatID: "c", Content: "Final",
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.dispatchOutbound(ctx)

	waitFor(t, "both deliveries", func() bool { return ch.callCount() == 2 })
	got := ch.recorded()
	if got[0].op != "delta" || got[0].content != "AB" {
		t.Errorf("first delivery = %+v, want delta AB", got[0])
	}
	if got[1].op != "send" || got[1].content != "Final" {
		t.Errorf("second delivery = %+v, want send Final", got[1])
	}
}

// Cancelling the dispatcher cancels active AND waiting destination tasks and
// leaves the tail map empty (tests/channels/test_channel_manager_concurrency.py:163).
func TestDispatcherShutdownCancelsActiveAndWaitingTasks(t *testing.T) {
	m, b := newMgr(t)
	ch := newMgrChannel("mock")
	m.AddChannel("mock", ch)
	ch.blockSend("chat", make(chan struct{}))

	for _, content := range []string{"one", "two", "three"} {
		if err := b.PublishOutbound(context.Background(), core.OutboundMessage{
			Channel: "mock", ChatID: "chat", Content: content,
		}); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.dispatchOutbound(ctx) }()

	waitFor(t, "three queued tasks", func() bool { return m.outboundTaskCount() == 3 })
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the dispatcher did not stop")
	}

	if got := m.outboundTaskCount(); got != 0 {
		t.Errorf("outbound tasks = %d, want 0", got)
	}
	if got := m.outboundTailCount(); got != 0 {
		t.Errorf("outbound tails = %d, want 0", got)
	}
	// Only the FIFO head ever started sending.
	if got := ch.callCount(); got != 1 {
		t.Errorf("Send was called %d times, want 1 (the successors wait on the head)", got)
	}
}

// ---------------------------------------------------------------------------
// _send_with_retry (manager.py:998-1056)
// ---------------------------------------------------------------------------

func TestSendWithRetryAttemptBudget(t *testing.T) {
	cases := []struct {
		name       string
		failures   int
		maxRetries int
		retryable  bool
		wantCalls  int
	}{
		{"success_first_try", 0, 3, true, 1},
		{"two_failures_then_success", 2, 3, true, 3},
		{"exhausts_at_the_cap", 5, 3, true, 3},
		{"cap_one_means_one_attempt", 5, 1, true, 1},
		{"cap_zero_clamps_to_one", 5, 0, true, 1},
		{"cap_five", 9, 5, true, 5},
		{"non_retryable_stops_immediately", 5, 3, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newMgr(t, WithSendRetryDelays([]time.Duration{time.Millisecond}))
			m.config.Channels.SendMaxRetries = tc.maxRetries
			ch := newMgrChannel("mock")
			ch.setSendPolicy(func(error) bool { return tc.retryable })
			ch.setSendResult(func(attempt int, _ core.OutboundMessage) error {
				if attempt <= tc.failures {
					return errors.New("boom")
				}
				return nil
			})

			m.SendWithRetry(context.Background(), ch, core.OutboundMessage{
				Channel: "mock", ChatID: "c", Content: "x",
			}, nil)

			if got := ch.callCount(); got != tc.wantCalls {
				t.Errorf("attempts = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

func TestSendWithRetryWalksTheClampedSchedule(t *testing.T) {
	m, _ := newMgr(t, WithSendRetryDelays([]time.Duration{
		time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond,
	}))
	m.config.Channels.SendMaxRetries = 5
	ch := newMgrChannel("mock")
	ch.setSendResult(func(int, core.OutboundMessage) error { return errors.New("always fails") })

	start := time.Now()
	m.SendWithRetry(context.Background(), ch, core.OutboundMessage{
		Channel: "mock", ChatID: "c", Content: "x",
	}, nil)
	elapsed := time.Since(start)

	if got := ch.callCount(); got != 5 {
		t.Fatalf("attempts = %d, want 5", got)
	}
	// 1 + 2 + 4 + 4 = 11ms of sleeping. A generous lower bound proves the
	// schedule was walked rather than skipped.
	if elapsed < 10*time.Millisecond {
		t.Errorf("elapsed %v, want at least 10ms of backoff", elapsed)
	}
}

func TestSendWithRetryHonoursCancellation(t *testing.T) {
	m, _ := newMgr(t, WithSendRetryDelays([]time.Duration{time.Hour}))
	ch := newMgrChannel("mock")
	ch.setSendResult(func(int, core.OutboundMessage) error { return errors.New("always fails") })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.SendWithRetry(ctx, ch, core.OutboundMessage{Channel: "mock", ChatID: "c", Content: "x"}, nil)
	}()

	waitFor(t, "the first attempt", func() bool { return ch.callCount() == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SendWithRetry ignored cancellation during the backoff sleep")
	}
	if got := ch.callCount(); got != 1 {
		t.Errorf("attempts = %d, want 1 (cancelled during the sleep)", got)
	}
}

// The deadline arm retries until the deadline rather than the attempt cap
// (manager.py:1032-1036) and clamps each delay to the remaining time (:1044-1045).
func TestSendWithRetryDeadlineOverridesTheAttemptCap(t *testing.T) {
	m, _ := newMgr(t, WithSendRetryDelays([]time.Duration{10 * time.Millisecond}))
	m.config.Channels.SendMaxRetries = 1
	ch := newMgrChannel("mock")
	ch.setSendResult(func(int, core.OutboundMessage) error { return errors.New("always fails") })

	deadline := time.Now().Add(60 * time.Millisecond)
	m.SendWithRetry(context.Background(), ch, core.OutboundMessage{
		Channel: "mock", ChatID: "c", Content: "x",
	}, &deadline)

	if got := ch.callCount(); got < 3 {
		t.Errorf("attempts = %d, want more than the cap of 1 (deadline path)", got)
	}
	if time.Now().Before(deadline.Add(-5 * time.Millisecond)) {
		t.Error("SendWithRetry returned before the deadline with failures outstanding")
	}
}

// ---------------------------------------------------------------------------
// Lifecycle (P0.6)
// ---------------------------------------------------------------------------

func TestStartAllWithNoChannelsDoesNotStart(t *testing.T) {
	m, _ := newMgr(t)
	if err := m.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	if m.Started() {
		t.Error("Started() = true; the reference returns before setting _started (manager.py:600-602)")
	}
	m.mu.Lock()
	dispatch := m.dispatchCancel
	m.mu.Unlock()
	if dispatch != nil {
		t.Error("a dispatcher was started with no channels")
	}
}

func TestStartAllBlocksUntilChannelsFinish(t *testing.T) {
	m, _ := newMgr(t)
	ch := newMgrChannel("mock")
	m.AddChannel("mock", ch)

	done := make(chan error, 1)
	go func() { done <- m.StartAll(context.Background()) }()

	waitFor(t, "the channel to be started", func() bool { return ch.starts() == 1 })
	if !m.Started() {
		t.Error("Started() = false after StartAll with channels")
	}
	select {
	case err := <-done:
		t.Fatalf("StartAll returned %v while a channel is still running", err)
	case <-time.After(50 * time.Millisecond):
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.StopAll(ctx); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StartAll returned %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("StartAll did not return after StopAll")
	}
	if m.Started() {
		t.Error("Started() = true after StopAll")
	}
}

func TestStartChannelRecordsTheErrorAndKeepsGoing(t *testing.T) {
	m, _ := newMgr(t)
	failing := newMgrChannel("bad")
	failing.setStart(func(context.Context) error { return errors.New("nope") })
	healthy := newMgrChannel("good")
	m.AddChannel("bad", failing)
	m.AddChannel("good", healthy)

	go m.StartAll(context.Background())
	waitFor(t, "the healthy channel to start", func() bool { return healthy.starts() == 1 })
	waitFor(t, "the failure to be recorded", func() bool {
		_, ok := m.ChannelError("bad")
		return ok
	})

	if got, _ := m.ChannelError("bad"); got != genericStartError {
		t.Errorf("error = %q, want the generic fallback %q", got, genericStartError)
	}
	status := m.GetStatus()
	if status["bad"].State != ChannelStateFailed {
		t.Errorf("bad state = %q, want failed", status["bad"].State)
	}
	if status["good"].State != ChannelStateRunning {
		t.Errorf("good state = %q, want running", status["good"].State)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.StopAll(ctx); err != nil {
		t.Fatal(err)
	}
}

// A channel-provided start error message replaces the generic one
// (manager.py:384-385).
func TestStartChannelUsesTheChannelErrorMessage(t *testing.T) {
	m, _ := newMgr(t)
	ch := &startMessageChannel{mgrChannel: newMgrChannel("bad")}
	ch.setStart(func(context.Context) error { return errors.New("nope") })
	m.AddChannel("bad", ch)

	go m.StartAll(context.Background())
	waitFor(t, "the failure to be recorded", func() bool {
		_, ok := m.ChannelError("bad")
		return ok
	})
	if got, _ := m.ChannelError("bad"); got != "install the SDK" {
		t.Errorf("error = %q, want %q", got, "install the SDK")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.StopAll(ctx); err != nil {
		t.Fatal(err)
	}
}

// A restart of a previously failed channel clears the recorded error FIRST
// (manager.py:378).
func TestStartChannelClearsThePreviousError(t *testing.T) {
	m, _ := newMgr(t)
	ch := newMgrChannel("mock")
	ch.setStart(func(context.Context) error { return errors.New("nope") })
	m.AddChannel("mock", ch)
	m.SetChannelError("mock", "stale")

	m.startChannel(context.Background(), "mock", ch)

	if got, _ := m.ChannelError("mock"); got == "stale" {
		t.Error("the stale error survived; _start_channel must pop it first")
	}
}

func TestStopChannelCancelsItsSendsBeforeStoppingTheRuntime(t *testing.T) {
	m, _ := newMgr(t)
	ch := newMgrChannel("mock")
	m.AddChannel("mock", ch)

	release := make(chan struct{})
	ch.blockSend("a", release)
	ch.setStop(func(context.Context) error { return nil })

	if err := m.QueueOutbound(context.Background(), ch, core.OutboundMessage{
		Channel: "mock", ChatID: "a", Content: "1",
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the send to start", func() bool { return ch.callCount() == 1 })

	other := newMgrChannel("other")
	m.AddChannel("other", other)
	if err := m.QueueOutbound(context.Background(), other, core.OutboundMessage{
		Channel: "other", ChatID: "b", Content: "2",
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the other channel to deliver", func() bool { return other.callCount() == 1 })

	stopped, err := m.StopChannel(context.Background(), "mock")
	if err != nil {
		t.Fatalf("StopChannel: %v", err)
	}
	if !stopped {
		t.Error("StopChannel reported the runtime as absent")
	}
	ch.mu.Lock()
	sawCancelled := ch.stopSawCanceled
	ch.mu.Unlock()
	if !sawCancelled {
		t.Error("channel.Stop ran before this channel's outbound tasks were cancelled")
	}
	if got := m.outboundTailCount(); got != 0 {
		t.Errorf("outbound tails = %d, want 0", got)
	}
}

func TestStopChannelReturnsFalseForAnUnknownRuntime(t *testing.T) {
	m, _ := newMgr(t)
	stopped, err := m.StopChannel(context.Background(), "nope")
	if err != nil {
		t.Fatalf("StopChannel: %v", err)
	}
	if stopped {
		t.Error("StopChannel reported an unknown runtime as stopped")
	}
}

func TestStopAllWithoutStartIsHarmless(t *testing.T) {
	m, _ := newMgr(t)
	m.AddChannel("mock", newMgrChannel("mock"))
	if err := m.StopAll(context.Background()); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if m.Started() {
		t.Error("Started() = true")
	}
}

// ---------------------------------------------------------------------------
// get_status (manager.py:1062-1096)
// ---------------------------------------------------------------------------

func TestGetStatusStates(t *testing.T) {
	m, _ := newMgr(t)
	running := newMgrChannel("running")
	running.SetRunning(true)
	starting := newMgrChannel("starting")
	failed := newMgrChannel("failed")
	m.AddChannel("running", running)
	m.AddChannel("starting", starting)
	m.AddChannel("failed", failed)
	m.SetChannelError("failed", "boom")

	// "starting" needs a live task entry.
	m.mu.Lock()
	m.tasks["starting"] = &channelTask{cancel: func() {}, done: make(chan struct{})}
	m.mu.Unlock()

	status := m.GetStatus()
	if got := status["running"].State; got != ChannelStateRunning {
		t.Errorf("running state = %q, want running", got)
	}
	if !status["running"].Running {
		t.Error("running.running = false")
	}
	if got := status["starting"].State; got != ChannelStateStarting {
		t.Errorf("starting state = %q, want starting", got)
	}
	if got := status["failed"].State; got != ChannelStateFailed {
		t.Errorf("failed state = %q, want failed", got)
	}
	if got := status["failed"].Error; got != "boom" {
		t.Errorf("failed error = %q, want boom", got)
	}
	for name, st := range status {
		if !st.Enabled {
			t.Errorf("%s.enabled = false; the reference only reports enabled runtimes", name)
		}
		if st.Owner != name {
			t.Errorf("%s.owner = %q, want %q", name, st.Owner, name)
		}
		if st.InstanceID != "default" {
			t.Errorf("%s.instance_id = %q, want default", name, st.InstanceID)
		}
	}
}

func TestGetStatusStoppedWhenTheTaskIsDone(t *testing.T) {
	m, _ := newMgr(t)
	m.AddChannel("mock", newMgrChannel("mock"))

	done := make(chan struct{})
	close(done)
	m.mu.Lock()
	m.tasks["mock"] = &channelTask{cancel: func() {}, done: done}
	m.mu.Unlock()

	if got := m.GetStatus()["mock"].State; got != ChannelStateStopped {
		t.Errorf("state = %q, want stopped", got)
	}
}

// An empty recorded error is not an error: the reference keys the field's
// presence on truthiness (manager.py:1079, :1094).
func TestGetStatusIgnoresAnEmptyError(t *testing.T) {
	m, _ := newMgr(t)
	m.AddChannel("mock", newMgrChannel("mock"))
	m.SetChannelError("mock", "")

	st := m.GetStatus()["mock"]
	if st.State == ChannelStateFailed {
		t.Error("state = failed for an empty error string")
	}
	if st.Error != "" {
		t.Errorf("error = %q, want empty", st.Error)
	}
}

// A runtime whose plugin failed to load has a spec but no channel: it still
// appears in the status (manager.py:1064-1077).
func TestGetStatusIncludesSpecsWithNoChannel(t *testing.T) {
	m, _ := newMgr(t)
	live := newMgrChannel("feishu.a")
	live.SetRunning(true)
	m.AddChannelInstance("feishu.a", "feishu", "a", live)
	m.SetChannelRuntimeSpec("feishu.b", "feishu", "b")
	m.SetChannelError("feishu.b", "boom")

	status := m.GetStatus()
	if len(status) != 2 {
		t.Fatalf("status entries = %d, want 2", len(status))
	}
	if got := status["feishu.a"]; got.Owner != "feishu" || got.InstanceID != "a" || got.State != ChannelStateRunning {
		t.Errorf("feishu.a = %+v", got)
	}
	if got := status["feishu.b"]; got.Owner != "feishu" || got.InstanceID != "b" ||
		got.State != ChannelStateFailed || got.Running {
		t.Errorf("feishu.b = %+v", got)
	}
	if got := m.StatusOrder(); !equalStrings(got, []string{"feishu.a", "feishu.b"}) {
		t.Errorf("status order = %v, want [feishu.a feishu.b]", got)
	}
}

// A channel with no recorded spec falls back to its own name as the owner and
// "default" as the instance id: Python's `owners.get(runtime_name, runtime_name)`
// and `specs.get(runtime_name, (owner, "default"))` (manager.py:1070-1077).
//
// The differential dump cannot reach this arm — _init_channels always writes the
// owner and the spec together — so it is covered here instead, by writing the
// owner map the way that pair of assignments would.
func TestGetStatusFallsBackForAChannelWithoutASpec(t *testing.T) {
	m, _ := newMgr(t)
	m.AddChannel("mystery", newMgrChannel("mystery"))

	m.mu.Lock()
	m.owners["mystery"] = "plugin"
	m.mu.Unlock()

	got := m.GetStatus()["mystery"]
	if got.Owner != "plugin" {
		t.Errorf("owner = %q, want plugin (the recorded owner wins over the name)", got.Owner)
	}
	if got.InstanceID != "default" {
		t.Errorf("instance_id = %q, want default", got.InstanceID)
	}
	if got.State != ChannelStateStopped {
		t.Errorf("state = %q, want stopped", got.State)
	}

	// And with no owner recorded at all, the name is the owner.
	m2, _ := newMgr(t)
	m2.AddChannel("mystery", newMgrChannel("mystery"))
	if got := m2.GetStatus()["mystery"].Owner; got != "mystery" {
		t.Errorf("owner = %q, want mystery", got)
	}
}

func TestEnabledChannelsKeepsInsertionOrder(t *testing.T) {
	m, _ := newMgr(t)
	m.AddChannel("b", newMgrChannel("b"))
	m.AddChannel("a", newMgrChannel("a"))
	m.AddChannel("b", newMgrChannel("b")) // reassignment keeps the position
	if got := m.EnabledChannels(); !equalStrings(got, []string{"b", "a"}) {
		t.Errorf("enabled channels = %v, want [b a]", got)
	}
	if _, ok := m.GetChannel("a"); !ok {
		t.Error("GetChannel(a) reported the channel as absent")
	}
	m.RemoveChannel("b")
	if got := m.EnabledChannels(); !equalStrings(got, []string{"a"}) {
		t.Errorf("after removal = %v, want [a]", got)
	}
}
