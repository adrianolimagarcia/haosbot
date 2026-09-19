// Delivery-policy differential tests.
//
// These are the Go half of compat/python/dump_delivery_policy.py: the dumper
// executes the real Python implementation of the tail of ChannelManager._build_channel
// (nanobot/channels/manager.py:206-232) and of ChannelManager._resolve_bool_override
// (manager.py:355-368), and this file drives the Go port through the same inputs
// and compares the resolved triples. Nothing here asserts behaviour read off the
// source by hand — every expected value comes from running the reference.
//
// The reference venv lives at .tools/venv. When it is absent the tests SKIP
// rather than fail, so a checkout without it still builds and tests cleanly —
// but they never silently pass: the skip is reported, and every test also
// asserts that a non-zero number of cases was actually compared.
package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// deliveryPolicyReference is the decoded dumper output.
type deliveryPolicyReference struct {
	UpstreamCommit     string              `json:"upstream_commit"`
	Channel            string              `json:"channel"`
	BaseHookReturnsNil bool                `json:"base_hook_returns_none"`
	HookOwners         map[string][]string `json:"hook_owners"`
	CamelAliases       map[string]string   `json:"camel_aliases"`
	Cases              []struct {
		Label    string         `json:"label"`
		Section  map[string]any `json:"section"`
		Global   []bool         `json:"global"`
		Hook     []bool         `json:"hook"`
		Expected []bool         `json:"expected"`
	} `json:"cases"`
}

func loadDeliveryPolicyReference(t *testing.T) *deliveryPolicyReference {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller path")
	}
	root := filepath.Dir(filepath.Dir(file))
	python := filepath.Join(root, ".tools", "venv", "bin", "python")
	script := filepath.Join(root, "compat", "python", "dump_delivery_policy.py")

	if _, err := os.Stat(python); err != nil {
		t.Skipf("SKIP: reference venv not present at %s — differential check not run", python)
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("SKIP: dumper missing at %s", script)
	}

	out, err := runReferenceCommand("delivery-policy dumper", []string{python, script}, root, nil)
	if err != nil {
		t.Fatalf("delivery-policy dumper failed: %v", err)
	}

	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	var ref deliveryPolicyReference
	if err := dec.Decode(&ref); err != nil {
		t.Fatalf("parse delivery-policy reference output: %v", err)
	}
	if ref.UpstreamCommit != "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9" {
		t.Fatalf("dumper reported unexpected upstream commit %q", ref.UpstreamCommit)
	}
	return &ref
}

// deliveryPolicyProbe is a channel built on the real *channels.Base, so the
// three flags live in the production storage and start at the reference's class
// defaults (base.py:31-33, all true). It overrides
// progress_transport_defaults() so the hook path can be driven; `hasHook` false
// is the port's spelling of Python's None (internal/channels/base.go:391-397).
type deliveryPolicyProbe struct {
	*channels.Base
	hookProgress, hookToolHints bool
	hasHook                     bool
}

func newDeliveryPolicyProbe() *deliveryPolicyProbe {
	p := &deliveryPolicyProbe{}
	p.Base = channels.NewBase(p, channels.NewMapSection(nil), nil)
	return p
}

func (c *deliveryPolicyProbe) Start(context.Context) error { return nil }
func (c *deliveryPolicyProbe) Stop(context.Context) error  { return nil }
func (c *deliveryPolicyProbe) Send(context.Context, core.OutboundMessage) error {
	return nil
}

func (c *deliveryPolicyProbe) ProgressTransportDefaults() (bool, bool, bool) {
	return c.hookProgress, c.hookToolHints, c.hasHook
}

// TestDeliveryPolicyDifferential drives ResolveBoolOverride and
// ApplyDeliveryPolicy over every case the dumper executed and requires the same
// answer the reference produced.
//
// The section the Go side is handed is the dict the REFERENCE holds after
// Config.model_validate, so the comparison is over identical inputs rather than
// over two independent decodings of the same file.
func TestDeliveryPolicyDifferential(t *testing.T) {
	ref := loadDeliveryPolicyReference(t)

	if len(ref.Cases) == 0 {
		t.Fatal("the dumper returned no cases: the differential check compared nothing")
	}

	compared := 0
	for _, tc := range ref.Cases {
		t.Run(tc.Label, func(t *testing.T) {
			if len(tc.Global) != 3 || len(tc.Expected) != 3 {
				t.Fatalf("malformed case from the dumper: global=%v expected=%v", tc.Global, tc.Expected)
			}
			global := channels.GlobalDeliveryPolicy{
				SendProgress:  tc.Global[0],
				SendToolHints: tc.Global[1],
				ShowReasoning: tc.Global[2],
			}
			want := [3]bool{tc.Expected[0], tc.Expected[1], tc.Expected[2]}

			// (1) The resolver on its own, key by key.
			//
			// The defaults fed to it are the reference's own
			// `hook or (global, global)` result, which the dumper recomputes
			// and reports; a channel with no hook therefore falls through to
			// the globals here exactly as it does upstream.
			progressDefault := global.SendProgress
			toolHintsDefault := global.SendToolHints
			if tc.Hook != nil {
				progressDefault, toolHintsDefault = tc.Hook[0], tc.Hook[1]
			}
			direct := [3]bool{
				channels.ResolveBoolOverride(tc.Section, "send_progress", progressDefault),
				channels.ResolveBoolOverride(tc.Section, "send_tool_hints", toolHintsDefault),
				channels.ResolveBoolOverride(tc.Section, "show_reasoning", global.ShowReasoning),
			}
			if direct != want {
				t.Errorf("ResolveBoolOverride resolved %v, reference says %v\n  section: %s\n  global:  %v\n  hook:    %v",
					direct, want, jsonish(tc.Section), tc.Global, tc.Hook)
			}

			// (2) The whole resolution, including the progress_transport_defaults
			// hook and the `or` semantics, through the function the gateway calls.
			probe := newDeliveryPolicyProbe()
			if tc.Hook != nil {
				probe.hookProgress, probe.hookToolHints, probe.hasHook = tc.Hook[0], tc.Hook[1], true
			}
			channels.ApplyDeliveryPolicy(probe, tc.Section, global)
			applied := [3]bool{probe.SendProgress(), probe.SendToolHints(), probe.ShowReasoning()}
			if applied != want {
				t.Errorf("ApplyDeliveryPolicy resolved %v, reference says %v\n  section: %s\n  global:  %v\n  hook:    %v",
					applied, want, jsonish(tc.Section), tc.Global, tc.Hook)
			}

			compared++
		})
	}

	if compared == 0 {
		t.Fatal("no case was compared")
	}
}

// TestDeliveryPolicyCamelAliasesMatchTheReference pins the alias table to the
// reference's own _BOOL_CAMEL_ALIASES (manager.py:67-71), read out of the
// running module rather than off the source.
func TestDeliveryPolicyCamelAliasesMatchTheReference(t *testing.T) {
	ref := loadDeliveryPolicyReference(t)

	if len(ref.CamelAliases) == 0 {
		t.Fatal("the dumper reported no camelCase aliases: nothing was compared")
	}
	for snake, camel := range ref.CamelAliases {
		// A section that carries ONLY the alias must resolve to the alias value,
		// which proves the port knows this alias exists.
		section := map[string]any{camel: true}
		if got := channels.ResolveBoolOverride(section, snake, false); !got {
			t.Errorf("ResolveBoolOverride(section{%q: true}, %q, false) = false, want true: "+
				"the reference's _BOOL_CAMEL_ALIASES maps %q -> %q", camel, snake, snake, camel)
		}
		// ...and the snake_case key must still win when both are present.
		both := map[string]any{snake: false, camel: true}
		if got := channels.ResolveBoolOverride(both, snake, true); got {
			t.Errorf("ResolveBoolOverride(section{%q: false, %q: true}, %q, true) = true, want false: "+
				"section.get(key) is tried before the alias (manager.py:363-366)", snake, camel, snake)
		}
	}
}

// TestTelegramKeepsTheGlobalProgressPolicy pins the assumption the port's
// Telegram runtime relies on: the reference's telegram runtime does NOT override
// progress_transport_defaults, so `hook or (global, global)` always yields the
// globals for Telegram.
//
// The owner list is produced by the dumper with ast over the frozen source, not
// by grep and not by hand.
func TestTelegramKeepsTheGlobalProgressPolicy(t *testing.T) {
	ref := loadDeliveryPolicyReference(t)

	if !ref.BaseHookReturnsNil {
		t.Fatal("BaseChannel.progress_transport_defaults() did not return None: " +
			"the port's ok==false spelling of None is no longer faithful")
	}
	if len(ref.HookOwners) == 0 {
		t.Fatal("the dumper reported no owners of progress_transport_defaults: the check compared nothing")
	}
	for class, paths := range ref.HookOwners {
		for _, path := range paths {
			if strings.Contains(path, "/telegram/") {
				t.Errorf("the reference's %s (%s) overrides progress_transport_defaults: "+
					"the port must port that override rather than inherit the global policy", class, path)
			}
		}
	}
}

// jsonish renders a decoded section compactly for failure messages.
func jsonish(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "<unencodable>"
	}
	return string(b)
}
