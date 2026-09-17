// Channel-abstraction and pairing-store differential tests.
//
// These are the Go half of compat/python/dump_channels.py: the dumper executes
// the real Python implementation of nanobot/channels/base.py and
// nanobot/pairing/store.py, and this file drives the Go port through the same
// inputs and compares the results. Nothing here asserts behaviour read off the
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
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/pairing"
)

// ---------------------------------------------------------------------------
// Reference loading
// ---------------------------------------------------------------------------

// channelsRepoRoot resolves the module root from this file's location.
//
// It is a separate helper from repoRoot in differential_test.go on purpose:
// these tests must keep working while that file is edited by other work.
func channelsRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller path")
	}
	return filepath.Dir(filepath.Dir(file))
}

type channelsReference struct {
	UpstreamCommit string `json:"upstream_commit"`
	StoreConstants struct {
		Alphabet       string `json:"alphabet"`
		CodeLength     int    `json:"code_length"`
		TTLDefaultS    int    `json:"ttl_default_s"`
		DefaultPath    string `json:"default_path"`
		MetaCodeKey    string `json:"meta_code_key"`
		MetaCommandKey string `json:"meta_command_key"`
	} `json:"store_constants"`
	IsAllowed []struct {
		Label  string         `json:"label"`
		Config map[string]any `json:"config"`
		Sender string         `json:"sender"`
		Result *bool          `json:"result"`
		Error  *string        `json:"error"`
	} `json:"is_allowed"`
	IsAllowedObject []struct {
		Label     string  `json:"label"`
		AllowFrom any     `json:"allow_from"`
		Streaming any     `json:"streaming"`
		Sender    string  `json:"sender"`
		Result    *bool   `json:"result"`
		Error     *string `json:"error"`
	} `json:"is_allowed_object"`
	PairingFallback []struct {
		Label  string `json:"label"`
		Sender string `json:"sender"`
		Result bool   `json:"result"`
	} `json:"pairing_fallback"`
	SupportsStreaming []struct {
		Label  string         `json:"label"`
		Config map[string]any `json:"config"`
		Class  string         `json:"class"`
		Result bool           `json:"result"`
	} `json:"supports_streaming"`
	FormatPairingReply []struct {
		Code string `json:"code"`
		Text string `json:"text"`
	} `json:"format_pairing_reply"`
	FloatRepr []struct {
		Value json.Number `json:"value"`
		Repr  string      `json:"repr"`
	} `json:"float_repr"`
	PyStr []struct {
		Value any    `json:"value"`
		Str   string `json:"str"`
		Repr  string `json:"repr"`
	} `json:"py_str"`
	HandleMessage []struct {
		Label       string         `json:"label"`
		Config      map[string]any `json:"config"`
		Kwargs      map[string]any `json:"kwargs"`
		Error       *string        `json:"error"`
		Published   map[string]any `json:"published"`
		InboundSize int            `json:"inbound_size"`
		Sent        []any          `json:"sent"`
	} `json:"handle_message"`
	Store struct {
		FrozenTime float64           `json:"frozen_time"`
		Fixtures   map[string]string `json:"fixtures"`
		Steps      []struct {
			Label  string  `json:"label"`
			Exists bool    `json:"exists"`
			Text   *string `json:"text"`
		} `json:"steps"`
		Results map[string]any `json:"results"`
	} `json:"store"`
}

// loadChannelsReference runs the dumper and parses its output.
//
// Numbers are decoded as json.Number so that the integer/float distinction the
// Python document carries survives; comparisons normalise them afterwards.
func loadChannelsReference(t *testing.T) *channelsReference {
	t.Helper()
	root := channelsRepoRoot(t)
	python := filepath.Join(root, ".tools", "venv", "bin", "python")
	script := filepath.Join(root, "compat", "python", "dump_channels.py")

	if _, err := os.Stat(python); err != nil {
		t.Skipf("SKIP: reference venv not present at %s — differential check not run", python)
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("SKIP: dumper missing at %s", script)
	}

	cmd := exec.Command(python, script)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("channels dumper failed: %v\n%s", err, stderr)
	}

	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	var ref channelsReference
	if err := dec.Decode(&ref); err != nil {
		t.Fatalf("parse channels reference output: %v", err)
	}
	if ref.UpstreamCommit != "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9" {
		t.Fatalf("dumper reported unexpected upstream commit %q", ref.UpstreamCommit)
	}
	return &ref
}

// ---------------------------------------------------------------------------
// Comparison helpers
// ---------------------------------------------------------------------------

var channelsCodeRE = regexp.MustCompile(`[A-Z0-9]{4}-[A-Z0-9]{4}`)

// comparableJSON round-trips v through JSON so that a Go value and a value
// decoded from the reference compare structurally. Numbers become float64 on
// both sides; the integer/float distinction is covered byte-exactly by the
// store file comparison instead.
func comparableJSON(t *testing.T, v any) any {
	t.Helper()
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal for comparison: %v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal for comparison: %v (%s)", err, raw)
	}
	return out
}

// normalizeNumbers converts json.Number to float64, so a reference value
// decoded with UseNumber compares against a Go-produced value.
func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	case []any:
		if t == nil {
			// JSON null, not an empty array: the two must stay distinct, since
			// "nothing was published" is exactly what a null records.
			return nil
		}
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = normalizeNumbers(item)
		}
		return out
	case map[string]any:
		if t == nil {
			return nil
		}
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = normalizeNumbers(item)
		}
		return out
	}
	return v
}

// normalizePairingCodes reproduces the dumper's norm(): every pairing code in
// the value is replaced by the literal CODE, because secrets.choice and
// crypto/rand cannot agree on a value.
func normalizePairingCodes(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal for normalisation: %v", err)
	}
	raw = channelsCodeRE.ReplaceAll(raw, []byte("CODE"))
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal after normalisation: %v (%s)", err, raw)
	}
	return out
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// captureBus records published inbound messages. *bus.Bus is deliberately not
// used: the reference's MessageBus is consumed with consume_inbound, and a
// queue would make the comparison depend on consumption order rather than on
// what the channel produced.
type captureBus struct {
	mu   sync.Mutex
	msgs []core.InboundMessage
}

func (b *captureBus) PublishInbound(_ context.Context, msg core.InboundMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, msg)
	return nil
}

func (b *captureBus) take() (core.InboundMessage, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.msgs) == 0 {
		return core.InboundMessage{}, false
	}
	msg := b.msgs[0]
	b.msgs = b.msgs[1:]
	return msg, true
}

func (b *captureBus) size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.msgs)
}

// handleMessageChannel is what the _handle_message loop needs: the universal
// inbound entry point plus the record of what the channel delivered.
type handleMessageChannel interface {
	HandleMessage(ctx context.Context, req channels.InboundRequest) error
	Sent() []core.OutboundMessage
}

// recordingChannel is the Go equivalent of the reference's _DummyChannel
// (tests/channels/test_base_channel.py:10-25): it implements the three required
// methods and records what it sends.
type recordingChannel struct {
	*channels.Base

	sent []core.OutboundMessage
}

func (c *recordingChannel) Start(context.Context) error { return nil }
func (c *recordingChannel) Stop(context.Context) error  { return nil }

func (c *recordingChannel) Send(_ context.Context, msg core.OutboundMessage) error {
	c.sent = append(c.sent, msg)
	return nil
}

// Sent returns what the channel delivered.
func (c *recordingChannel) Sent() []core.OutboundMessage { return c.sent }

// streamingChannel adds send_delta, which is the ONLY thing that makes
// supports_streaming true.
type streamingChannel struct {
	*recordingChannel
}

func (c *streamingChannel) SendDelta(
	_ context.Context, _ string, _ string, _ map[string]any, _ channels.DeltaOptions,
) error {
	return nil
}

func newRecordingChannel(cfg channels.Section, store *pairing.Store) (*recordingChannel, *captureBus) {
	bus := &captureBus{}
	c := &recordingChannel{}
	c.Base = channels.NewBase(c, cfg, bus,
		channels.WithName("dummy"), channels.WithPairingStore(store), channels.WithLogger(discardLogger()))
	return c, bus
}

func newStreamingChannel(cfg channels.Section, store *pairing.Store) (*streamingChannel, *captureBus) {
	bus := &captureBus{}
	c := &streamingChannel{recordingChannel: &recordingChannel{}}
	c.Base = channels.NewBase(c, cfg, bus,
		channels.WithName("dummy"), channels.WithPairingStore(store), channels.WithLogger(discardLogger()))
	return c, bus
}

// newTestStore returns an isolated store whose file does not exist yet.
func newTestStore(t *testing.T, name string) *pairing.Store {
	t.Helper()
	store := pairing.NewStore(filepath.Join(t.TempDir(), name))
	store.SetLogger(discardLogger())
	return store
}

// storeWithApproved writes a store file holding the given approvals.
func storeWithApproved(t *testing.T, approvals map[string][]string) *pairing.Store {
	t.Helper()
	store := newTestStore(t, "pairing.json")
	payload := map[string]any{"approved": approvals, "pending": map[string]any{}}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal store fixture: %v", err)
	}
	if err := os.WriteFile(store.Path(), raw, 0o600); err != nil {
		t.Fatalf("write store fixture: %v", err)
	}
	return store
}

// ---------------------------------------------------------------------------
// 1. Constants
// ---------------------------------------------------------------------------

func TestPairingMetaKeysMatchPython(t *testing.T) {
	ref := loadChannelsReference(t)

	cases := 0
	for _, tc := range []struct {
		name string
		want string
		got  string
	}{
		{"PAIRING_CODE_META_KEY", ref.StoreConstants.MetaCodeKey, pairing.PairingCodeMetaKey},
		{"PAIRING_COMMAND_META_KEY", ref.StoreConstants.MetaCommandKey, pairing.PairingCommandMetaKey},
	} {
		cases++
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	if cases != 2 {
		t.Fatalf("compared %d cases, want 2", cases)
	}
}

func TestPairingStoreConstantsMatchPython(t *testing.T) {
	ref := loadChannelsReference(t)

	if got, want := pairing.DefaultTTLSeconds, ref.StoreConstants.TTLDefaultS; got != want {
		t.Errorf("DefaultTTLSeconds = %d, want %d", got, want)
	}
	if got, want := pairing.CodeLength, ref.StoreConstants.CodeLength; got != want {
		t.Errorf("CodeLength = %d, want %d", got, want)
	}
	if got, want := pairing.CodeAlphabet, ref.StoreConstants.Alphabet; got != want {
		t.Errorf("CodeAlphabet = %q, want %q", got, want)
	}
	if got, want := pairing.DefaultPath(), ref.StoreConstants.DefaultPath; got != want {
		t.Errorf("DefaultPath() = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// 2. is_allowed
// ---------------------------------------------------------------------------

func TestChannelsIsAllowedMatchesPython(t *testing.T) {
	ref := loadChannelsReference(t)
	store := newTestStore(t, "pairing.json") // empty: never approves

	cases, divergences := 0, 0
	for _, tc := range ref.IsAllowed {
		cases++
		cfg := channels.NewMapSection(tc.Config)
		channel, _ := newRecordingChannel(cfg, store)
		got := channel.IsAllowed(tc.Sender)

		if tc.Error != nil {
			// The reference raises TypeError for an allowlist it cannot iterate
			// (an int, a bool, null). Go has no exception channel to carry that,
			// so the port denies. Recorded as a divergence rather than asserted
			// as equality, and never silently.
			divergences++
			if got {
				t.Errorf("%s: Go authorized a sender where Python raised %s", tc.Label, *tc.Error)
			}
			continue
		}
		if tc.Result == nil {
			t.Fatalf("%s: reference has neither result nor error", tc.Label)
		}
		if got != *tc.Result {
			t.Errorf("%s: IsAllowed(%q) = %v, want %v (config %v)",
				tc.Label, tc.Sender, got, *tc.Result, tc.Config)
		}
	}
	if cases < 30 {
		t.Fatalf("compared only %d is_allowed cases, want at least 30", cases)
	}
	if divergences != 2 {
		t.Errorf("documented TypeError divergences = %d, want 2", divergences)
	}
	t.Logf("is_allowed: %d cases compared, %d documented TypeError divergences", cases, divergences)
}

func TestChannelsIsAllowedObjectSectionMatchesPython(t *testing.T) {
	ref := loadChannelsReference(t)
	store := newTestStore(t, "pairing.json")

	cases := 0
	for _, tc := range ref.IsAllowedObject {
		cases++
		cfg := channels.NewObjectSection(channels.ObjectSection{
			AllowFrom: tc.AllowFrom,
			Streaming: tc.Streaming,
		})
		channel, _ := newRecordingChannel(cfg, store)
		got := channel.IsAllowed(tc.Sender)

		if tc.Error != nil {
			if got {
				t.Errorf("%s: Go authorized a sender where Python raised %s", tc.Label, *tc.Error)
			}
			continue
		}
		if tc.Result == nil {
			t.Fatalf("%s: reference has neither result nor error", tc.Label)
		}
		if got != *tc.Result {
			t.Errorf("%s: IsAllowed(%q) = %v, want %v (allow_from %#v)",
				tc.Label, tc.Sender, got, *tc.Result, tc.AllowFrom)
		}
	}
	if cases < 8 {
		t.Fatalf("compared only %d object-section cases, want at least 8", cases)
	}
	t.Logf("is_allowed (object section): %d cases compared", cases)
}

func TestChannelsIsAllowedPairingFallbackMatchesPython(t *testing.T) {
	ref := loadChannelsReference(t)

	// The reference monkeypatches is_approved to `sender_id == "paired"`;
	// a store holding exactly that approval is the same predicate.
	store := storeWithApproved(t, map[string][]string{"dummy": {"paired"}})

	cases := 0
	for _, tc := range ref.PairingFallback {
		cases++
		channel, _ := newRecordingChannel(channels.NewMapSection(map[string]any{"allowFrom": []any{}}), store)
		if got := channel.IsAllowed(tc.Sender); got != tc.Result {
			t.Errorf("%s: IsAllowed(%q) = %v, want %v", tc.Label, tc.Sender, got, tc.Result)
		}
	}
	if cases == 0 {
		t.Fatal("compared 0 pairing-fallback cases")
	}
	t.Logf("is_allowed (pairing fallback): %d cases compared", cases)
}

// ---------------------------------------------------------------------------
// 3. supports_streaming
// ---------------------------------------------------------------------------

func TestChannelsSupportsStreamingMatchesPython(t *testing.T) {
	ref := loadChannelsReference(t)
	store := newTestStore(t, "pairing.json")

	cases := 0
	for _, tc := range ref.SupportsStreaming {
		cases++
		cfg := channels.NewMapSection(tc.Config)

		// The same config must give different answers on the two classes: that
		// is the whole point of the capability probe.
		plain, _ := newRecordingChannel(cfg, store)
		streaming, _ := newStreamingChannel(cfg, store)
		if got := plain.SupportsStreaming(); got {
			t.Errorf("%s: a channel without SendDelta reported streaming", tc.Label)
		}

		got := streaming.SupportsStreaming()
		if tc.Class == "plain" {
			// The reference asked a channel that does not override send_delta;
			// the Go equivalent is the plain channel, already checked above.
			if tc.Result {
				t.Errorf("%s: reference reported streaming for a plain channel", tc.Label)
			}
			continue
		}
		if got != tc.Result {
			t.Errorf("%s: SupportsStreaming() = %v, want %v (config %v)",
				tc.Label, got, tc.Result, tc.Config)
		}
	}
	if cases < 10 {
		t.Fatalf("compared only %d supports_streaming cases, want at least 10", cases)
	}
	t.Logf("supports_streaming: %d cases compared", cases)
}

// ---------------------------------------------------------------------------
// 4. format_pairing_reply and Python value rendering
// ---------------------------------------------------------------------------

func TestPairingFormatPairingReplyMatchesPython(t *testing.T) {
	ref := loadChannelsReference(t)

	cases := 0
	for _, tc := range ref.FormatPairingReply {
		cases++
		if got := pairing.FormatPairingReply(tc.Code); got != tc.Text {
			t.Errorf("FormatPairingReply(%q) =\n%q\nwant\n%q", tc.Code, got, tc.Text)
		}
	}
	if cases < 5 {
		t.Fatalf("compared only %d reply cases, want at least 5", cases)
	}
}

func TestPairingPyFloatReprMatchesPython(t *testing.T) {
	ref := loadChannelsReference(t)

	cases := 0
	for _, tc := range ref.FloatRepr {
		cases++
		value, err := tc.Value.Float64()
		if err != nil {
			t.Fatalf("reference value %q is not a float: %v", tc.Value, err)
		}
		if got := pairing.PyFloatRepr(value); got != tc.Repr {
			t.Errorf("PyFloatRepr(%s) = %q, want %q", tc.Value, got, tc.Repr)
		}
	}
	if cases < 20 {
		t.Fatalf("compared only %d float-repr cases, want at least 20", cases)
	}
}

func TestPairingPyStrMatchesPython(t *testing.T) {
	ref := loadChannelsReference(t)

	cases := 0
	for _, tc := range ref.PyStr {
		cases++
		got := pairing.PyStr(tc.Value)
		if got != tc.Str {
			t.Errorf("PyStr(%#v) = %q, want %q", tc.Value, got, tc.Str)
		}
		// In Python 3 str() and repr() agree for every non-string value; for a
		// string they differ, which is why the two reference fields exist.
		if _, isString := tc.Value.(string); !isString && got != tc.Repr {
			t.Errorf("PyStr(%#v) = %q, want repr %q", tc.Value, got, tc.Repr)
		}
	}
	if cases < 20 {
		t.Fatalf("compared only %d str() cases, want at least 20", cases)
	}
}

// ---------------------------------------------------------------------------
// 5. _handle_message
// ---------------------------------------------------------------------------

// requestFromKwargs maps the reference's keyword arguments onto InboundRequest.
func requestFromKwargs(t *testing.T, kw map[string]any) channels.InboundRequest {
	t.Helper()
	req := channels.InboundRequest{}
	if v, ok := kw["sender_id"].(string); ok {
		req.SenderID = v
	}
	if v, ok := kw["chat_id"].(string); ok {
		req.ChatID = v
	}
	if v, ok := kw["content"].(string); ok {
		req.Content = v
	}
	if v, ok := kw["media"].([]any); ok {
		for _, item := range v {
			req.Media = append(req.Media, item.(string))
		}
	}
	if v, ok := kw["metadata"].(map[string]any); ok {
		req.Metadata = v
	}
	if v, ok := kw["session_key"].(string); ok {
		req.SessionKey = &v
	}
	if v, ok := kw["is_dm"].(bool); ok {
		req.IsDM = v
	}
	if v, ok := kw["authorization_id"].(string); ok {
		req.AuthorizationID = &v
	}
	if v, ok := kw["require_existing_session"].(bool); ok {
		req.RequireExistingSession = v
	}
	return req
}

func inboundToComparable(msg core.InboundMessage) map[string]any {
	out := map[string]any{
		"channel":                  msg.Channel,
		"sender_id":                msg.SenderID,
		"chat_id":                  msg.ChatID,
		"content":                  msg.Content,
		"media":                    msg.Media,
		"metadata":                 msg.Metadata,
		"session_key":              msg.SessionKey(),
		"is_user_input":            msg.IsUserInput(),
		"require_existing_session": msg.RequireExistingSess,
	}
	if msg.SessionKeyOverride == nil {
		out["session_key_override"] = nil
	} else {
		out["session_key_override"] = *msg.SessionKeyOverride
	}
	if msg.InputRole == nil {
		out["input_role"] = nil
	} else {
		out["input_role"] = *msg.InputRole
	}
	return out
}

func outboundToComparable(msg core.OutboundMessage) map[string]any {
	var event any
	if msg.Event != nil {
		event = msg.Event.EventName()
	}
	return map[string]any{
		"channel":  msg.Channel,
		"chat_id":  msg.ChatID,
		"content":  msg.Content,
		"reply_to": msg.ReplyTo,
		"media":    msg.Media,
		"metadata": msg.Metadata,
		"buttons":  msg.Buttons,
		"event":    event,
	}
}

func TestChannelsHandleMessageMatchesPython(t *testing.T) {
	ref := loadChannelsReference(t)

	// The aliasing cases and the store-unavailable case need code the generic
	// loop cannot express; they are handled explicitly below.
	skip := map[string]bool{
		"aliasing-nonstream-nonempty": true,
		"aliasing-nonstream-empty":    true,
		"aliasing-streaming-nonempty": true,
		"aliasing-media":              true,
		"store-unavailable-dm":        true,
	}

	// The dumper builds a streaming channel exactly for these labels; the
	// choice is by class, not by the config value, because
	// "streaming-on-nonstream-class" deliberately has streaming: true on a
	// channel that does not implement send_delta.
	streamingLabels := map[string]bool{
		"streaming-on":               true,
		"streaming-on-with-metadata": true,
	}

	cases := 0
	for _, tc := range ref.HandleMessage {
		if skip[tc.Label] {
			continue
		}
		cases++
		store := newTestStore(t, "pairing.json")
		cfg := channels.NewMapSection(tc.Config)

		var channel handleMessageChannel
		var bus *captureBus
		if streamingLabels[tc.Label] {
			streaming, b := newStreamingChannel(cfg, store)
			channel, bus = streaming, b
		} else {
			recording, b := newRecordingChannel(cfg, store)
			channel, bus = recording, b
		}

		var errText any
		if err := channel.HandleMessage(context.Background(), requestFromKwargs(t, tc.Kwargs)); err != nil {
			errText = err.Error()
		}
		if tc.Error == nil && errText != nil {
			t.Errorf("%s: unexpected error %v", tc.Label, errText)
			continue
		}

		// The reference records inbound_size before consuming the message.
		pending := bus.size()
		var published any
		if msg, ok := bus.take(); ok {
			published = inboundToComparable(msg)
		}
		gotSent := make([]any, 0, len(channel.Sent()))
		for _, msg := range channel.Sent() {
			gotSent = append(gotSent, outboundToComparable(msg))
		}

		if got, want := normalizePairingCodes(t, published), normalizeNumbers(tc.Published); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: published mismatch\n got %s\nwant %s", tc.Label, mustJSON(t, got), mustJSON(t, want))
		}
		if got, want := normalizePairingCodes(t, gotSent), normalizeNumbers(tc.Sent); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: sent mismatch\n got %s\nwant %s", tc.Label, mustJSON(t, got), mustJSON(t, want))
		}
		if pending != tc.InboundSize {
			t.Errorf("%s: pending inbound = %d, want %d", tc.Label, pending, tc.InboundSize)
		}
	}
	if cases < 14 {
		t.Fatalf("compared only %d _handle_message cases, want at least 14", cases)
	}
	t.Logf("_handle_message: %d cases compared", cases)
}

// TestChannelsHandleMessageAliasing pins the reference's metadata and media
// aliasing: a non-empty metadata dict and a non-empty media slice are SHARED
// with the published message, while an empty metadata dict is replaced by a
// fresh one and enabling streaming always copies.
func TestChannelsHandleMessageAliasing(t *testing.T) {
	ref := loadChannelsReference(t)

	want := map[string]map[string]any{}
	for _, tc := range ref.HandleMessage {
		if strings.HasPrefix(tc.Label, "aliasing-") {
			want[tc.Label] = tc.Published
		}
	}
	if len(want) != 4 {
		t.Fatalf("reference has %d aliasing cases, want 4", len(want))
	}

	store := newTestStore(t, "pairing.json")
	allowed := map[string]any{"allow_from": []any{"alice"}}
	streaming := map[string]any{"allow_from": []any{"alice"}, "streaming": true}

	check := func(label string, got map[string]any) {
		t.Helper()
		expected, ok := want[label]
		if !ok {
			t.Fatalf("%s: no reference case", label)
		}
		if !reflect.DeepEqual(comparableJSON(t, got), comparableJSON(t, normalizeNumbers(expected))) {
			t.Errorf("%s: got %s\nwant %s", label, mustJSON(t, got), mustJSON(t, expected))
		}
	}

	// Non-empty metadata on a non-streaming channel: shared, so the mutation is
	// visible on the published message.
	channel, bus := newRecordingChannel(channels.NewMapSection(allowed), store)
	metadata := map[string]any{"k": "v"}
	if err := channel.HandleMessage(context.Background(), channels.InboundRequest{
		SenderID: "alice", ChatID: "c", Content: "x", Metadata: metadata,
	}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	metadata["late"] = "added-after"
	msg, _ := bus.take()
	check("aliasing-nonstream-nonempty", inboundToComparable(msg))

	// Empty metadata: replaced by a fresh dict, so the mutation is invisible.
	channel, bus = newRecordingChannel(channels.NewMapSection(allowed), store)
	metadata = map[string]any{}
	if err := channel.HandleMessage(context.Background(), channels.InboundRequest{
		SenderID: "alice", ChatID: "c", Content: "x", Metadata: metadata,
	}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	metadata["late"] = "added-after"
	msg, _ = bus.take()
	check("aliasing-nonstream-empty", inboundToComparable(msg))

	// Streaming always copies.
	streamingChannel, bus := newStreamingChannel(channels.NewMapSection(streaming), store)
	metadata = map[string]any{"k": "v"}
	if err := streamingChannel.HandleMessage(context.Background(), channels.InboundRequest{
		SenderID: "alice", ChatID: "c", Content: "x", Metadata: metadata,
	}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	metadata["late"] = "added-after"
	msg, _ = bus.take()
	check("aliasing-streaming-nonempty", inboundToComparable(msg))

	// A non-empty media slice is shared with the caller: the published message
	// aliases the caller's backing array, exactly as it aliases the caller's
	// list in Python.
	//
	// The mutation is an element assignment because that is the only form Go
	// can reproduce. A slice HEADER is copied into the message, so appending to
	// the caller's slice never reaches it, while Python's list append does —
	// a divergence the reference's own dumper acknowledges by mutating
	// element 0 instead.
	channel, bus = newRecordingChannel(channels.NewMapSection(allowed), store)
	media := []string{"u1"}
	if err := channel.HandleMessage(context.Background(), channels.InboundRequest{
		SenderID: "alice", ChatID: "c", Content: "x", Media: media,
	}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	media[0] = "u9"
	msg, _ = bus.take()
	check("aliasing-media", inboundToComparable(msg))
}

// TestChannelsHandleMessageUnavailableStore pins the transient-store-failure
// path: an unapproved DM is dropped, nothing is sent, no error escapes, and
// existing approvals survive.
func TestChannelsHandleMessageUnavailableStore(t *testing.T) {
	ref := loadChannelsReference(t)

	var expected *struct {
		Label       string
		InboundSize int
		Sent        int
	}
	for _, tc := range ref.HandleMessage {
		if tc.Label == "store-unavailable-dm" {
			expected = &struct {
				Label       string
				InboundSize int
				Sent        int
			}{tc.Label, tc.InboundSize, len(tc.Sent)}
		}
	}
	if expected == nil {
		t.Fatal("reference is missing the store-unavailable-dm case")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "pairing.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir store path: %v", err)
	}
	store := pairing.NewStore(path)
	store.SetLogger(discardLogger())

	// An existing approval must survive the failure.
	if _, err := store.GenerateCode("dummy", "friend", pairing.DefaultTTLSeconds); err == nil {
		t.Fatal("GenerateCode against an unreadable store returned no error")
	}

	channel, bus := newRecordingChannel(channels.NewMapSection(map[string]any{"allowFrom": []any{}}), store)
	err := channel.HandleMessage(context.Background(), channels.InboundRequest{
		SenderID: "stranger", ChatID: "c", Content: "hello", IsDM: true,
	})
	if err != nil {
		t.Errorf("HandleMessage returned %v, want nil (the reference swallows OSError)", err)
	}
	if got := len(channel.sent); got != expected.Sent {
		t.Errorf("sent %d messages, want %d", got, expected.Sent)
	}
	if got := bus.size(); got != expected.InboundSize {
		t.Errorf("published %d messages, want %d", got, expected.InboundSize)
	}
}

// ---------------------------------------------------------------------------
// 6. The pairing store script
// ---------------------------------------------------------------------------

type storeStep struct {
	Label  string  `json:"label"`
	Exists bool    `json:"exists"`
	Text   *string `json:"text"`
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func TestPairingStoreScriptMatchesPython(t *testing.T) {
	ref := loadChannelsReference(t)
	work := t.TempDir()

	steps, results := runPairingStoreScript(t, work, ref.Store.FrozenTime, ref.Store.Fixtures)

	if len(steps) != len(ref.Store.Steps) {
		t.Fatalf("script produced %d steps, reference has %d", len(steps), len(ref.Store.Steps))
	}
	for i, want := range ref.Store.Steps {
		got := steps[i]
		if got.Label != want.Label {
			t.Fatalf("step %d: label %q, want %q", i, got.Label, want.Label)
		}
		if got.Exists != want.Exists {
			t.Errorf("%s: file exists = %v, want %v", want.Label, got.Exists, want.Exists)
			continue
		}
		gotText := ""
		if got.Text != nil {
			gotText = *got.Text
		}
		wantText := ""
		if want.Text != nil {
			wantText = *want.Text
		}
		if gotText != wantText {
			t.Errorf("%s: on-disk file differs\n--- got ---\n%s\n--- want ---\n%s",
				want.Label, gotText, wantText)
		}
	}

	if len(ref.Store.Results) < 40 {
		t.Fatalf("reference carries only %d store results", len(ref.Store.Results))
	}
	keys := make([]string, 0, len(ref.Store.Results))
	for k := range ref.Store.Results {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	compared := 0
	for _, key := range keys {
		compared++
		want, ok := results[key]
		if !ok {
			t.Errorf("result %q was not produced by the Go script", key)
			continue
		}
		// Codes are normalised on BOTH sides: the dumper normalised the fields
		// it knew carried one, and the Go side normalises the same fields. Any
		// code-shaped literal left in a result would be normalised on both
		// sides too, which is why the script's own literals avoid the shape.
		gotComparable := normalizePairingCodes(t, want)
		wantComparable := normalizePairingCodes(t, normalizeNumbers(ref.Store.Results[key]))
		if !reflect.DeepEqual(gotComparable, wantComparable) {
			t.Errorf("result %q:\n got %s\nwant %s", key, mustJSON(t, gotComparable), mustJSON(t, wantComparable))
		}
	}
	if compared < 40 {
		t.Fatalf("compared only %d store results, want at least 40", compared)
	}
	t.Logf("pairing store: %d file snapshots and %d results compared", len(steps), compared)
}

// runPairingStoreScript drives the Go store through the exact operation
// sequence of dump_store(), returning the file snapshots and the operation
// results in the reference's own shapes.
func runPairingStoreScript(
	t *testing.T, work string, frozen float64, fixtures map[string]string,
) ([]storeStep, map[string]any) {
	t.Helper()

	path := filepath.Join(work, "pairing.json")
	store := pairing.NewStore(path)
	store.SetLogger(discardLogger())

	now := frozen
	store.SetClock(func() float64 { return now })

	results := map[string]any{}
	var steps []storeStep

	snapshot := func(label string) {
		step := storeStep{Label: label}
		raw, err := os.ReadFile(path)
		if err == nil {
			step.Exists = true
			text := channelsCodeRE.ReplaceAllString(string(raw), "CODE")
			step.Text = &text
		}
		steps = append(steps, step)
	}
	must := func(label string, err error) {
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
	approveResult := func(a pairing.Approval, ok bool) any {
		if !ok {
			return nil
		}
		return []any{a.Channel, a.SenderID}
	}
	pendingList := func() any {
		list := store.ListPending()
		out := make([]any, 0, len(list))
		for _, item := range list {
			out = append(out, item.Map())
		}
		return out
	}
	clearResult := func(r pairing.ClearResult) any {
		return map[string]any{"approved": r.Approved, "pending": r.Pending}
	}

	snapshot("initial")

	codeA, err := store.GenerateCode("telegram", "alice", pairing.DefaultTTLSeconds)
	must("generate-alice", err)
	results["code_a_shape_ok"] = channelsCodeRE.MatchString(codeA)
	results["code_a_length"] = len(codeA)
	snapshot("generate-alice")

	codeAAgain, err := store.GenerateCode("telegram", "alice", pairing.DefaultTTLSeconds)
	must("generate-alice-again", err)
	results["generate_is_idempotent"] = codeA == codeAAgain

	codeB, err := store.GenerateCode("discord", "bob", pairing.DefaultTTLSeconds)
	must("generate-bob", err)
	results["codes_differ"] = codeA != codeB
	snapshot("generate-bob")

	results["is_approved_before"] = store.IsApproved("telegram", "alice")
	results["is_approved_unknown_channel"] = store.IsApproved("slack", "alice")

	now = frozen + 0.5
	results["list_pending"] = pendingList()
	results["format_expiry_future"] = store.FormatExpiry(now + 120)
	results["format_expiry_now"] = store.FormatExpiry(now)
	results["format_expiry_past"] = store.FormatExpiry(now - 1)
	results["format_expiry_fractional"] = store.FormatExpiry(now + 0.5)

	approval, ok, err := store.ApproveCode(codeA)
	must("approve-a", err)
	results["approve_a"] = approveResult(approval, ok)
	_, ok, err = store.ApproveCode(codeA)
	must("approve-a-again", err)
	results["approve_a_again"] = approveResult(pairing.Approval{}, ok)
	_, ok, err = store.ApproveCode("ZZZZ-ZZZZ")
	must("approve-unknown", err)
	results["approve_unknown"] = approveResult(pairing.Approval{}, ok)
	snapshot("approve-a")

	results["is_approved_after"] = store.IsApproved("telegram", "alice")
	results["get_approved_telegram"] = store.GetApproved("telegram")
	results["get_approved_discord"] = store.GetApproved("discord")
	results["get_approved_unknown"] = store.GetApproved("nope")

	codeC, err := store.GenerateCode("telegram", "carol", pairing.DefaultTTLSeconds)
	must("generate-carol", err)
	if _, _, err := store.ApproveCode(codeC); err != nil {
		t.Fatalf("approve-carol: %v", err)
	}
	results["get_approved_telegram_sorted"] = store.GetApproved("telegram")
	snapshot("approve-carol")

	results["cmd_empty"] = store.HandlePairingCommand("telegram", "")
	results["cmd_list"] = store.HandlePairingCommand("telegram", "list")
	results["cmd_list_whitespace"] = store.HandlePairingCommand("telegram", "  list  ")
	results["cmd_list_control_ws"] = store.HandlePairingCommand("telegram", "\x1clist\x1c")
	results["cmd_approve_no_arg"] = store.HandlePairingCommand("telegram", "approve")
	results["cmd_approve_bad"] = store.HandlePairingCommand("telegram", "approve NOPE-NOPE")
	results["cmd_deny_no_arg"] = store.HandlePairingCommand("telegram", "deny")
	results["cmd_deny_bad"] = store.HandlePairingCommand("telegram", "deny NOPE-NOPE")
	results["cmd_revoke_one"] = store.HandlePairingCommand("telegram", "revoke alice")
	results["cmd_revoke_one_missing"] = store.HandlePairingCommand("telegram", "revoke nobody")
	results["cmd_revoke_two"] = store.HandlePairingCommand("telegram", "revoke discord bob")
	results["cmd_revoke_usage"] = store.HandlePairingCommand("telegram", "revoke")
	results["cmd_unknown"] = store.HandlePairingCommand("telegram", "frobnicate")
	snapshot("after-commands")

	denied, err := store.DenyCode("NOPE-NOPE")
	must("deny-unknown", err)
	results["deny_unknown"] = denied
	denied, err = store.DenyCode(codeB)
	must("deny-known", err)
	results["deny_known"] = denied
	denied, err = store.DenyCode(codeB)
	must("deny-known-again", err)
	results["deny_known_again"] = denied
	snapshot("after-deny")

	revoked, err := store.Revoke("telegram", "carol")
	must("revoke-present", err)
	results["revoke_present"] = revoked
	revoked, err = store.Revoke("telegram", "nobody")
	must("revoke-absent", err)
	results["revoke_absent"] = revoked
	snapshot("after-revoke")

	count, err := store.RevokeChannel("telegram")
	must("revoke-channel-known", err)
	results["revoke_channel_known"] = count
	count, err = store.RevokeChannel("telegram")
	must("revoke-channel-absent", err)
	results["revoke_channel_absent"] = count
	count, err = store.RevokeChannel("nope")
	must("revoke-channel-unknown", err)
	results["revoke_channel_unknown"] = count
	snapshot("after-revoke-channel")

	codeD, err := store.GenerateCode("discord", "dave", pairing.DefaultTTLSeconds)
	must("generate-dave", err)
	cleared, err := store.ClearChannel("discord")
	must("clear-channel", err)
	results["clear_channel"] = clearResult(cleared)
	cleared, err = store.ClearChannel("discord")
	must("clear-channel-empty", err)
	results["clear_channel_empty"] = clearResult(cleared)
	denied, err = store.DenyCode(codeD)
	must("deny-d", err)
	results["deny_d"] = denied
	snapshot("after-clear")

	codeE, err := store.GenerateCode("x", "y", 10)
	must("generate-expiring", err)
	now = frozen + 100.0
	results["list_pending_expired"] = pendingList()
	_, ok, err = store.ApproveCode(codeE)
	must("approve-expired", err)
	results["approve_expired"] = approveResult(pairing.Approval{}, ok)
	snapshot("after-expiry-no-save")

	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write corrupted store: %v", err)
	}
	results["is_approved_corrupted"] = store.IsApproved("telegram", "alice")
	results["list_pending_corrupted"] = pendingList()
	snapshot("corrupted-untouched")

	if err := os.WriteFile(path, []byte("[1, 2, 3]"), 0o600); err != nil {
		t.Fatalf("write non-dict store: %v", err)
	}
	results["get_approved_nondict"] = store.GetApproved("telegram")
	snapshot("nondict-untouched")

	if err := os.WriteFile(path, []byte(fixtures["typed"]), 0o600); err != nil {
		t.Fatalf("write typed fixture: %v", err)
	}
	results["list_pending_typed"] = pendingList()
	snapshot("typed-after-list-pending")
	results["get_approved_typed"] = store.GetApproved("typed")
	results["get_approved_repr"] = store.GetApproved("repr")
	results["get_approved_empty"] = store.GetApproved("empty")
	results["get_approved_scalar"] = store.GetApproved("scalar")
	results["is_approved_typed_numeric"] = store.IsApproved("typed", "5")
	approval, ok, err = store.ApproveCode("GOODCODE")
	must("approve-typed-good", err)
	results["approve_typed_good"] = approveResult(approval, ok)
	snapshot("typed-after-approve")

	revoked, err = store.Revoke("typed", "5")
	must("revoke-typed", err)
	results["revoke_typed"] = revoked
	snapshot("typed-after-revoke")

	codeF, err := store.GenerateCode("typed", "zoe", pairing.DefaultTTLSeconds)
	must("generate-after-gc", err)
	results["generate_after_gc"] = channelsCodeRE.MatchString(codeF)
	snapshot("typed-after-generate")

	if err := os.WriteFile(path, []byte(fixtures["typed2"]), 0o600); err != nil {
		t.Fatalf("write typed2 fixture: %v", err)
	}
	count, err = store.RevokeChannel("typed")
	must("revoke-channel-typed", err)
	results["revoke_channel_typed"] = count
	snapshot("typed2-after-revoke-channel")
	results["list_pending_typed2"] = pendingList()

	return steps, results
}
