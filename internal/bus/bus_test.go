package bus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

func TestInboundFIFO(t *testing.T) {
	b := New(Options{})
	defer b.Close()
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := b.PublishInbound(ctx, core.InboundMessage{Channel: "c", ChatID: "1", Content: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	if got := b.InboundSize(); got != 5 {
		t.Fatalf("size = %d, want 5", got)
	}
	for i := 0; i < 5; i++ {
		m, err := b.ConsumeInbound(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := string(rune('a' + i))
		if m.Content != want {
			t.Errorf("msg %d = %q, want %q", i, m.Content, want)
		}
	}
	if got := b.InboundSize(); got != 0 {
		t.Errorf("size after drain = %d", got)
	}
}

func TestConsumeBlocksUntilPublish(t *testing.T) {
	b := New(Options{})
	defer b.Close()
	ctx := context.Background()

	done := make(chan core.InboundMessage, 1)
	go func() {
		m, err := b.ConsumeInbound(ctx)
		if err == nil {
			done <- m
		}
	}()

	// Give the consumer time to block, then publish.
	time.Sleep(20 * time.Millisecond)
	if err := b.PublishInbound(ctx, core.InboundMessage{Content: "wake"}); err != nil {
		t.Fatal(err)
	}

	select {
	case m := <-done:
		if m.Content != "wake" {
			t.Errorf("got %q", m.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consumer did not wake")
	}
}

func TestConsumeRespectsContextCancel(t *testing.T) {
	b := New(Options{})
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := b.ConsumeInbound(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestCloseUnblocksWaiters(t *testing.T) {
	b := New(Options{})

	done := make(chan error, 1)
	go func() {
		_, err := b.ConsumeInbound(context.Background())
		done <- err
	}()

	time.Sleep(20 * time.Millisecond)
	b.Close()

	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err = %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not unblocked by Close")
	}

	if err := b.PublishInbound(context.Background(), core.InboundMessage{}); !errors.Is(err, ErrClosed) {
		t.Errorf("publish after close = %v, want ErrClosed", err)
	}
}

// TestMultipleWaitersAllWake is the regression test for the missed-wakeup
// hazard that a single buffered signal channel would introduce.
func TestMultipleWaitersAllWake(t *testing.T) {
	b := New(Options{})
	defer b.Close()
	ctx := context.Background()

	const waiters = 8
	var got atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				_, err := b.ConsumeInbound(ctx)
				if err != nil {
					return
				}
				got.Add(1)
			}
		}()
	}

	// Publish exactly as many messages as waiters, in small batches.
	for i := 0; i < waiters; i++ {
		if err := b.PublishInbound(ctx, core.InboundMessage{Content: "x"}); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.After(5 * time.Second)
	for got.Load() < waiters {
		select {
		case <-deadline:
			t.Fatalf("only %d/%d messages consumed", got.Load(), waiters)
		case <-time.After(10 * time.Millisecond):
		}
	}
	b.Close()
	wg.Wait()
}

func TestBoundedQueueRejectsWhenFull(t *testing.T) {
	b := New(Options{MaxInbound: 2})
	defer b.Close()
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := b.PublishInbound(ctx, core.InboundMessage{}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	if err := b.PublishInbound(ctx, core.InboundMessage{}); !errors.Is(err, ErrFull) {
		t.Fatalf("err = %v, want ErrFull", err)
	}
	// Draining frees capacity.
	if _, err := b.ConsumeInbound(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.PublishInbound(ctx, core.InboundMessage{}); err != nil {
		t.Fatalf("publish after drain: %v", err)
	}
}

func TestUnboundedByDefault(t *testing.T) {
	b := New(Options{})
	defer b.Close()
	ctx := context.Background()
	for i := 0; i < 10000; i++ {
		if err := b.PublishInbound(ctx, core.InboundMessage{}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	if got := b.InboundSize(); got != 10000 {
		t.Fatalf("size = %d, want 10000", got)
	}
}

type testEvent struct{}

func (testEvent) EventName() string { return "test" }

func TestSubscribeOrderedDispatch(t *testing.T) {
	b := New(Options{})
	defer b.Close()

	var mu sync.Mutex
	var order []int
	for i := 0; i < 3; i++ {
		i := i
		b.Subscribe(func(ctx context.Context, e Event) {
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
		})
	}

	b.Publish(context.Background(), testEvent{})

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 {
		t.Fatalf("got %d calls", len(order))
	}
	for i, v := range order {
		if v != i {
			t.Errorf("order[%d] = %d, want %d", i, v, i)
		}
	}
}

func TestUnsubscribeIsIdempotent(t *testing.T) {
	b := New(Options{})
	defer b.Close()

	var calls atomic.Int64
	unsub := b.Subscribe(func(ctx context.Context, e Event) { calls.Add(1) })

	b.Publish(context.Background(), testEvent{})
	unsub()
	unsub() // must not panic
	b.Publish(context.Background(), testEvent{})

	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
}

// TestUnsubscribePeerSuppressesItMidDispatch pins the reference's call-time
// activity check.
//
// The reference snapshots the handler LIST at the top of publish
// (queue.py:120, `list(self._handlers)`), but every entry re-checks its own
// `active` flag at the moment it is CALLED (queue.py:104-107). A handler that
// unsubscribes a peer therefore suppresses that peer for the dispatch already
// in progress.
//
// This port must snapshot the list, not the active set. Resolving the active
// set up front (the previous revision) decides that B is live before A ever
// runs, so B is called anyway — a divergence the reference never exhibits.
//
// The late-subscription assertion is not the discriminator (it holds in both
// revisions); it guards the fix against the over-correction of re-reading
// b.handlers on each iteration instead of snapshotting it, which would both
// diverge from the reference and deadlock on the handler's own unsubscribe.
func TestUnsubscribePeerSuppressesItMidDispatch(t *testing.T) {
	b := New(Options{})
	defer b.Close()
	ctx := context.Background()

	var peerCalls atomic.Int64
	var lateSubscribed atomic.Int64
	var unsubPeer func()

	b.Subscribe(func(context.Context, Event) {
		// A unsubscribes its peer B from inside the dispatch, then registers
		// a handler of its own.
		unsubPeer()
		b.Subscribe(func(context.Context, Event) { lateSubscribed.Add(1) })
	})
	unsubPeer = b.Subscribe(func(context.Context, Event) { peerCalls.Add(1) })

	b.Publish(ctx, testEvent{})

	if got := peerCalls.Load(); got != 0 {
		t.Errorf("peer handler called %d times during the dispatch that unsubscribed it; "+
			"the reference checks active at call time and suppresses it", got)
	}
	if got := lateSubscribed.Load(); got != 0 {
		t.Errorf("handler registered during dispatch called %d times; the reference "+
			"iterates a snapshot of the handler list", got)
	}

	// Control, true in every revision: the peer stays suppressed afterwards.
	before := peerCalls.Load()
	b.Publish(ctx, testEvent{})
	if got := peerCalls.Load(); got != before {
		t.Errorf("peer handler called %d more times after unsubscribe", got-before)
	}
}

// TestPanickingHandlerIsIsolated ensures one bad subscriber cannot stop
// delivery to the others.
func TestPanickingHandlerIsIsolated(t *testing.T) {
	b := New(Options{})
	defer b.Close()

	var reached atomic.Bool
	b.Subscribe(func(ctx context.Context, e Event) { panic("boom") })
	b.Subscribe(func(ctx context.Context, e Event) { reached.Store(true) })

	b.Publish(context.Background(), testEvent{})

	if !reached.Load() {
		t.Error("second handler was not reached after first panicked")
	}
}

func TestOutboundRoundTrip(t *testing.T) {
	b := New(Options{})
	defer b.Close()
	ctx := context.Background()

	replyTo := "msg-1"
	want := core.OutboundMessage{
		Channel:  "telegram",
		ChatID:   "42",
		Content:  "hello",
		ReplyTo:  &replyTo,
		Media:    []string{"a.png"},
		Metadata: map[string]any{"k": "v"},
		Buttons:  [][]string{{"yes", "no"}},
	}
	if err := b.PublishOutbound(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := b.ConsumeOutbound(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != want.Content || got.Channel != want.Channel || got.ChatID != want.ChatID {
		t.Errorf("got %+v", got)
	}
	if got.ReplyTo == nil || *got.ReplyTo != replyTo {
		t.Errorf("reply_to lost: %+v", got.ReplyTo)
	}
	if len(got.Buttons) != 1 || len(got.Buttons[0]) != 2 {
		t.Errorf("buttons lost: %+v", got.Buttons)
	}
}

// TestConcurrentUnsubscribeAndPublish is the regression test for the
// unsynchronised read of subscription.active in Publish.
//
// Publish used to snapshot the handler list under b.mu and then read each
// subscription's active flag AFTER unlocking, while the unsubscribe closure
// clears that flag under the same mutex. The two accesses are then unordered,
// which is a data race; this test must be run with -race.
//
// The slow first handler keeps Publish inside the dispatch loop between its
// snapshot and the read of a later subscription's flag, so the overlap is
// deterministic rather than left to scheduling luck.
func TestConcurrentUnsubscribeAndPublish(t *testing.T) {
	b := New(Options{})
	defer b.Close()
	ctx := context.Background()

	// Signals that Publish has taken its snapshot and entered the first handler.
	inHandler := make(chan struct{}, 1)
	b.Subscribe(func(context.Context, Event) {
		select {
		case inHandler <- struct{}{}:
		default:
		}
		time.Sleep(2 * time.Millisecond)
	})

	const rounds = 25
	for i := 0; i < rounds; i++ {
		victim := b.Subscribe(func(context.Context, Event) {})

		done := make(chan struct{})
		go func() {
			defer close(done)
			b.Publish(ctx, testEvent{})
		}()

		// By the time the first handler runs, Publish has already copied
		// b.handlers — victim is in its snapshot — and has not yet read
		// victim's active flag.
		<-inHandler
		victim()
		<-done
	}
}

// enqueueProbeCtx is a context that reports cancellation only once the bus
// queue is already non-empty, i.e. exactly in the window where the previous
// revision consulted ctx.Err() — after the message had been committed.
type enqueueProbeCtx struct {
	context.Context
	bus *Bus
	// sawEnqueued records that Err was consulted after the append, meaning the
	// publish committed before it checked the caller's context.
	sawEnqueued atomic.Bool
}

func (c *enqueueProbeCtx) Err() error {
	// White-box on purpose. Err is only called from the publishing goroutine
	// while it holds b.mu, so b.inbound cannot be inspected through
	// InboundSize (which needs the same mutex), and there is no concurrent
	// writer for the read to race with.
	if len(c.bus.inbound) > 0 {
		c.sawEnqueued.Store(true)
		return context.Canceled
	}
	return nil
}

// TestPublishInboundChecksContextBeforeEnqueue pins the check ordering: the
// context must be consulted BEFORE the message is committed. The probe only
// reports cancellation once a message is already queued, so an implementation
// that commits first and checks ctx afterwards returns an error for a message
// it has already delivered.
func TestPublishInboundChecksContextBeforeEnqueue(t *testing.T) {
	b := New(Options{})
	defer b.Close()

	ctx := &enqueueProbeCtx{Context: context.Background(), bus: b}
	if err := b.PublishInbound(ctx, core.InboundMessage{Content: "once"}); err != nil {
		t.Fatalf("publish = %v, want nil: ctx must be checked before the commit", err)
	}
	if ctx.sawEnqueued.Load() {
		t.Fatal("ctx.Err() was consulted after the message was already queued")
	}
	if got := b.InboundSize(); got != 1 {
		t.Fatalf("queue holds %d messages, want 1", got)
	}

	msg, err := b.ConsumeInbound(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if msg.Content != "once" {
		t.Errorf("content = %q, want %q", msg.Content, "once")
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := b.ConsumeInbound(drainCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second consume = %v, want the queue to be empty", err)
	}
}

// TestPublishInboundCancelledContextDoesNotEnqueue is the duplicate-delivery
// regression test. A caller whose context is already done must get an error
// AND find the queue untouched, so that retrying on that error cannot deliver
// the same message twice.
func TestPublishInboundCancelledContextDoesNotEnqueue(t *testing.T) {
	b := New(Options{})
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := b.PublishInbound(ctx, core.InboundMessage{Content: "dup"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := b.InboundSize(); got != 0 {
		t.Fatalf("queue holds %d messages after a rejected publish, want 0", got)
	}

	// The retry a caller performs on that error must deliver exactly one copy.
	if err := b.PublishInbound(context.Background(), core.InboundMessage{Content: "dup"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	msg, err := b.ConsumeInbound(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if msg.Content != "dup" {
		t.Errorf("content = %q, want %q", msg.Content, "dup")
	}

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelDrain()
	if _, err := b.ConsumeInbound(drainCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second consume = %v, want exactly one delivery", err)
	}
}

// TestPublishOutboundCancelledContextDoesNotEnqueue is the outbound twin; the
// root cause is shared, so it gets its own coverage.
func TestPublishOutboundCancelledContextDoesNotEnqueue(t *testing.T) {
	b := New(Options{})
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := b.PublishOutbound(ctx, core.OutboundMessage{Content: "dup"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := b.OutboundSize(); got != 0 {
		t.Fatalf("queue holds %d messages after a rejected publish, want 0", got)
	}

	if err := b.PublishOutbound(context.Background(), core.OutboundMessage{Content: "dup"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	msg, err := b.ConsumeOutbound(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if msg.Content != "dup" {
		t.Errorf("content = %q, want %q", msg.Content, "dup")
	}

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelDrain()
	if _, err := b.ConsumeOutbound(drainCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second consume = %v, want exactly one delivery", err)
	}
}

// TestPublishCancelAfterEnqueueIsNotAnError covers the other half of the
// contract: a cancellation that arrives once the message has been accepted
// must not retroactively turn the accepted publish into a reported failure.
func TestPublishCancelAfterEnqueueIsNotAnError(t *testing.T) {
	b := New(Options{})
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	if err := b.PublishInbound(ctx, core.InboundMessage{Content: "once"}); err != nil {
		t.Fatalf("inbound publish: %v", err)
	}
	if err := b.PublishOutbound(ctx, core.OutboundMessage{Content: "once"}); err != nil {
		t.Fatalf("outbound publish: %v", err)
	}
	cancel()

	in, err := b.ConsumeInbound(context.Background())
	if err != nil {
		t.Fatalf("consume inbound: %v", err)
	}
	if in.Content != "once" {
		t.Errorf("inbound content = %q, want %q", in.Content, "once")
	}
	out, err := b.ConsumeOutbound(context.Background())
	if err != nil {
		t.Fatalf("consume outbound: %v", err)
	}
	if out.Content != "once" {
		t.Errorf("outbound content = %q, want %q", out.Content, "once")
	}

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelDrain()
	if _, err := b.ConsumeInbound(drainCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second inbound consume = %v, want exactly one delivery", err)
	}
	if _, err := b.ConsumeOutbound(drainCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second outbound consume = %v, want exactly one delivery", err)
	}
}

// TestPublishCheckOrdering pins the documented order closed -> ctx -> capacity,
// and that ErrClosed and ErrFull still behave for a live context.
func TestPublishCheckOrdering(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	// A closed bus outranks a cancelled context: no publish can ever succeed.
	closed := New(Options{})
	closed.Close()
	if err := closed.PublishInbound(cancelled, core.InboundMessage{}); !errors.Is(err, ErrClosed) {
		t.Errorf("closed+cancelled inbound = %v, want ErrClosed", err)
	}
	if err := closed.PublishOutbound(cancelled, core.OutboundMessage{}); !errors.Is(err, ErrClosed) {
		t.Errorf("closed+cancelled outbound = %v, want ErrClosed", err)
	}

	// A cancelled context outranks a full queue, and neither enqueues.
	full := New(Options{MaxInbound: 1, MaxOutbound: 1})
	defer full.Close()
	live := context.Background()
	if err := full.PublishInbound(live, core.InboundMessage{Content: "a"}); err != nil {
		t.Fatalf("inbound fill: %v", err)
	}
	if err := full.PublishOutbound(live, core.OutboundMessage{Content: "a"}); err != nil {
		t.Fatalf("outbound fill: %v", err)
	}
	if err := full.PublishInbound(cancelled, core.InboundMessage{Content: "b"}); !errors.Is(err, context.Canceled) {
		t.Errorf("full+cancelled inbound = %v, want context.Canceled", err)
	}
	if err := full.PublishOutbound(cancelled, core.OutboundMessage{Content: "b"}); !errors.Is(err, context.Canceled) {
		t.Errorf("full+cancelled outbound = %v, want context.Canceled", err)
	}
	if got := full.InboundSize(); got != 1 {
		t.Errorf("inbound size = %d, want 1", got)
	}
	if got := full.OutboundSize(); got != 1 {
		t.Errorf("outbound size = %d, want 1", got)
	}

	// Full with a live context still reports ErrFull.
	if err := full.PublishInbound(live, core.InboundMessage{Content: "c"}); !errors.Is(err, ErrFull) {
		t.Errorf("full inbound = %v, want ErrFull", err)
	}
	if err := full.PublishOutbound(live, core.OutboundMessage{Content: "c"}); !errors.Is(err, ErrFull) {
		t.Errorf("full outbound = %v, want ErrFull", err)
	}
}

// TestConcurrentPublishConsume exercises the queue under the race detector.
func TestConcurrentPublishConsume(t *testing.T) {
	b := New(Options{})
	defer b.Close()
	ctx := context.Background()

	const producers = 4
	const perProducer = 500

	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				_ = b.PublishInbound(ctx, core.InboundMessage{Content: "m"})
			}
		}()
	}

	var consumed atomic.Int64
	var cwg sync.WaitGroup
	cwg.Add(1)
	go func() {
		defer cwg.Done()
		for consumed.Load() < producers*perProducer {
			if _, err := b.ConsumeInbound(ctx); err != nil {
				return
			}
			consumed.Add(1)
		}
	}()

	wg.Wait()
	cwg.Wait()
	if got := consumed.Load(); got != producers*perProducer {
		t.Errorf("consumed %d, want %d", got, producers*perProducer)
	}
}

// ---------------------------------------------------------------------------
// Consumer observation
// ---------------------------------------------------------------------------

// HasConsumer is the readiness observable: it must be false until something
// actually enters ConsumeInbound, and true from then on.
func TestHasConsumerReportsWhetherAnythingDrains(t *testing.T) {
	b := New(Options{})
	defer b.Close()

	if b.HasConsumer() {
		t.Fatal("a bus nobody has consumed from reported a consumer")
	}
	if got := b.ConsumerCount(); got != 0 {
		t.Fatalf("ConsumerCount()=%d on a fresh bus, want 0", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	consumed := make(chan struct{})
	go func() {
		close(consumed)
		_, _ = b.ConsumeInbound(ctx)
	}()

	<-consumed
	deadline := time.Now().Add(5 * time.Second)
	for !b.HasConsumer() {
		if time.Now().After(deadline) {
			t.Fatal("HasConsumer() stayed false while a consumer was blocked in ConsumeInbound")
		}
		time.Sleep(time.Millisecond)
	}
	if got := b.ConsumerCount(); got != 1 {
		t.Fatalf("ConsumerCount()=%d with one consumer parked in the queue, want 1", got)
	}

	// The count is in flight, the flag is historical: a consumer that is busy
	// delivering is not parked any more, but it has not gone away either.
	cancel()
	deadline = time.Now().Add(5 * time.Second)
	for b.ConsumerCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("ConsumerCount() never returned to 0 after the consumer stopped")
		}
		time.Sleep(time.Millisecond)
	}
	if !b.HasConsumer() {
		t.Fatal("HasConsumer() went back to false after a consumer had attached")
	}
}

// The counters are read by a probe while the bus is in use, so they must be
// race-free and must not disturb dispatch.
func TestConsumerObservationRacesWithTraffic(t *testing.T) {
	b := New(Options{})
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var consumers sync.WaitGroup
	for i := 0; i < 4; i++ {
		consumers.Add(1)
		go func() {
			defer consumers.Done()
			for {
				if _, err := b.ConsumeInbound(ctx); err != nil {
					return
				}
			}
		}()
	}

	// Observers read the counters while the bus is in use; the race detector is
	// the assertion for them.
	var observers sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		observers.Add(1)
		go func() {
			defer observers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = b.HasConsumer()
				_ = b.ConsumerCount()
			}
		}()
	}

	for i := 0; i < 500; i++ {
		if err := b.PublishInbound(ctx, core.InboundMessage{Channel: "c", ChatID: "1"}); err != nil {
			t.Fatalf("PublishInbound: %v", err)
		}
	}

	// The queue draining is what proves the consumers really ran, so the
	// assertion below cannot pass on a goroutine that was never scheduled.
	deadline := time.Now().Add(5 * time.Second)
	for b.InboundSize() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the consumers did not drain the queue: %d pending", b.InboundSize())
		}
		time.Sleep(time.Millisecond)
	}
	if !b.HasConsumer() {
		t.Fatal("HasConsumer() is false after consumers drained the bus")
	}

	cancel()
	consumers.Wait()
	close(stop)
	observers.Wait()
}
