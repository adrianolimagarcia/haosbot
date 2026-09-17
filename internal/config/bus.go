package config

// Message-bus capacity limits.
//
// DELIBERATE DIVERGENCE FROM THE REFERENCE. nanobot/bus/queue.py:32-33 builds
// its inbound and outbound queues as bare `asyncio.Queue()`, i.e. unbounded, and
// no other queue-size setting exists in the reference config schema. There is
// therefore no upstream name, semantic or default to mirror here.
//
// The Go port must not run unbounded by default: an unbounded queue turns a
// stalled consumer into unbounded memory growth, which is a real failure mode on
// the small devices this port targets. The limits are non-zero by default and
// configurable under `gateway` (see GatewayConfig), and 0 remains available as
// the explicit opt-out that reproduces the reference's unbounded behavior.
//
// 1024 is a safety valve, not a capacity target: it is orders of magnitude
// above any realistic burst for a single gateway, while still bounding the
// worst case. A queue that reaches it reports bus.ErrFull to the producer, so
// the bound is observable rather than a silent drop.

import "github.com/adrianolimagarcia/nanobot-go/internal/bus"

const (
	// DefaultMaxInboundQueue is the pending-inbound cap applied when
	// gateway.maxInboundQueue is absent.
	DefaultMaxInboundQueue = 1024
	// DefaultMaxOutboundQueue is the pending-outbound cap applied when
	// gateway.maxOutboundQueue is absent.
	DefaultMaxOutboundQueue = 1024
)

// BusOptions translates the configured gateway queue limits into bus options.
//
// It is the single wiring point between the configuration file and
// internal/bus: cmd/haosbot/runtime.go passes the result straight to bus.New, so
// a running gateway is always bounded unless the operator explicitly sets a
// limit to 0.
func (c *Config) BusOptions() bus.Options {
	return bus.Options{
		MaxInbound:  c.Gateway.MaxInboundQueue,
		MaxOutbound: c.Gateway.MaxOutboundQueue,
	}
}
