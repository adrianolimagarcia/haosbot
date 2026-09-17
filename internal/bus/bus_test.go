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

type testEvent struct{ n int }

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
