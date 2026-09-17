package config

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// TestGatewayQueueLimitsDefaultNonZero is the regression test for the dead
// Options.MaxInbound/MaxOutbound wiring: the bus honours the limits, but no
// caller ever set them, so every deployed gateway ran unbounded.
//
// The reference has no equivalent setting (nanobot/bus/queue.py:32-33 uses bare
// asyncio.Queue()), so these defaults are a deliberate divergence; the point of
// this test is that they are non-zero and reach a running bus.
func TestGatewayQueueLimitsDefaultNonZero(t *testing.T) {
	t.Setenv("TZ", "UTC")

	cases := []struct {
		name string
		cfg  *Config
	}{
		{"DefaultConfig", DefaultConfig()},
		{"load of an empty document", mustLoad(t, `{}`)},
		{"load of a gateway section without the limits", mustLoad(t, `{"gateway":{"port":1234}}`)},
	}
	for _, tc := range cases {
		if tc.cfg.Gateway.MaxInboundQueue <= 0 {
			t.Errorf("%s: gateway.maxInboundQueue = %d, want a non-zero default",
				tc.name, tc.cfg.Gateway.MaxInboundQueue)
		}
		if tc.cfg.Gateway.MaxOutboundQueue <= 0 {
			t.Errorf("%s: gateway.maxOutboundQueue = %d, want a non-zero default",
				tc.name, tc.cfg.Gateway.MaxOutboundQueue)
		}
	}
}

// TestBusOptionsBoundTheProductionBus drives the limits through the exact call
// the gateway runtime makes (cmd/haosbot/runtime.go: bus.New(cfg.BusOptions()))
// rather than constructing a Bus with hand-written options, which would only
// re-prove that the bus honours a limit it is given.
func TestBusOptionsBoundTheProductionBus(t *testing.T) {
	t.Setenv("TZ", "UTC")
	cfg := mustLoad(t, `{"gateway":{"maxInboundQueue":3,"maxOutboundQueue":2}}`)
	if got := cfg.Gateway.MaxInboundQueue; got != 3 {
		t.Fatalf("maxInboundQueue = %d, want 3", got)
	}
	if got := cfg.Gateway.MaxOutboundQueue; got != 2 {
		t.Fatalf("maxOutboundQueue = %d, want 2", got)
	}

	b := bus.New(cfg.BusOptions())
	defer b.Close()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := b.PublishInbound(ctx, core.InboundMessage{}); err != nil {
			t.Fatalf("inbound publish %d: %v", i, err)
		}
	}
	if err := b.PublishInbound(ctx, core.InboundMessage{}); !errors.Is(err, bus.ErrFull) {
		t.Fatalf("inbound past the configured cap = %v, want bus.ErrFull", err)
	}
	for i := 0; i < 2; i++ {
		if err := b.PublishOutbound(ctx, core.OutboundMessage{}); err != nil {
			t.Fatalf("outbound publish %d: %v", i, err)
		}
	}
	if err := b.PublishOutbound(ctx, core.OutboundMessage{}); !errors.Is(err, bus.ErrFull) {
		t.Fatalf("outbound past the configured cap = %v, want bus.ErrFull", err)
	}
}

// TestBusOptionsDefaultConfigIsBounded proves the default configuration — the
// one a deployment gets with no queue settings at all — produces a bounded bus.
func TestBusOptionsDefaultConfigIsBounded(t *testing.T) {
	t.Setenv("TZ", "UTC")

	b := bus.New(DefaultConfig().BusOptions())
	defer b.Close()
	ctx := context.Background()

	for i := 0; i < DefaultMaxInboundQueue; i++ {
		if err := b.PublishInbound(ctx, core.InboundMessage{}); err != nil {
			t.Fatalf("inbound publish %d: %v", i, err)
		}
	}
	if err := b.PublishInbound(ctx, core.InboundMessage{}); !errors.Is(err, bus.ErrFull) {
		t.Fatalf("inbound past the default cap = %v, want bus.ErrFull", err)
	}

	for i := 0; i < DefaultMaxOutboundQueue; i++ {
		if err := b.PublishOutbound(ctx, core.OutboundMessage{}); err != nil {
			t.Fatalf("outbound publish %d: %v", i, err)
		}
	}
	if err := b.PublishOutbound(ctx, core.OutboundMessage{}); !errors.Is(err, bus.ErrFull) {
		t.Fatalf("outbound past the default cap = %v, want bus.ErrFull", err)
	}
}

// TestGatewayQueueLimitsZeroIsExplicitlyUnbounded keeps the reference's
// unbounded behavior reachable, but only on request.
func TestGatewayQueueLimitsZeroIsExplicitlyUnbounded(t *testing.T) {
	t.Setenv("TZ", "UTC")
	cfg := mustLoad(t, `{"gateway":{"maxInboundQueue":0,"maxOutboundQueue":0}}`)

	b := bus.New(cfg.BusOptions())
	defer b.Close()
	ctx := context.Background()
	for i := 0; i < 2000; i++ {
		if err := b.PublishInbound(ctx, core.InboundMessage{}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	if got := b.InboundSize(); got != 2000 {
		t.Fatalf("inbound size = %d, want 2000 (0 must mean unbounded)", got)
	}
}

// TestGatewayQueueLimitsRejectNegative mirrors the ge=0 bound the decoder
// applies to every other numeric setting.
func TestGatewayQueueLimitsRejectNegative(t *testing.T) {
	t.Setenv("TZ", "UTC")
	if _, err := loadFromJSON(t, `{"gateway":{"maxInboundQueue":-1}}`); err == nil {
		t.Fatal("negative maxInboundQueue must be rejected")
	}
	if _, err := loadFromJSON(t, `{"gateway":{"maxOutboundQueue":-1}}`); err == nil {
		t.Fatal("negative maxOutboundQueue must be rejected")
	}
}

// TestGatewayQueueLimitsAcceptSnakeAndCamel pins the alias handling every other
// config field gets from the reference's to_camel alias generator.
func TestGatewayQueueLimitsAcceptSnakeAndCamel(t *testing.T) {
	t.Setenv("TZ", "UTC")
	for _, key := range []string{"maxInboundQueue", "max_inbound_queue"} {
		cfg := mustLoad(t, `{"gateway":{"`+key+`":7}}`)
		if got := cfg.Gateway.MaxInboundQueue; got != 7 {
			t.Errorf("%s did not bind: got %d", key, got)
		}
	}
	for _, key := range []string{"maxOutboundQueue", "max_outbound_queue"} {
		cfg := mustLoad(t, `{"gateway":{"`+key+`":9}}`)
		if got := cfg.Gateway.MaxOutboundQueue; got != 9 {
			t.Errorf("%s did not bind: got %d", key, got)
		}
	}
}

// TestSavedQueueLimitsStayOutOfTheRootObject pins the reason these settings live
// under `gateway` and not at the root of the document.
//
// The reference's root Config is a BaseSettings with extra="forbid"
// (schema.py:422), so a root-level key makes a config file written by this port
// fail to load upstream ("Unknown setting"). GatewayConfig descends from `Base`,
// whose pydantic default is extra="ignore", so the reference loads these keys
// and ignores them. Verified against upstream 1bb712d3: load_config accepts a
// config saved here, and rejects the same document with the keys moved to the
// root.
func TestSavedQueueLimitsStayOutOfTheRootObject(t *testing.T) {
	t.Setenv("TZ", "UTC")

	raw, err := marshalNoEscape(DefaultConfig())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"maxInboundQueue", "maxOutboundQueue", "max_inbound_queue", "max_outbound_queue"} {
		if _, atRoot := doc[key]; atRoot {
			t.Errorf("%s must not be serialized at the root of the config document", key)
		}
	}

	gateway, ok := doc["gateway"]
	if !ok {
		t.Fatal("saved config has no gateway object")
	}
	var gw map[string]json.RawMessage
	if err := json.Unmarshal(gateway, &gw); err != nil {
		t.Fatalf("unmarshal gateway: %v", err)
	}
	for _, key := range []string{"maxInboundQueue", "maxOutboundQueue"} {
		if _, inGateway := gw[key]; !inGateway {
			t.Errorf("%s missing from the saved gateway object", key)
		}
	}
}
