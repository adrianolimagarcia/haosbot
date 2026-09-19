package compat

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
)

// This file is deliberately self-contained: its own dumper
// (compat/python/dump_bus_subscribe.py) and its own loader. It shares no
// declarations with differential_test.go beyond repoRoot, because that file and
// dump_reference.py are edited concurrently by several agents and a
// read-modify-write race there would destroy work.
//
// Every expectation comes from EXECUTING the frozen reference
// (upstream/nanobot @ 1bb712d3), never from documentation or from reading the
// Python by eye.
//
// The scenario this exists for is "peer_unsubscribed_mid_dispatch": the
// reference checks each subscriber's active flag at CALL time (queue.py:104-107)
// while iterating a snapshot of the handler list (queue.py:120), so a handler
// that unsubscribes a peer suppresses that peer for the dispatch in progress.
// internal/bus once resolved the active set up front instead, which called the
// peer anyway — the divergence this test detects.

// busDumperPath is the dumper this file drives.
func busDumperPath(root string) string {
	return filepath.Join(root, "compat", "python", "dump_bus_subscribe.py")
}

// loadBusDump runs the reference dumper and returns its parsed document.
func loadBusDump(t *testing.T) map[string]any {
	t.Helper()
	root := repoRoot(t)
	python := filepath.Join(root, ".tools", "venv", "bin", "python")
	script := busDumperPath(root)

	if _, err := os.Stat(python); err != nil {
		t.Skipf("SKIP: reference venv not present at %s — differential check not run", python)
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("SKIP: dumper missing at %s", script)
	}

	// Stdout only: the reference logs the deliberately-raising handler to
	// stderr through loguru, which is expected and must not reach the parser.
	// runReferenceCommand captures stderr but returns only stdout on success.
	out, err := runReferenceCommand("bus dumper", []string{python, script}, root, nil)
	if err != nil {
		t.Fatalf("bus dumper failed: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("dumper output is not valid JSON: %v\n%s", err, out)
	}
	return doc
}

// busProbeEvent is the Go counterpart of the dumper's ProbeEvent.
type busProbeEvent struct{}

func (busProbeEvent) EventName() string { return "probe" }

// goBusScenarios replays the dumper's scenarios against internal/bus.
//
// Each function mirrors its Python namesake line for line, including the fact
// that the call log is cumulative across publishes.
func goBusScenarios() map[string]any {
	ctx := context.Background()

	peerUnsubscribedMidDispatch := func() []string {
		b := bus.New(bus.Options{})
		defer b.Close()
		var calls []string
		var unsubB func()

		b.Subscribe(func(context.Context, bus.Event) {
			calls = append(calls, "A")
			unsubB()
		})
		unsubB = b.Subscribe(func(context.Context, bus.Event) { calls = append(calls, "B") })

		b.Publish(ctx, busProbeEvent{})
		return calls
	}

	peerUnsubscribedBeforePublish := func() []string {
		b := bus.New(bus.Options{})
		defer b.Close()
		var calls []string

		b.Subscribe(func(context.Context, bus.Event) { calls = append(calls, "A") })
		unsubB := b.Subscribe(func(context.Context, bus.Event) { calls = append(calls, "B") })
		unsubB()

		b.Publish(ctx, busProbeEvent{})
		return calls
	}

	subscriptionAddedMidDispatch := func() map[string]any {
		b := bus.New(bus.Options{})
		defer b.Close()
		var calls []string
		var c func(context.Context, bus.Event)

		b.Subscribe(func(context.Context, bus.Event) {
			calls = append(calls, "A")
			b.Subscribe(c)
		})
		b.Subscribe(func(context.Context, bus.Event) { calls = append(calls, "B") })
		c = func(context.Context, bus.Event) { calls = append(calls, "C") }

		b.Publish(ctx, busProbeEvent{})
		first := append([]string(nil), calls...)
		b.Publish(ctx, busProbeEvent{})
		return map[string]any{"first": first, "second": calls}
	}

	selfUnsubscribeMidDispatch := func() map[string]any {
		b := bus.New(bus.Options{})
		defer b.Close()
		var calls []string
		var unsubA func()

		unsubA = b.Subscribe(func(context.Context, bus.Event) {
			calls = append(calls, "A")
			unsubA()
		})
		b.Subscribe(func(context.Context, bus.Event) { calls = append(calls, "B") })

		b.Publish(ctx, busProbeEvent{})
		first := append([]string(nil), calls...)
		b.Publish(ctx, busProbeEvent{})
		return map[string]any{"first": first, "second": calls}
	}

	registrationOrder := func() []string {
		b := bus.New(bus.Options{})
		defer b.Close()
		var calls []string
		for _, name := range []string{"A", "B", "C"} {
			name := name
			b.Subscribe(func(context.Context, bus.Event) { calls = append(calls, name) })
		}
		b.Publish(ctx, busProbeEvent{})
		return calls
	}

	raisingHandlerIsolated := func() []string {
		b := bus.New(bus.Options{})
		defer b.Close()
		var calls []string

		b.Subscribe(func(context.Context, bus.Event) {
			calls = append(calls, "A")
			panic("boom")
		})
		b.Subscribe(func(context.Context, bus.Event) { calls = append(calls, "B") })

		b.Publish(ctx, busProbeEvent{})
		return calls
	}

	return map[string]any{
		"peer_unsubscribed_mid_dispatch":   peerUnsubscribedMidDispatch(),
		"peer_unsubscribed_before_publish": peerUnsubscribedBeforePublish(),
		"subscription_added_mid_dispatch":  subscriptionAddedMidDispatch(),
		"self_unsubscribe_mid_dispatch":    selfUnsubscribeMidDispatch(),
		"registration_order":               registrationOrder(),
		"raising_handler_is_isolated":      raisingHandlerIsolated(),
	}
}

// TestBusDispatchMatchesPythonReference requires internal/bus to produce the
// same handler-call sequence as the frozen reference for every scenario.
//
// The Go result is round-tripped through JSON before comparison so that the two
// sides are compared as decoded values ([]any / map[string]any) rather than as
// Go types, which also pins that the shapes are JSON-compatible.
func TestBusDispatchMatchesPythonReference(t *testing.T) {
	want := loadBusDump(t)

	raw, err := json.Marshal(goBusScenarios())
	if err != nil {
		t.Fatalf("marshal Go scenarios: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal Go scenarios: %v", err)
	}

	for _, name := range []string{
		"peer_unsubscribed_mid_dispatch",
		"peer_unsubscribed_before_publish",
		"subscription_added_mid_dispatch",
		"self_unsubscribe_mid_dispatch",
		"registration_order",
		"raising_handler_is_isolated",
	} {
		if _, ok := want[name]; !ok {
			t.Fatalf("reference dumper omitted scenario %q", name)
		}
		if !reflect.DeepEqual(got[name], want[name]) {
			t.Errorf("scenario %q:\n  go        = %#v\n  reference = %#v", name, got[name], want[name])
		}
	}
}
