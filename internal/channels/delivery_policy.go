package channels

// Per-channel delivery-policy resolution.
//
// Port of the tail of ChannelManager._build_channel
// (nanobot/channels/manager.py:206-232) and of ChannelManager._resolve_bool_override
// (manager.py:355-368).
//
// WHY IT LIVES HERE, NEXT TO deliveryPolicy. The reference resolves the three
// booleans while building a channel and reads them back on the outbound path.
// This package owns the read half (deliveryPolicy, manager.go:805) and the
// storage (Base's sendProgress/sendToolHints/showReasoning, base.go:252-254), so
// the write half belongs beside them. What stays out of this package is channel
// CONSTRUCTION itself: the port deliberately does not implement plugin
// discovery, so cmd/haosbot/channels.go builds the channel and calls
// ApplyDeliveryPolicy on the result — the same order as _build_channel, which
// also assigns the three attributes after `cls(section, self.bus, **kwargs)`.
//
// THE DEFECT THIS CLOSES. Every piece existed — the fields, the setters, the
// WithDeliveryPolicy option, the read path — but no production code called any
// of the setters, so every channel kept NewBase's hardcoded true, true, true
// (base.go:303-305) and the whole delivery-policy configuration surface, global
// and per-channel alike, silently did nothing. The failure was invisible while
// the defaults were true and appeared only when a user tried to turn something
// OFF.
//
// TWO BOOLEAN RULES, DELIBERATELY DIFFERENT. _resolve_bool_override tests
// `isinstance(value, bool)`, a STRICT bool check, so the STRING "false" is not a
// bool and the DEFAULT wins. The channel-activation rule
// (contracts.py:88-89, ported as pyTruthy in cmd/haosbot/channels.go) uses
// Python truthiness instead, so the same string "false" ENABLES the channel.
// Both rules are exercised in the reference and must never be unified.

// boolCamelAliases is _BOOL_CAMEL_ALIASES (manager.py:67-71). The reference
// consults these when the snake_case key is absent from a raw dict section,
// which is how a config written with the camelCase spelling — the spelling
// `plugin.default_config()` produces, `model_dump(by_alias=True)` — works
// alongside the pydantic field names.
var boolCamelAliases = map[string]string{
	"send_progress":   "sendProgress",
	"send_tool_hints": "sendToolHints",
	"show_reasoning":  "showReasoning",
}

// ResolveBoolOverride is _resolve_bool_override (manager.py:355-368): return
// section[key] when it is a bool, otherwise def.
//
// A value that is not a real bool — the string "false", the number 0, an
// explicit JSON null — is treated as ABSENT and def is returned. This is the
// strict-bool rule, not Python truthiness; see the package comment above.
//
// The reference has a second branch for a non-dict section
// (`getattr(section, key, None)`, manager.py:367-368). It is unreachable from
// this port: the section is always the decoded JSON object, because
// ChannelsConfig declares extra="allow" (internal/config/schema.go:245-258) and
// a channel section is preserved verbatim. The camelCase alias is consulted only
// on the dict path, exactly as upstream does it.
func ResolveBoolOverride(section map[string]any, key string, def bool) bool {
	value := section[key]
	if value == nil {
		// Python's `section.get(key)` returns None both for an absent key and
		// for an explicit null, and `if value is None` sends BOTH to the alias.
		if camel, hasAlias := boolCamelAliases[key]; hasAlias {
			value = section[camel]
		}
	}
	if v, isBool := value.(bool); isBool {
		return v
	}
	return def
}

// GlobalDeliveryPolicy is the `channels.send_progress` / `send_tool_hints` /
// `show_reasoning` policy (schema.py:33-35, all true). It is the default every
// channel inherits when it neither overrides the value in its own section nor
// declares a transport default.
type GlobalDeliveryPolicy struct {
	SendProgress  bool
	SendToolHints bool
	ShowReasoning bool
}

// ApplyDeliveryPolicy resolves and writes the three per-channel delivery
// booleans onto a freshly built channel. Port of the tail of _build_channel
// (manager.py:219-231).
//
// The order is the reference's:
//
//	progress_default, tool_hints_default = channel.progress_transport_defaults() or (
//	    self.config.channels.send_progress,
//	    self.config.channels.send_tool_hints,
//	)
//	channel.send_progress   = self._resolve_bool_override(section, "send_progress",   progress_default)
//	channel.send_tool_hints = self._resolve_bool_override(section, "send_tool_hints", tool_hints_default)
//	channel.show_reasoning  = self._resolve_bool_override(section, "show_reasoning",  self.config.channels.show_reasoning)
//
// The transport hook is asked AFTER construction, because it may read the
// instance's own parsed config (weixin/runtime.py:342 does). `or` there tests
// TRUTHINESS: a 2-tuple is truthy however false its elements are, so
// (False, False) is honoured and only None falls through to the globals. This
// port spells None as ok == false (base.go:391-397), which is what the
// ProgressTransportDefaults interface returns.
//
// show_reasoning has no transport default: its default is always the global
// policy.
//
// A channel that does not expose the write half keeps its own values. The read
// path deliberately admits such a channel — deliveryPolicyReader (manager.go
// :786-793) is read-only precisely so "a channel whose values are computed
// rather than stored (say, from its own config)" is not forced into the
// true, true, true default — and this function cannot assign through an
// interface the channel does not implement. Every runtime this port registers
// embeds *Base and therefore does implement DeliveryPolicy;
// TestRegisteredRuntimesImplementDeliveryPolicy is the guard on that assumption.
func ApplyDeliveryPolicy(ch Channel, section map[string]any, global GlobalDeliveryPolicy) {
	progressDefault := global.SendProgress
	toolHintsDefault := global.SendToolHints

	if ptd, ok := ch.(ProgressTransportDefaults); ok {
		if progress, hints, present := ptd.ProgressTransportDefaults(); present {
			progressDefault, toolHintsDefault = progress, hints
		}
	}

	policy, ok := ch.(DeliveryPolicy)
	if !ok {
		return
	}
	policy.SetSendProgress(ResolveBoolOverride(section, "send_progress", progressDefault))
	policy.SetSendToolHints(ResolveBoolOverride(section, "send_tool_hints", toolHintsDefault))
	policy.SetShowReasoning(ResolveBoolOverride(section, "show_reasoning", global.ShowReasoning))
}
