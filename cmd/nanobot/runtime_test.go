package main

import (
	"context"
	"errors"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// TestLegacyRuntimeBusIsBounded pins the legacy `nanobot` entrypoint to the same
// configured queue limits as `haosbot`.
//
// cmd/nanobot/runtime.go used to build its bus with `bus.New(bus.Options{})`,
// and a zero Options field means UNBOUNDED (internal/bus/bus.go:8-12). The
// legacy binary therefore ran with queues that grow without limit whenever its
// consumer stalls — exactly the failure mode config.BusOptions exists to
// prevent (internal/config/bus.go). The gateway entrypoint already passed
// cfg.BusOptions(); this test keeps the two entrypoints from drifting apart
// again.
//
// The assertion is behavioural rather than structural: it drives the bus the
// runtime actually built, through the real buildRuntime path, and requires the
// configured cap to be observable as bus.ErrFull. A structural check on the
// Options value would not catch a bus that ignores its options.
func TestLegacyRuntimeBusIsBounded(t *testing.T) {
	// DefaultDataDir() derives from $HOME; isolate it so the test never touches
	// a real ~/.nanobot or ~/.haosbot installation.
	t.Setenv("HOME", t.TempDir())

	// Small caps keep the probe short. The point is not the value 1024 but that
	// the runtime honours the configured value at all.
	const inCap, outCap = 4, 3

	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Gateway.MaxInboundQueue = inCap
	cfg.Gateway.MaxOutboundQueue = outCap

	rt, err := buildRuntime(cfg)
	if err != nil {
		t.Fatalf("buildRuntime: %v", err)
	}
	defer rt.Close()

	ctx := context.Background()

	for i := 0; i < inCap; i++ {
		if err := rt.bus.PublishInbound(ctx, core.InboundMessage{Channel: "test", ChatID: "1"}); err != nil {
			t.Fatalf("inbound publish %d of %d: %v", i+1, inCap, err)
		}
	}
	if err := rt.bus.PublishInbound(ctx, core.InboundMessage{Channel: "test", ChatID: "1"}); !errors.Is(err, bus.ErrFull) {
		t.Errorf("inbound publish past the configured cap of %d returned %v, want bus.ErrFull; "+
			"the legacy runtime built an unbounded bus", inCap, err)
	}

	for i := 0; i < outCap; i++ {
		if err := rt.bus.PublishOutbound(ctx, core.OutboundMessage{Channel: "test", ChatID: "1"}); err != nil {
			t.Fatalf("outbound publish %d of %d: %v", i+1, outCap, err)
		}
	}
	if err := rt.bus.PublishOutbound(ctx, core.OutboundMessage{Channel: "test", ChatID: "1"}); !errors.Is(err, bus.ErrFull) {
		t.Errorf("outbound publish past the configured cap of %d returned %v, want bus.ErrFull; "+
			"the legacy runtime built an unbounded bus", outCap, err)
	}
}
