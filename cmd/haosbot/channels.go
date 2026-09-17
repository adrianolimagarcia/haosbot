package main

// Channel wiring for the gateway.
//
// The reference constructs its channels inside ChannelManager._init_channels
// (nanobot/channels/manager.py:246-311), which belongs to the plugin-discovery
// phase (contracts.py + plugin.py + registry.py) that this port does not
// implement — see the SCOPE note at the top of internal/channels/manager.go.
// This file is the minimum of that phase the gateway needs, and nothing more:
//
//   - the plugin REGISTRY is the static channelBuilders table below. The port
//     ships one channel runtime, Telegram.
//   - the ACTIVATION rule is _channel_section (manager.py:152-182) plus
//     channel_instance_specs (manager.py:258-265, contracts.py:337-356): a
//     channel is active only when its `channels.<name>` section exists AND
//     resolves to enabled, where an omitted `enabled` falls back to the
//     plugin's default_enabled — false for every channel here (plugin.py:38;
//     only the reference's websocket channel declares true).
//
// Deliberately absent, with the reason: multi-instance channels
// (contracts.py:73-109), the channel dependency gate
// (optional_features.ensure_enabled_channel_dependencies, manager.py:279), hot
// reload (manager.py:429-596) and the reference's other 16 channels. A
// `channels.<name>` key with no runtime here is ignored exactly as the
// reference ignores a config key that has no plugin: _init_channels iterates
// the discovered plugins, never the configuration keys (manager.py:254-257).

import (
	"encoding/json"
	"log/slog"
	"sort"

	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/telegram"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
)

// channelLoadError is the public message recorded when a channel's runtime
// cannot be constructed. It is the literal at manager.py:305, and it is what
// get_status reports; the underlying error goes to the log.
const channelLoadError = "Channel runtime could not be loaded. Check gateway logs."

// defaultChannelEnabled is ChannelPlugin.default_enabled (plugin.py:38). No
// channel this port implements overrides it, so a channel section needs an
// explicit `enabled` to activate its runtime.
const defaultChannelEnabled = false

// channelBuilder constructs one channel runtime from its decoded configuration
// section. Python: `plugin.load_channel_class()(section, self.bus)`
// (manager.py:206, :289-291).
type channelBuilder func(section channels.Section, publisher channels.InboundPublisher) (channels.Channel, error)

// channelBuilders is the port's channel registry, keyed by the
// `channels.<name>` configuration key — the Go stand-in for the reference's
// package scan (registry.py:16-38) and its lazily imported runtime
// (plugin.py:74-94).
var channelBuilders = map[string]channelBuilder{
	telegram.ChannelName: func(section channels.Section, publisher channels.InboundPublisher) (channels.Channel, error) {
		return telegram.New(section, publisher)
	},
}

// buildChannelManager constructs the channel manager and registers every
// channel the configuration activates.
//
// A channel that cannot be constructed does NOT fail this call: the reference
// catches the construction error per channel, records it and keeps going
// (manager.py:303-310), so a broken channel section cannot stop the gateway
// from starting. The failure is recorded on the manager — which is what
// GetStatus reports — and logged here with the underlying error.
func buildChannelManager(cfg *config.Config, messageBus *bus.Bus) *channels.ChannelManager {
	manager := channels.New(cfg, messageBus)

	for _, name := range channelRuntimeNames() {
		section, ok := channelSection(cfg, name)
		if !ok {
			// `_channel_section` returns None: the section is absent and the
			// channel's default is disabled, so it is not an activation.
			continue
		}
		if !channelEnabled(section) {
			// `channel_instance_specs(..., enabled_only=True)` yields no spec
			// for a section that does not resolve to enabled (contracts.py:349-353).
			continue
		}

		ch, err := channelBuilders[name](channels.NewMapSection(section), messageBus)
		if err != nil {
			manager.SetChannelRuntimeSpec(name, name, "default")
			manager.SetChannelError(name, channelLoadError)
			slog.Warn("channel not available", "channel", name, "error", err)
			continue
		}
		manager.AddChannelInstance(name, name, "default", ch)
	}

	return manager
}

// channelRuntimeNames lists the registered runtimes in a deterministic order.
//
// The reference iterates the discovered plugins, whose order comes from the
// package scan; Go map iteration is randomised, and registration order is what
// the manager reports through EnabledChannels, so it is sorted explicitly.
func channelRuntimeNames() []string {
	names := make([]string, 0, len(channelBuilders))
	for name := range channelBuilders {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// channelSection returns the decoded `channels.<name>` section.
//
// Python: `getattr(config.channels, name, None)` (manager.py:158).
// ChannelsConfig declares extra="allow" (internal/config/schema.go:229-240),
// so the section is the raw decoded JSON object — the dict path of
// BaseChannel's duck-typing (base.py:229-245), which is the only path a
// persisted configuration can take.
//
// A section that is not a JSON object (a string, a list, a number) is not an
// activation: ChannelActivation.from_config finds no mapping, falls back to
// getattr(section, "enabled") on a non-model value, and resolves to the
// disabled default (contracts.py:79-84, :105-109).
func channelSection(cfg *config.Config, name string) (map[string]any, bool) {
	raw, ok := cfg.Channels.Extra[name]
	if !ok || raw == nil {
		return nil, false
	}
	section, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	return section, true
}

// channelEnabled resolves a section's activation state for a single-instance
// channel. Port of ChannelActivation.from_config + resolve (contracts.py:66-109):
// the section's own `enabled` value wins when the key is present, otherwise the
// plugin default applies.
//
// The test is PYTHON truthiness, not a bool assertion, because the reference
// writes `bool(raw_enabled)` (contracts.py:88-89): the string "false" enables
// the channel. Values decoded from JSON are the only inputs here.
func channelEnabled(section map[string]any) bool {
	raw, ok := section["enabled"]
	if !ok {
		return defaultChannelEnabled
	}
	return pyTruthy(raw)
}

// pyTruthy reproduces Python's bool() for the values a decoded JSON object can
// hold.
//
// internal/channels has the same function, unexported, and this package must
// not modify that file; the implementation is deliberately identical to
// section.go's pyTruthy, exactly as internal/channels/telegram/policy.go:139
// already does for its own use.
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	case float32:
		return t != 0
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			// A non-numeric json.Number cannot come from encoding/json, but a
			// caller could construct one; a non-empty string is truthy.
			return t.String() != ""
		}
		return f != 0
	case []any:
		return len(t) != 0
	case []string:
		return len(t) != 0
	case map[string]any:
		return len(t) != 0
	}
	// An arbitrary Go value has no Python __bool__/__len__ to consult; Python
	// treats an object with neither as truthy, so this is the faithful default.
	return true
}
