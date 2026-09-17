package main

// Regression tests for the delivery-policy resolution the gateway performs when
// it builds a channel.
//
// THE DEFECT these tests pin down. ChannelManager._build_channel resolves three
// booleans on EVERY channel it constructs (nanobot/channels/manager.py:219-231):
//
//	progress_default, tool_hints_default = channel.progress_transport_defaults() or (
//	    self.config.channels.send_progress,
//	    self.config.channels.send_tool_hints,
//	)
//	channel.send_progress   = self._resolve_bool_override(section, "send_progress",   progress_default)
//	channel.send_tool_hints = self._resolve_bool_override(section, "send_tool_hints", tool_hints_default)
//	channel.show_reasoning  = self._resolve_bool_override(section, "show_reasoning",  self.config.channels.show_reasoning)
//
// The port resolved none of them. internal/channels/base.go defines
// SetSendProgress/SetSendToolHints/SetShowReasoning and WithDeliveryPolicy, and
// internal/channels/manager.go READS the values (deliveryPolicy, manager.go:805),
// but nothing in the production path ever wrote them, so every channel kept
// NewBase's hardcoded `true, true, true` (base.go:303-305).
//
// The failure is invisible while the global defaults are true: a user who turns
// nothing off sees the correct behaviour by accident. It is visible the moment
// anything is turned OFF, which is what the cases below exercise.
//
// THE STRICT-BOOL RULE. _resolve_bool_override (manager.py:355-368) is
//
//	value = section.get(key)                       # or the camelCase alias
//	return value if isinstance(value, bool) else default
//
// `isinstance(value, bool)` is a STRICT bool check, so the STRING "false" is not
// a bool and the DEFAULT wins — the opposite of the channel-activation rule,
// which uses Python truthiness and therefore treats the string "false" as
// ENABLED (contracts.py:88-89, ported as pyTruthy in cmd/haosbot/channels.go).
// Both rules are exercised here so neither can be "fixed" into the other.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/telegram"
	"github.com/adrianolimagarcia/nanobot-go/internal/events"
)

// resolvedDeliveryPolicy builds a manager from body through the production
// wiring path and returns the policy the manager will read from the telegram
// channel it registered.
func resolvedDeliveryPolicy(t *testing.T, body string) (progress, toolHints, reasoning bool) {
	t.Helper()

	cfg := loadConfigFile(t, body)
	manager := buildChannelManager(cfg, newTestBus(t))

	ch, ok := manager.GetChannel(telegram.ChannelName)
	if !ok {
		t.Fatalf("no %s channel registered for config %s", telegram.ChannelName, body)
	}
	policy, ok := ch.(channels.DeliveryPolicy)
	if !ok {
		t.Fatalf("registered channel %T does not implement channels.DeliveryPolicy", ch)
	}
	return policy.SendProgress(), policy.SendToolHints(), policy.ShowReasoning()
}

// TestChannelDeliveryPolicyResolution is the resolution table. Every case is a
// documented behaviour of _build_channel + _resolve_bool_override, and the
// `want` values are what the reference produces for the same configuration.
//
// The global policy is written as the camelCase key the port's own JSON tags use
// (internal/config/schema.go:248-250); the port's decoder accepts both spellings
// (internal/config/decode.go:48-68) exactly as pydantic does.
func TestChannelDeliveryPolicyResolution(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		want      [3]bool // progress, tool hints, reasoning
		reference string  // why the reference answers this way
	}{
		{
			name:      "absent_keys_use_the_global_defaults",
			body:      `{"channels": {"telegram": {"enabled": true, "token": "T"}}}`,
			want:      [3]bool{true, true, true},
			reference: "schema.py:33-35 defaults are all true; no override key is present",
		},
		{
			name: "global_false_is_the_default_the_channel_inherits",
			body: `{"channels": {"sendProgress": false, "sendToolHints": false,
			         "showReasoning": false, "telegram": {"enabled": true, "token": "T"}}}`,
			want:      [3]bool{false, false, false},
			reference: "manager.py:219-231: the section has no override, so the global policy is used",
		},
		{
			name:      "global_policy_is_per_key_not_all_or_nothing",
			body:      `{"channels": {"sendProgress": false, "telegram": {"enabled": true, "token": "T"}}}`,
			want:      [3]bool{false, true, true},
			reference: "each of the three keys resolves independently",
		},
		{
			name: "per_channel_false_overrides_the_true_global",
			body: `{"channels": {"telegram": {"enabled": true, "token": "T",
			         "sendProgress": false, "sendToolHints": false, "showReasoning": false}}}`,
			want:      [3]bool{false, false, false},
			reference: "a real bool in the section wins over the default (manager.py:367)",
		},
		{
			name: "per_channel_true_overrides_the_false_global",
			body: `{"channels": {"sendProgress": false, "sendToolHints": false,
			         "showReasoning": false,
			         "telegram": {"enabled": true, "token": "T",
			                      "sendProgress": true, "sendToolHints": true,
			                      "showReasoning": true}}}`,
			want:      [3]bool{true, true, true},
			reference: "a real bool in the section wins over the default (manager.py:367)",
		},
		{
			name: "snake_case_section_key_is_read_directly",
			body: `{"channels": {"sendProgress": false, "sendToolHints": false,
			         "showReasoning": false,
			         "telegram": {"enabled": true, "token": "T",
			                      "send_progress": true, "send_tool_hints": true,
			                      "show_reasoning": true}}}`,
			want:      [3]bool{true, true, true},
			reference: "section.get(key) is tried before the alias (manager.py:363)",
		},
		{
			name: "camel_case_alias_is_the_fallback_key",
			body: `{"channels": {"sendProgress": false, "sendToolHints": false,
			         "showReasoning": false,
			         "telegram": {"enabled": true, "token": "T",
			                      "sendProgress": true, "sendToolHints": true,
			                      "showReasoning": true}}}`,
			want:      [3]bool{true, true, true},
			reference: "_BOOL_CAMEL_ALIASES (manager.py:67-71) is consulted when the snake_case key is absent",
		},
		{
			name: "string_false_is_not_a_bool_so_the_true_default_wins",
			body: `{"channels": {"telegram": {"enabled": true, "token": "T",
			         "sendProgress": "false", "sendToolHints": "false",
			         "showReasoning": "false"}}}`,
			want:      [3]bool{true, true, true},
			reference: `isinstance("false", bool) is False, so the default is returned`,
		},
		{
			name: "string_true_is_not_a_bool_so_the_false_default_wins",
			body: `{"channels": {"sendProgress": false, "sendToolHints": false,
			         "showReasoning": false,
			         "telegram": {"enabled": true, "token": "T",
			                      "sendProgress": "true", "sendToolHints": "true",
			                      "showReasoning": "true"}}}`,
			want:      [3]bool{false, false, false},
			reference: `isinstance("true", bool) is False, so the default is returned`,
		},
		{
			name: "non_bool_numbers_fall_back_to_the_default",
			body: `{"channels": {"telegram": {"enabled": true, "token": "T",
			         "sendProgress": 0, "sendToolHints": 0, "showReasoning": 0}}}`,
			want:      [3]bool{true, true, true},
			reference: "isinstance(0, bool) is False in Python (bool is a subclass of int, not the reverse)",
		},
		{
			name: "json_null_falls_back_to_the_default",
			body: `{"channels": {"sendProgress": false, "sendToolHints": false,
			         "showReasoning": false,
			         "telegram": {"enabled": true, "token": "T",
			                      "sendProgress": null, "sendToolHints": null,
			                      "showReasoning": null}}}`,
			want:      [3]bool{false, false, false},
			reference: "section.get(key) returns None, the alias is absent too, so the default is returned",
		},
		{
			name: "null_snake_case_key_falls_through_to_the_camel_alias",
			body: `{"channels": {"sendProgress": false, "sendToolHints": false,
			         "showReasoning": false,
			         "telegram": {"enabled": true, "token": "T",
			                      "send_progress": null, "sendProgress": true,
			                      "send_tool_hints": null, "sendToolHints": true,
			                      "show_reasoning": null, "showReasoning": true}}}`,
			want:      [3]bool{true, true, true},
			reference: "`if value is None` sends an explicit null to the alias lookup (manager.py:364-366)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			progress, hints, reasoning := resolvedDeliveryPolicy(t, tc.body)
			got := [3]bool{progress, hints, reasoning}
			if got != tc.want {
				t.Errorf("resolved policy (progress, toolHints, reasoning) = %v, want %v\n  config: %s\n  reference: %s",
					got, tc.want, tc.body, tc.reference)
			}
		})
	}
}

// TestChannelDeliveryPolicyIsConsumedOnTheOutboundPath proves the resolved value
// is not merely stored: it is the value the outbound dispatcher reads when it
// decides whether a progress message reaches the channel.
//
// internal/channels/manager_test.go:1296 already covers the READ half by calling
// the setters directly. This test never touches a setter: it configures a file,
// builds the manager through buildChannelManager, and watches the real telegram
// transport for a sendMessage call.
func TestChannelDeliveryPolicyIsConsumedOnTheOutboundPath(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantDelivered bool
	}{
		{
			name:          "default_delivers_progress",
			body:          `{"channels": {"telegram": {"enabled": true, "token": "T"}}}`,
			wantDelivered: true,
		},
		{
			name: "global_send_progress_false_suppresses_it",
			body: `{"channels": {"sendProgress": false,
			         "telegram": {"enabled": true, "token": "T"}}}`,
			wantDelivered: false,
		},
		{
			name: "per_channel_send_progress_false_suppresses_it",
			body: `{"channels": {"telegram": {"enabled": true, "token": "T",
			         "sendProgress": false}}}`,
			wantDelivered: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadConfigFile(t, tc.body)
			messageBus := newTestBus(t)
			manager := buildChannelManager(cfg, messageBus)

			registered, ok := manager.GetChannel(telegram.ChannelName)
			if !ok {
				t.Fatalf("no telegram channel registered for config %s", tc.body)
			}
			tg, ok := registered.(*telegram.Channel)
			if !ok {
				t.Fatalf("registered channel has type %T, want *telegram.Channel", registered)
			}
			api := &fakeBotAPI{}
			tg.SetClient(telegram.NewBotClient("T", "", telegram.WithHTTPClient(api)))

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan error, 1)
			go func() { started <- manager.StartAll(ctx) }()
			waitFor(t, 5*time.Second, "the channel to reach getUpdates", func() bool {
				return api.called("getUpdates")
			})

			if err := messageBus.PublishOutbound(ctx, events.OutboundMessageForEvent(
				telegram.ChannelName, "12345",
				events.ProgressEvent{Content: "working"},
				nil, nil,
			)); err != nil {
				t.Fatalf("publish outbound: %v", err)
			}

			waitFor(t, 5*time.Second, "the outbound queue to drain", func() bool {
				return messageBus.OutboundSize() == 0
			})
			// The dispatcher drains the queue before it hands the message to the
			// channel, so a short grace period is needed before the ABSENCE of a
			// sendMessage call means anything.
			deadline := time.Now().Add(500 * time.Millisecond)
			for time.Now().Before(deadline) && !api.called("sendMessage") {
				time.Sleep(10 * time.Millisecond)
			}

			if got := api.called("sendMessage"); got != tc.wantDelivered {
				t.Errorf("sendMessage called = %v, want %v (progress reached the transport: %v)\n  config: %s",
					got, tc.wantDelivered, got, tc.body)
			}

			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopCancel()
			if err := manager.StopAll(stopCtx); err != nil {
				t.Fatalf("StopAll() = %v, want nil", err)
			}
			select {
			case err := <-started:
				if err != nil {
					t.Fatalf("StartAll() = %v, want nil", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("StartAll did not return after StopAll")
			}
		})
	}
}

// TestChannelActivationStillUsesPythonTruthiness guards the rule that must NOT be
// conflated with the strict-bool override rule above.
//
// contracts.py:88-89 is `bool(raw_enabled)`, so the STRING "false" ENABLES the
// channel; _resolve_bool_override would treat the same string as "absent". A fix
// that reused one rule for both would break one of them, and this test is what
// catches that.
func TestChannelActivationStillUsesPythonTruthiness(t *testing.T) {
	for _, value := range []string{`"false"`, `"0"`, `"no"`, `1`, `"true"`} {
		t.Run(value, func(t *testing.T) {
			body := fmt.Sprintf(
				`{"channels": {"telegram": {"enabled": %s, "token": "T", "sendProgress": false}}}`,
				value,
			)
			cfg := loadConfigFile(t, body)
			manager := buildChannelManager(cfg, newTestBus(t))

			ch, ok := manager.GetChannel(telegram.ChannelName)
			if !ok {
				t.Fatalf("enabled=%s did not activate the channel, but bool(%s) is truthy", value, value)
			}
			// ...and the strict-bool rule still applies to the OTHER key in the
			// same section, proving the two rules coexist.
			policy, ok := ch.(channels.DeliveryPolicy)
			if !ok {
				t.Fatalf("channel %T does not implement channels.DeliveryPolicy", ch)
			}
			if policy.SendProgress() {
				t.Errorf("enabled=%s: sendProgress = true, want false", value)
			}
		})
	}
}

// TestChannelActivationStringFalseIsStillEnabled pins the exact reading of the
// activation rule for the string the strict-bool rule would reject, so a future
// "unification" of the two rules fails loudly.
func TestChannelActivationStringFalseIsStillEnabled(t *testing.T) {
	cfg := loadConfigFile(t, `{"channels": {"telegram": {"enabled": "false", "token": "T"}}}`)
	manager := buildChannelManager(cfg, newTestBus(t))

	if names := manager.EnabledChannels(); len(names) != 1 || names[0] != telegram.ChannelName {
		t.Fatalf("EnabledChannels() = %v, want [%s]: bool(\"false\") is True in Python (contracts.py:88-89)",
			names, telegram.ChannelName)
	}
}

// TestChannelWithoutDeliveryPolicyKeepsClassDefaults documents what happens when
// a registered runtime does not expose the write half of the policy. Python
// assigns plain attributes and cannot fail; Go can only assign through the
// interface, so a runtime that does not implement it keeps BaseChannel's class
// defaults. Every runtime this port registers embeds *channels.Base and
// therefore implements it — this test is the guard on that assumption.
func TestRegisteredRuntimesImplementDeliveryPolicy(t *testing.T) {
	for name, build := range channelBuilders {
		section := channels.NewMapSection(map[string]any{"enabled": true, "token": "T"})
		ch, err := build(section, nil)
		if err != nil {
			t.Fatalf("channelBuilders[%q] returned %v, want a channel", name, err)
		}
		if _, ok := ch.(channels.DeliveryPolicy); !ok {
			t.Errorf("channelBuilders[%q] built %T, which does not implement channels.DeliveryPolicy: "+
				"the manager's per-channel overrides would be silently dropped", name, ch)
		}
		if _, ok := ch.(channels.ProgressTransportDefaults); !ok {
			t.Errorf("channelBuilders[%q] built %T, which does not implement channels.ProgressTransportDefaults: "+
				"progress_transport_defaults() would have no port equivalent", name, ch)
		}
	}
}

// TestProgressTransportDefaultsNoneKeepsTheGlobalPolicy is the (c) layer of the
// resolution: `channel.progress_transport_defaults() or (global, global)`.
//
// Python's `or` tests TRUTHINESS, and a 2-tuple is truthy however false its
// elements are, so only None falls through to the globals. The port spells None
// as ok == false (internal/channels/base.go:391-397).
//
// OBSERVED at this commit: the reference's telegram runtime does not override
// progress_transport_defaults (grep over nanobot/channels/telegram returns
// nothing), so Telegram always keeps the global policy; email/runtime.py:154 and
// weixin/runtime.py:342 are the only overrides, and neither is registered here.
// The hook is nevertheless ported and consulted, so a future runtime that needs
// it does not have to re-plumb the manager.
func TestProgressTransportDefaultsNoneKeepsTheGlobalPolicy(t *testing.T) {
	ch, ok := telegramChannelFor(t, `{"channels": {"sendProgress": false, "sendToolHints": false,
	                                  "telegram": {"enabled": true, "token": "T"}}}`)
	if !ok {
		t.Fatal("telegram channel was not registered")
	}
	ptd, ok := ch.(channels.ProgressTransportDefaults)
	if !ok {
		t.Fatalf("channel %T does not implement channels.ProgressTransportDefaults", ch)
	}
	if progress, hints, present := ptd.ProgressTransportDefaults(); present {
		t.Fatalf("telegram ProgressTransportDefaults() = (%v, %v, %v), want present=false: "+
			"the reference's telegram runtime does not override the hook, so it must return None",
			progress, hints, present)
	}
	policy := ch.(channels.DeliveryPolicy)
	if policy.SendProgress() || policy.SendToolHints() {
		t.Errorf("ProgressTransportDefaults() returned None but the global false policy was not applied: "+
			"sendProgress=%v sendToolHints=%v", policy.SendProgress(), policy.SendToolHints())
	}
}

// telegramChannelFor builds the manager and returns the registered telegram
// channel.
func telegramChannelFor(t *testing.T, body string) (channels.Channel, bool) {
	t.Helper()
	manager := buildChannelManager(loadConfigFile(t, body), newTestBus(t))
	return manager.GetChannel(telegram.ChannelName)
}
