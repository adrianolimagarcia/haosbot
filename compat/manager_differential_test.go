package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/events"
)

// This file is deliberately self-contained: its own dumper
// (compat/python/dump_manager.py) and its own loader. It shares no declarations
// with differential_test.go or outbound_events_differential_test.go beyond
// repoRoot, because those files are edited concurrently by several agents and a
// read-modify-write race there would destroy work.
//
// The dumper emits both the scenario INPUTS and the reference OUTPUTS. The Go
// side rebuilds every message from the emitted input rather than re-deriving it
// from a hand-written fixture: a fixture that drifted from the scenario would
// otherwise look like an implementation bug.
//
// Everything below is driven through the EXPORTED surface of
// internal/channels. Nothing here reaches into the manager's private state,
// because a differential test that needed private access would not be testing
// the API the gateway actually uses.
//
// Every expectation below comes from executing the frozen reference. Nothing is
// transcribed from documentation.

// ---------------------------------------------------------------------------
// Dumper plumbing
// ---------------------------------------------------------------------------

func managerDumperPath(root string) string {
	return filepath.Join(root, "compat", "python", "dump_manager.py")
}

// The dumper drives the real asyncio dispatcher and takes several seconds, so
// its output is parsed once per package run.
var (
	managerDocOnce sync.Once
	managerDocData map[string]any
	managerDocErr  error
)

func loadManagerDump(t *testing.T) map[string]any {
	t.Helper()
	managerDocOnce.Do(func() {
		root := repoRootForManager()
		python := filepath.Join(root, ".tools", "venv", "bin", "python")
		script := managerDumperPath(root)
		if _, err := os.Stat(python); err != nil {
			managerDocErr = fmt.Errorf("reference venv not present at %s", python)
			return
		}
		if _, err := os.Stat(script); err != nil {
			managerDocErr = fmt.Errorf("dumper missing at %s", script)
			return
		}
		cmd := exec.Command(python, script)
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			var stderr string
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = string(ee.Stderr)
			}
			managerDocErr = fmt.Errorf("dumper failed: %v\nstderr:\n%s", err, stderr)
			return
		}
		var doc map[string]any
		if err := json.Unmarshal(out, &doc); err != nil {
			managerDocErr = fmt.Errorf("dumper output is not valid JSON: %w", err)
			return
		}
		managerDocData = doc
	})
	if managerDocErr != nil {
		t.Skipf("SKIP: %v — differential check not run", managerDocErr)
	}
	return managerDocData
}

// repoRootForManager locates the repository root without depending on
// differential_test.go, which other agents edit concurrently.
func repoRootForManager() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return filepath.Dir(wd) // compat/ -> repository root
}

func managerSection(t *testing.T, doc map[string]any, name string) []any {
	t.Helper()
	raw, ok := doc[name].([]any)
	if !ok {
		t.Fatalf("dump section %q missing or not a list", name)
	}
	if len(raw) == 0 {
		t.Fatalf("dump section %q is empty", name)
	}
	return raw
}

func asMap(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: want an object, got %T", what, v)
	}
	return m
}

func asList(t *testing.T, v any, what string) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: want a list, got %T", what, v)
	}
	return l
}

func asString(t *testing.T, v any, what string) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("%s: want a string, got %T", what, v)
	}
	return s
}

func asBool(t *testing.T, v any, what string) bool {
	t.Helper()
	b, ok := v.(bool)
	if !ok {
		t.Fatalf("%s: want a bool, got %T", what, v)
	}
	return b
}

func asInt(t *testing.T, v any, what string) int {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s: want a number, got %T", what, v)
	}
	return int(f)
}

func optString(v any) *string {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	return &s
}

func asStringSlice(t *testing.T, v any, what string) []string {
	t.Helper()
	list := asList(t, v, what)
	out := make([]string, 0, len(list))
	for i, item := range list {
		out = append(out, asString(t, item, fmt.Sprintf("%s[%d]", what, i)))
	}
	return out
}

func sortedManagerKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Message specs: rebuild a message from the dumper's description
// ---------------------------------------------------------------------------

type msgSpec struct {
	Channel        string
	ChatID         string
	Content        string
	Event          string
	StreamID       *string
	Resuming       bool
	MergeNext      bool
	Reasoning      bool
	ReasoningDelta bool
	ReasoningEnd   bool
	ToolHint       bool
	FileEditEvents []map[string]any
	Metadata       map[string]any
}

func parseMsgSpec(t *testing.T, v any, what string) msgSpec {
	t.Helper()
	m := asMap(t, v, what)
	s := msgSpec{
		Channel:  asString(t, m["channel"], what+".channel"),
		ChatID:   asString(t, m["chat_id"], what+".chat_id"),
		Content:  asString(t, m["content"], what+".content"),
		Event:    asString(t, m["event"], what+".event"),
		StreamID: optString(m["stream_id"]),
	}
	if b, ok := m["resuming"].(bool); ok {
		s.Resuming = b
	}
	if b, ok := m["merge_next"].(bool); ok {
		s.MergeNext = b
	}
	if b, ok := m["reasoning"].(bool); ok {
		s.Reasoning = b
	}
	if b, ok := m["reasoning_delta"].(bool); ok {
		s.ReasoningDelta = b
	}
	if b, ok := m["reasoning_end"].(bool); ok {
		s.ReasoningEnd = b
	}
	if b, ok := m["tool_hint"].(bool); ok {
		s.ToolHint = b
	}
	if raw, ok := m["file_edit_events"]; ok && raw != nil {
		for i, item := range asList(t, raw, what+".file_edit_events") {
			s.FileEditEvents = append(s.FileEditEvents,
				asMap(t, item, fmt.Sprintf("%s.file_edit_events[%d]", what, i)))
		}
	}
	if raw, ok := m["metadata"]; ok && raw != nil {
		s.Metadata = asMap(t, raw, what+".metadata")
	}
	return s
}

func (s msgSpec) buildEvent() core.AgentEvent {
	switch s.Event {
	case "none":
		return nil
	case "progress":
		return events.ProgressEvent{
			Content:        s.Content,
			ToolHint:       s.ToolHint,
			Reasoning:      s.Reasoning,
			ReasoningDelta: s.ReasoningDelta,
			ReasoningEnd:   s.ReasoningEnd,
			StreamID:       s.StreamID,
			FileEditEvents: s.FileEditEvents,
		}
	case "file_edit":
		return events.FileEditEvent{ProgressEvent: events.ProgressEvent{
			Content:        s.Content,
			FileEditEvents: s.FileEditEvents,
		}}
	case "stream_delta":
		return events.StreamDeltaEvent{Content: s.Content, StreamID: s.StreamID}
	case "stream_end":
		return events.StreamEndEvent{
			Content:   s.Content,
			StreamID:  s.StreamID,
			Resuming:  s.Resuming,
			MergeNext: s.MergeNext,
		}
	case "streamed_response":
		return events.StreamedResponseEvent{}
	case "retry_wait":
		return events.RetryWaitEvent{Content: s.Content}
	}
	panic("unknown event kind " + s.Event)
}

func (s msgSpec) buildMessage() core.OutboundMessage {
	event := s.buildEvent()
	if event == nil {
		return core.OutboundMessage{
			Channel: s.Channel, ChatID: s.ChatID, Content: s.Content,
			Metadata: cloneMetadata(s.Metadata),
		}
	}
	return events.OutboundMessageForEvent(
		s.Channel, s.ChatID, event, nil, cloneMetadata(s.Metadata),
	)
}

func cloneMetadata(in map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

// messageDesc is the Go mirror of the dumper's describe(): only the fields the
// two runtimes share, so any difference is a real behavioural difference.
func describeMessage(msg core.OutboundMessage) map[string]any {
	out := map[string]any{
		"channel":    msg.Channel,
		"chat_id":    msg.ChatID,
		"content":    msg.Content,
		"event":      "none",
		"stream_id":  nil,
		"resuming":   nil,
		"merge_next": nil,
		"metadata":   map[string]any{},
	}
	for k, v := range msg.Metadata {
		out["metadata"].(map[string]any)[k] = v
	}
	if msg.Event == nil {
		return out
	}
	out["event"] = msg.Event.EventName()
	if id := channels.EventStreamID(msg.Event); id != nil {
		out["stream_id"] = *id
	}
	if end, ok := msg.Event.(events.StreamEndEvent); ok {
		out["resuming"] = end.Resuming
		out["merge_next"] = end.MergeNext
	}
	return out
}

func descFromJSON(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m := asMap(t, v, what)
	out := map[string]any{
		"channel":    m["channel"],
		"chat_id":    m["chat_id"],
		"content":    m["content"],
		"event":      m["event"],
		"stream_id":  m["stream_id"],
		"resuming":   m["resuming"],
		"merge_next": m["merge_next"],
		"metadata":   m["metadata"],
	}
	if out["metadata"] == nil {
		out["metadata"] = map[string]any{}
	}
	return out
}

// ---------------------------------------------------------------------------
// The recording channel double
// ---------------------------------------------------------------------------

// recChannel mirrors the dumper's RecordingChannel: it records every outbound
// primitive with normalised arguments, and implements the capability interfaces
// directly so every recorded argument is explicit.
//
// It does NOT embed channels.Base, because Base's SendReasoning bridge is part
// of what is under test for the one-shot-reasoning arm; the dumper reproduces
// base.py:201-223 verbatim inside its double for the same reason.
type recChannel struct {
	mu sync.Mutex

	name          string
	sendProgress  bool
	sendToolHints bool
	showReasoning bool
	running       bool
	retryable     bool

	calls []string
	raw   [][]any

	// Start behaviour knobs. The zero value means "return nil immediately and
	// leave the running flag alone", which is what the status scenarios need;
	// the lifecycle scenarios opt in explicitly.
	startErr           error
	startErrMsg        string
	startBlocks        bool
	manageRunning      bool
	failFirstSends     int
	sendAttempts       int
	blockChat          string
	blockEvery         bool
	blockGate          chan struct{}
	stopGate           chan struct{}
	stopReached        chan struct{}
	stopReachedOnce    sync.Once
	manager            *channels.ChannelManager
	started            int
	stopped            int
	stopSawNoTasks     *bool
	startedObserved    chan struct{}
	startedObservedOne sync.Once
}

func newRecChannel(name string) *recChannel {
	return &recChannel{
		name: name, sendProgress: true, sendToolHints: true,
		showReasoning: true, retryable: true,
	}
}

func (c *recChannel) Name() string        { return c.name }
func (c *recChannel) DisplayName() string { return c.name }

func (c *recChannel) SendProgress() bool              { return c.sendProgress }
func (c *recChannel) SendToolHints() bool             { return c.sendToolHints }
func (c *recChannel) ShowReasoning() bool             { return c.showReasoning }
func (c *recChannel) IsRunning() bool                 { return c.running }
func (c *recChannel) ShouldRetrySendError(error) bool { return c.retryable }
func (c *recChannel) StartErrorMessage(error) string  { return c.startErrMsg }

func (c *recChannel) Start(ctx context.Context) error {
	c.mu.Lock()
	c.started++
	err := c.startErr
	blocks := c.startBlocks
	manage := c.manageRunning
	if c.startedObserved != nil {
		c.startedObservedOne.Do(func() { close(c.startedObserved) })
	}
	if manage && err == nil {
		c.running = true
	}
	c.mu.Unlock()

	if err != nil {
		return err
	}
	if !blocks {
		return nil
	}
	<-ctx.Done()
	if manage {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}
	return ctx.Err()
}

func (c *recChannel) Stop(context.Context) error {
	c.mu.Lock()
	c.stopped++
	c.running = false
	if c.manager != nil {
		drained := c.manager.OutboundTaskCount() == 0
		c.stopSawNoTasks = &drained
	}
	gate, reached := c.stopGate, c.stopReached
	c.mu.Unlock()

	if reached != nil {
		c.stopReachedOnce.Do(func() { close(reached) })
	}
	if gate != nil {
		<-gate
	}
	return nil
}

func (c *recChannel) record(entry ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.raw = append(c.raw, entry)
	c.calls = append(c.calls, normaliseCall(entry))
}

// gateFor reports the blocking gate that applies to a chat, if any.
func (c *recChannel) gateFor(chatID string) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blockGate == nil {
		return nil
	}
	if c.blockEvery || chatID == c.blockChat {
		return c.blockGate
	}
	return nil
}

func (c *recChannel) Send(ctx context.Context, msg core.OutboundMessage) error {
	c.mu.Lock()
	c.sendAttempts++
	attempt := c.sendAttempts
	failures := c.failFirstSends
	c.mu.Unlock()

	if gate := c.gateFor(msg.ChatID); gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.record("send", msg.ChatID, msg.Content, normaliseMetadata(msg.Metadata))
	if attempt <= failures {
		return fmt.Errorf("boom%d", attempt)
	}
	return nil
}

func (c *recChannel) SendDelta(
	_ context.Context, chatID, delta string, metadata map[string]any, opts channels.DeltaOptions,
) error {
	var streamID any
	if opts.StreamID != nil {
		streamID = *opts.StreamID
	}
	c.record("send_delta", chatID, delta, streamID,
		opts.StreamEnd, opts.Resuming, opts.MergeNext, normaliseMetadata(metadata))
	return nil
}

func (c *recChannel) SendReasoningDelta(
	_ context.Context, chatID, delta string, metadata map[string]any, streamID *string,
) error {
	var id any
	if streamID != nil {
		id = *streamID
	}
	c.record("send_reasoning_delta", chatID, delta, id, normaliseMetadata(metadata))
	return nil
}

func (c *recChannel) SendReasoningEnd(
	_ context.Context, chatID string, metadata map[string]any, streamID *string,
) error {
	var id any
	if streamID != nil {
		id = *streamID
	}
	c.record("send_reasoning_end", chatID, id, normaliseMetadata(metadata))
	return nil
}

func (c *recChannel) SendFileEditEvents(
	_ context.Context, chatID string, edits []map[string]any, metadata map[string]any,
) error {
	copied := make([]any, 0, len(edits))
	for _, e := range edits {
		copied = append(copied, normaliseMetadata(e))
	}
	c.record("send_file_edit_events", chatID, copied, normaliseMetadata(metadata))
	return nil
}

// SendReasoning reproduces base.py:201-223 verbatim, so the recorded delta+end
// pair is the reference's and not a Go invention.
func (c *recChannel) SendReasoning(ctx context.Context, msg core.OutboundMessage) error {
	if msg.Content == "" {
		return nil
	}
	streamID := channels.EventStreamID(msg.Event)
	if err := c.SendReasoningDelta(ctx, msg.ChatID, msg.Content, msg.Metadata, streamID); err != nil {
		return err
	}
	return c.SendReasoningEnd(ctx, msg.ChatID, msg.Metadata, streamID)
}

// --- knobs used by the scenarios ---

func (c *recChannel) blockSend(chatID string, gate chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blockChat, c.blockEvery, c.blockGate = chatID, false, gate
}

func (c *recChannel) blockEverySend(gate chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blockChat, c.blockEvery, c.blockGate = "", true, gate
}

func (c *recChannel) callStrings() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *recChannel) rawCalls() [][]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]any, len(c.raw))
	copy(out, c.raw)
	return out
}

func (c *recChannel) counters() (started, stopped int, sawNoTasks *bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started, c.stopped, c.stopSawNoTasks
}

func (c *recChannel) attempts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sendAttempts
}

func normaliseMetadata(in map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

// normaliseCall renders one recorded call exactly the way the dumper's JSON
// does, so the two sides compare as strings.
func normaliseCall(entry []any) string {
	parts := make([]string, 0, len(entry))
	for _, v := range entry {
		parts = append(parts, normaliseValue(v))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func normaliseValue(v any) string {
	switch value := v.(type) {
	case nil:
		return "null"
	case string:
		return pyJSONString(value)
	case bool:
		if value {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", value)
	case map[string]any:
		keys := make([]string, 0, len(value))
		for k := range value {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, pyJSONString(k)+": "+normaliseValue(value[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			parts = append(parts, normaliseValue(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return fmt.Sprintf("%v", v)
}

func pyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func normaliseJSONCall(t *testing.T, v any, what string) string {
	t.Helper()
	list := asList(t, v, what)
	parts := make([]string, 0, len(list))
	for _, item := range list {
		parts = append(parts, normaliseValue(item))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func jsonStrings(t *testing.T, list []any, what string) []string {
	t.Helper()
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, normaliseJSONCall(t, item, what))
	}
	return out
}

// ---------------------------------------------------------------------------
// Manager construction
// ---------------------------------------------------------------------------

func managerConfig(maxRetries int) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Channels.SendMaxRetries = maxRetries
	return cfg
}

func newCompatManager(t *testing.T, cfg *config.Config, opts ...channels.ManagerOption) (*channels.ChannelManager, *bus.Bus) {
	t.Helper()
	b := bus.New(bus.Options{})
	all := append([]channels.ManagerOption{
		channels.WithManagerLogger(slog.New(slog.NewTextHandler(discardWriter{}, nil))),
	}, opts...)
	return channels.New(cfg, b, all...), b
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func waitUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// managerEqualStrings compares two string slices treating nil and the empty
// slice as equal. The dump decodes an empty JSON list into a non-nil empty
// slice, while a Go accessor naturally returns nil for "nothing", and
// reflect.DeepEqual would report those as different.
func managerEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stopManager(t *testing.T, m *channels.ChannelManager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.StopAll(ctx); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 1. Constants and the retry schedule
// ---------------------------------------------------------------------------

func TestManagerConstantsMatchPython(t *testing.T) {
	doc := loadManagerDump(t)
	constants := asMap(t, doc["constants"], "constants")

	delays := asList(t, constants["send_retry_delays"], "send_retry_delays")
	got := channels.SendRetryDelays()
	if len(got) != len(delays) {
		t.Fatalf("SendRetryDelays() = %v, want %d entries", got, len(delays))
	}
	for i, want := range delays {
		if s := got[i].Seconds(); s != float64(asInt(t, want, "delay")) {
			t.Errorf("SendRetryDelays()[%d] = %vs, want %vs", i, s, want)
		}
	}

	cases := []struct {
		key  string
		want int
		got  int
	}{
		{"outbound_concurrency",
			asInt(t, constants["outbound_concurrency"], "outbound_concurrency"),
			channels.OutboundConcurrency},
		{"outbound_pending_limit",
			asInt(t, constants["outbound_pending_limit"], "outbound_pending_limit"),
			channels.OutboundPendingLimit},
		{"origin_reply_fingerprints_max_size",
			asInt(t, constants["origin_reply_fingerprints_max_size"], "origin_reply_fingerprints_max_size"),
			channels.OriginReplyFingerprintsMaxSize},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d (from the reference)", tc.key, tc.got, tc.want)
		}
	}
	t.Logf("manager differential: %d constants and %d retry delays compared against the reference",
		len(cases), len(delays))
}

// TestSendRetryScheduleMatchesPython pins the clamp rule
// `_SEND_RETRY_DELAYS[min(attempt - 1, len - 1)]` for attempts 1..8, which is
// where the "index past the end reuses the last delay" behaviour lives.
func TestManagerRetryScheduleMatchesPython(t *testing.T) {
	doc := loadManagerDump(t)
	raw := asList(t, doc["retry_schedule"], "retry_schedule")
	for i, want := range raw {
		attempt := i + 1
		wantSeconds := float64(asInt(t, want, "retry_schedule"))
		if got := channels.SendRetryDelay(attempt).Seconds(); got != wantSeconds {
			t.Errorf("SendRetryDelay(%d) = %vs, want %vs", attempt, got, wantSeconds)
		}
	}
	t.Logf("manager differential: retry schedule for attempts 1..%d compared against the reference", len(raw))
}

// ---------------------------------------------------------------------------
// 2. _fingerprint_content
// ---------------------------------------------------------------------------

func TestManagerFingerprintMatchesPython(t *testing.T) {
	doc := loadManagerDump(t)
	raw := managerSection(t, doc, "fingerprint")

	for i, item := range raw {
		m := asMap(t, item, fmt.Sprintf("fingerprint[%d]", i))
		in := asString(t, m["input"], "input")
		want := asString(t, m["digest"], "digest")
		if got := channels.FingerprintContent(in); got != want {
			t.Errorf("FingerprintContent(%q) = %q, want %q", in, got, want)
		}
	}
	t.Logf("manager differential: %d fingerprint digests compared against the reference", len(raw))
}

// ---------------------------------------------------------------------------
// 3. _should_suppress_outbound
// ---------------------------------------------------------------------------

func TestManagerSuppressionMatchesPython(t *testing.T) {
	doc := loadManagerDump(t)
	raw := managerSection(t, doc, "suppress")

	compared := 0
	for _, item := range raw {
		c := asMap(t, item, "suppress case")
		name := asString(t, c["name"], "name")
		t.Run(name, func(t *testing.T) {
			m, _ := newCompatManager(t, managerConfig(3))
			steps := asList(t, c["steps"], name+".steps")
			want := asList(t, c["results"], name+".results")

			for i, step := range steps {
				msg := parseMsgSpec(t, step, fmt.Sprintf("%s.steps[%d]", name, i)).buildMessage()
				got := m.ShouldSuppressOutbound(msg)
				if got != asBool(t, want[i], "result") {
					t.Errorf("step %d (%q): = %v, want %v", i, msg.Content, got, want[i])
				}
				compared++
			}
		})
	}
	t.Logf("manager differential: %d suppression decisions compared against the reference", compared)
}

// TestManagerFingerprintEvictionMatchesPython probes the bounded fingerprint
// memory the way a gateway exercises it: build the state by sending messages,
// then ask whether individual origin ids are still remembered. The reference's
// eviction order depends on a hit moving its key to the most-recent end, so this
// is what proves the port reproduced OrderedDict.move_to_end rather than a plain
// FIFO ring.
func TestManagerFingerprintEvictionMatchesPython(t *testing.T) {
	doc := loadManagerDump(t)
	want := asMap(t, doc["eviction"], "eviction")
	limit := asInt(t, want["limit"], "limit")

	if got := channels.OriginReplyFingerprintsMaxSize; got != limit {
		t.Fatalf("OriginReplyFingerprintsMaxSize = %d, want %d", got, limit)
	}

	// filled builds the reference's state: `limit` distinct origins, then a hit
	// on o0 (which must move it to the end), then one more insert.
	filled := func() *channels.ChannelManager {
		m, _ := newCompatManager(t, managerConfig(3))
		for i := 0; i < limit; i++ {
			m.ShouldSuppressOutbound(core.OutboundMessage{
				Channel: "mock", ChatID: "chat", Content: "hello",
				Metadata: map[string]any{"origin_message_id": fmt.Sprintf("o%d", i)},
			})
		}
		m.ShouldSuppressOutbound(core.OutboundMessage{
			Channel: "mock", ChatID: "chat", Content: "hello",
			Metadata: map[string]any{"origin_message_id": "o0"},
		})
		m.ShouldSuppressOutbound(core.OutboundMessage{
			Channel: "mock", ChatID: "chat", Content: "hello",
			Metadata: map[string]any{"origin_message_id": "new"},
		})
		return m
	}

	probes := asMap(t, want["probes"], "probes")
	keys := make([]string, 0, len(probes))
	for k := range probes {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	compared := 0
	for _, key := range keys {
		// A fresh manager per probe: asking about an unremembered origin INSERTS
		// it, so a shared manager would let one probe perturb the next.
		m := filled()
		got := m.ShouldSuppressOutbound(core.OutboundMessage{
			Channel: "mock", ChatID: "chat", Content: "hello",
			Metadata: map[string]any{"origin_message_id": key},
		})
		if wantBool := asBool(t, probes[key], "probe"); got != wantBool {
			t.Errorf("origin %q remembered = %v, want %v", key, got, wantBool)
		}
		compared++
	}
	t.Logf("manager differential: fingerprint eviction probed at %d origins (limit %d)", compared, limit)
}

// ---------------------------------------------------------------------------
// 4. _send_once
// ---------------------------------------------------------------------------

func TestManagerSendOnceMatchesPython(t *testing.T) {
	doc := loadManagerDump(t)
	raw := managerSection(t, doc, "send_once")

	compared := 0
	for _, item := range raw {
		c := asMap(t, item, "send_once case")
		name := asString(t, c["name"], "name")
		t.Run(name, func(t *testing.T) {
			m, _ := newCompatManager(t, managerConfig(3))
			ch := newRecChannel("mock")
			msg := parseMsgSpec(t, c["spec"], name+".spec").buildMessage()

			if err := m.SendOnce(context.Background(), ch, msg); err != nil {
				t.Fatalf("SendOnce: %v", err)
			}

			wantCalls := asList(t, c["calls"], name+".calls")
			gotCalls := ch.callStrings()
			want := jsonStrings(t, wantCalls, name)
			if !managerEqualStrings(gotCalls, want) {
				t.Fatalf("calls =\n  %v\nwant\n  %v", gotCalls, want)
			}
			compared += len(want)
		})
	}
	t.Logf("manager differential: %d _send_once dispatch calls compared against the reference", compared)
}

// ---------------------------------------------------------------------------
// 5. _coalesce_stream_deltas
// ---------------------------------------------------------------------------

func TestManagerCoalesceMatchesPython(t *testing.T) {
	doc := loadManagerDump(t)
	raw := managerSection(t, doc, "coalesce")

	compared := 0
	for _, item := range raw {
		c := asMap(t, item, "coalesce case")
		name := asString(t, c["name"], "name")
		t.Run(name, func(t *testing.T) {
			m, b := newCompatManager(t, managerConfig(3))
			for i, q := range asList(t, c["queued"], name+".queued") {
				msg := parseMsgSpec(t, q, fmt.Sprintf("%s.queued[%d]", name, i)).buildMessage()
				if err := b.PublishOutbound(context.Background(), msg); err != nil {
					t.Fatal(err)
				}
			}

			first := parseMsgSpec(t, c["first"], name+".first").buildMessage()
			merged, pending := m.CoalesceStreamDeltas(first)

			if got, want := describeMessage(merged), descFromJSON(t, c["merged"], name+".merged"); !reflect.DeepEqual(got, want) {
				t.Errorf("merged =\n  %v\nwant\n  %v", got, want)
			}
			compared++

			wantPending := asList(t, c["pending"], name+".pending")
			if len(pending) != len(wantPending) {
				t.Fatalf("pending has %d entries, want %d", len(pending), len(wantPending))
			}
			for i, wp := range wantPending {
				if got, want := describeMessage(pending[i]), descFromJSON(t, wp, "pending"); !reflect.DeepEqual(got, want) {
					t.Errorf("pending[%d] =\n  %v\nwant\n  %v", i, got, want)
				}
				compared++
			}

			if want := asInt(t, c["queue_left"], name+".queue_left"); b.OutboundSize() != want {
				t.Errorf("queue left = %d, want %d", b.OutboundSize(), want)
			}
		})
	}
	t.Logf("manager differential: %d coalescing results compared against the reference", compared)
}

// ---------------------------------------------------------------------------
// 6. _send_with_retry
// ---------------------------------------------------------------------------

func TestManagerRetryMatchesPython(t *testing.T) {
	doc := loadManagerDump(t)
	raw := managerSection(t, doc, "retry")

	compared := 0
	for _, item := range raw {
		c := asMap(t, item, "retry case")
		name := asString(t, c["name"], "name")
		t.Run(name, func(t *testing.T) {
			rec := &captureHandler{}
			m, _ := newCompatManager(t, managerConfig(asInt(t, c["max_retries"], "max_retries")),
				channels.WithManagerLogger(slog.New(rec)),
				channels.WithSendRetryDelays([]time.Duration{
					time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond,
				}))
			ch := newRecChannel("mock")
			ch.retryable = asBool(t, c["retryable"], "retryable")
			ch.failFirstSends = asInt(t, c["failures"], "failures")

			m.SendWithRetry(context.Background(), ch, core.OutboundMessage{
				Channel: "mock", ChatID: "c", Content: "x",
			}, nil)

			if want := asInt(t, c["attempts"], "attempts"); ch.attempts() != want {
				t.Errorf("attempts = %d, want %d", ch.attempts(), want)
			}
			wantSleeps := intsFromJSON(t, asList(t, c["sleep_ms"], "sleep_ms"))
			if got := rec.ints("delay_ms"); !reflect.DeepEqual(got, wantSleeps) {
				t.Errorf("backoff delays = %v ms, want %v ms", got, wantSleeps)
			}
			compared++
		})
	}
	t.Logf("manager differential: %d retry budgets/backoff schedules compared against the reference", compared)
}

// TestManagerRetryDeadlineMatchesPython checks the deadline arm, which overrides
// the attempt cap and clamps every delay to the remaining time
// (manager.py:1032-1036, :1044-1045).
func TestManagerRetryDeadlineMatchesPython(t *testing.T) {
	doc := loadManagerDump(t)
	want := asMap(t, doc["retry_deadline"], "retry_deadline")

	rec := &captureHandler{}
	m, _ := newCompatManager(t, managerConfig(1),
		channels.WithManagerLogger(slog.New(rec)),
		channels.WithSendRetryDelays([]time.Duration{
			time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond,
		}))
	ch := newRecChannel("mock")
	ch.failFirstSends = 1 << 30 // always fails

	deadline := time.Now().Add(50 * time.Millisecond)
	m.SendWithRetry(context.Background(), ch, core.OutboundMessage{
		Channel: "mock", ChatID: "c", Content: "x",
	}, &deadline)

	if asBool(t, want["attempts_exceed_cap"], "attempts_exceed_cap") && ch.attempts() <= 1 {
		t.Errorf("attempts = %d, want more than the cap of 1 (the deadline overrides it)", ch.attempts())
	}
	gotSleeps := rec.ints("delay_ms")
	wantFirst := intsFromJSON(t, asList(t, want["first_sleeps_ms"], "first_sleeps_ms"))
	if len(gotSleeps) < len(wantFirst) {
		t.Fatalf("only %d backoff delays observed, want at least %d", len(gotSleeps), len(wantFirst))
	}
	if !reflect.DeepEqual(gotSleeps[:len(wantFirst)], wantFirst) {
		t.Errorf("first backoff delays = %v ms, want %v ms", gotSleeps[:len(wantFirst)], wantFirst)
	}
	if asBool(t, want["last_sleep_is_a_clamp"], "last_sleep_is_a_clamp") {
		first := channels.SendRetryDelays()[0].Milliseconds()
		if last := gotSleeps[len(gotSleeps)-1]; int64(last) >= first {
			t.Errorf("last delay = %dms, want it clamped below the first schedule entry (%dms)", last, first)
		}
	}
	t.Logf("manager differential: deadline retry arm compared against the reference (%d attempts, %d backoffs)",
		ch.attempts(), len(gotSleeps))
}

func intsFromJSON(t *testing.T, list []any) []int {
	t.Helper()
	out := make([]int, 0, len(list))
	for i, v := range list {
		out = append(out, asInt(t, v, fmt.Sprintf("[%d]", i)))
	}
	return out
}

// captureHandler is a slog.Handler that records the numeric attributes the
// manager logs, which is how the Go side observes the backoff schedule without
// sleeping through it.
type captureHandler struct {
	mu    sync.Mutex
	attrs []slog.Attr
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	r.Attrs(func(a slog.Attr) bool {
		h.attrs = append(h.attrs, a)
		return true
	})
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.attrs = append(h.attrs, attrs...)
	return h
}

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// ints returns every recorded attribute with the given key, in order.
func (h *captureHandler) ints(key string) []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []int{}
	for _, a := range h.attrs {
		if a.Key != key {
			continue
		}
		switch a.Value.Kind() {
		case slog.KindInt64:
			out = append(out, int(a.Value.Int64()))
		case slog.KindDuration:
			out = append(out, int(a.Value.Duration().Milliseconds()))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 7. get_status
// ---------------------------------------------------------------------------

// fieldOrders extracts, for every object nested one level under the top-level
// object, the order its keys appear in the marshalled JSON. That is how the
// struct field order — and the conditional `error` key — is compared against
// the reference's dict insertion order.
func fieldOrders(t *testing.T, data []byte) map[string][]string {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("status JSON is not an object: %v", err)
	}
	out := map[string][]string{}
	for name, raw := range top {
		dec := json.NewDecoder(bytes.NewReader(raw))
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if delim, ok := tok.(json.Delim); !ok || delim != '{' {
			t.Fatalf("%s: want an object, got %v", name, tok)
		}
		var order []string
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			key, _ := keyTok.(string)
			order = append(order, key)
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		out[name] = order
	}
	return out
}

// statusScenario builds the state a status case describes, through the public
// lifecycle only.
func statusScenario(t *testing.T, c map[string]any, name string) *channels.ChannelManager {
	t.Helper()
	spec := asMap(t, c["spec"], name+".spec")
	m, _ := newCompatManager(t, managerConfig(3))

	startBehaviour := map[string]any{}
	if raw, ok := spec["start"]; ok && raw != nil {
		startBehaviour = asMap(t, raw, name+".start")
	}

	channelsField := asMap(t, spec["channels"], name+".channels")
	// The dump carries the insertion order explicitly: a JSON object is
	// unordered once decoded, and the reference's order IS the fixture.
	names := asStringSlice(t, c["channel_order"], name+".channel_order")
	for _, chName := range names {
		running, present := channelsField[chName]
		if !present {
			t.Fatalf("%s: channel_order names %q, which the spec does not describe", name, chName)
		}
		ch := newRecChannel(chName)
		ch.running = asBool(t, running, name+".channels."+chName)
		switch behaviour, _ := startBehaviour[chName].(string); behaviour {
		case "blocks":
			ch.startBlocks = true
		case "fails":
			ch.startErr = errors.New("nope")
		case "returns", "":
		default:
			t.Fatalf("unknown start behaviour %q", behaviour)
		}
		m.AddChannel(chName, ch)
	}

	for _, rawSpec := range asList(t, spec["specs"], name+".specs") {
		parts := asList(t, rawSpec, "spec")
		m.SetChannelRuntimeSpec(
			asString(t, parts[0], "runtime"),
			asString(t, parts[1], "owner"),
			asString(t, parts[2], "instance"),
		)
	}

	for errName, msg := range asMap(t, spec["errors"], name+".errors") {
		m.SetChannelError(errName, asString(t, msg, "error"))
	}

	if len(names) > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go func() { _ = m.StartAll(ctx) }()
		for _, chName := range names {
			ch, _ := m.GetChannel(chName)
			rec := ch.(*recChannel)
			waitUntil(t, chName+" to start", 5*time.Second, func() bool {
				started, _, _ := rec.counters()
				return started >= 1
			})
		}
		// A failing start records its error on a later scheduling step.
		for _, chName := range names {
			if behaviour, _ := startBehaviour[chName].(string); behaviour == "fails" {
				waitUntil(t, chName+"'s failure to be recorded", 5*time.Second, func() bool {
					_, ok := m.ChannelError(chName)
					return ok
				})
			}
		}
	}
	return m
}

func TestManagerStatusMatchesPython(t *testing.T) {
	doc := loadManagerDump(t)
	raw := managerSection(t, doc, "status")

	compared := 0
	for _, item := range raw {
		c := asMap(t, item, "status case")
		name := asString(t, c["name"], "name")
		t.Run(name, func(t *testing.T) {
			m := statusScenario(t, c, name)
			defer stopManager(t, m)

			status := m.GetStatus()
			wantStatus := asMap(t, c["status"], name+".status")

			gotKeys := make([]string, 0, len(status))
			for k := range status {
				gotKeys = append(gotKeys, k)
			}
			sort.Strings(gotKeys)
			wantKeys := sortedManagerKeys(wantStatus)
			if !reflect.DeepEqual(gotKeys, wantKeys) {
				t.Fatalf("status keys = %v, want %v", gotKeys, wantKeys)
			}

			for _, key := range gotKeys {
				want := asMap(t, wantStatus[key], "status entry")
				got := status[key]
				if got.Enabled != asBool(t, want["enabled"], "enabled") {
					t.Errorf("%s.enabled = %v, want %v", key, got.Enabled, want["enabled"])
				}
				if got.Running != asBool(t, want["running"], "running") {
					t.Errorf("%s.running = %v, want %v", key, got.Running, want["running"])
				}
				if wantState := asString(t, want["state"], "state"); got.State != wantState {
					t.Errorf("%s.state = %q, want %q", key, got.State, wantState)
				}
				if wantOwner := asString(t, want["owner"], "owner"); got.Owner != wantOwner {
					t.Errorf("%s.owner = %q, want %q", key, got.Owner, wantOwner)
				}
				if wantInstance := asString(t, want["instance_id"], "instance_id"); got.InstanceID != wantInstance {
					t.Errorf("%s.instance_id = %q, want %q", key, got.InstanceID, wantInstance)
				}
				wantErr, hasErr := want["error"]
				if hasErr {
					if got.Error != asString(t, wantErr, "error") {
						t.Errorf("%s.error = %q, want %q", key, got.Error, wantErr)
					}
				} else if got.Error != "" {
					t.Errorf("%s.error = %q, want it absent", key, got.Error)
				}
				compared++
			}

			// Insertion order: runtime specs first, then channels.
			if got, want := m.StatusOrder(), asStringSlice(t, c["order"], name+".order"); !managerEqualStrings(got, want) {
				t.Errorf("status order = %v, want %v", got, want)
			}
			if got, want := m.EnabledChannels(), asStringSlice(t, c["enabled_channels"], name+".enabled_channels"); !managerEqualStrings(got, want) {
				t.Errorf("enabled channels = %v, want %v", got, want)
			}

			// Field order, including the conditional error key, taken from the
			// marshalled JSON so it is the real wire format under test.
			data, err := json.Marshal(status)
			if err != nil {
				t.Fatalf("marshal status: %v", err)
			}
			gotOrders := fieldOrders(t, data)

			wantFieldOrder := asList(t, c["field_order"], name+".field_order")
			wantOrder := asStringSlice(t, c["order"], name+".order")
			if len(wantFieldOrder) != len(wantOrder) {
				t.Fatalf("field_order has %d entries but order has %d", len(wantFieldOrder), len(wantOrder))
			}
			for i, entryName := range wantOrder {
				wantFields := asStringSlice(t, wantFieldOrder[i], "field_order")
				gotFields := gotOrders[entryName]
				if !managerEqualStrings(gotFields, wantFields) {
					t.Errorf("%s field order = %v, want %v", entryName, gotFields, wantFields)
				}
				compared++
			}
		})
	}
	t.Logf("manager differential: %d status fields compared against the reference", compared)
}

// ---------------------------------------------------------------------------
// 8. The dispatcher
// ---------------------------------------------------------------------------

func TestManagerDispatchMatchesPython(t *testing.T) {
	doc := loadManagerDump(t)
	raw := managerSection(t, doc, "dispatch")

	compared := 0
	for _, item := range raw {
		c := asMap(t, item, "dispatch case")
		name := asString(t, c["name"], "name")
		t.Run(name, func(t *testing.T) {
			spec := asMap(t, c["spec"], name+".spec")
			m, b := newCompatManager(t, managerConfig(3))

			ch := newRecChannel("mock")
			for gate, value := range asMap(t, spec["gates"], name+".gates") {
				switch gate {
				case "send_progress":
					ch.sendProgress = asBool(t, value, gate)
				case "send_tool_hints":
					ch.sendToolHints = asBool(t, value, gate)
				case "show_reasoning":
					ch.showReasoning = asBool(t, value, gate)
				default:
					t.Fatalf("unknown gate %q", gate)
				}
			}
			m.AddChannel("mock", ch)

			// Publish before starting, exactly as the dumper does, so the
			// dispatcher drains the whole queue without waiting on its poll.
			for i, rawMsg := range asList(t, spec["messages"], name+".messages") {
				msg := parseMsgSpec(t, rawMsg, fmt.Sprintf("%s.messages[%d]", name, i)).buildMessage()
				if err := b.PublishOutbound(context.Background(), msg); err != nil {
					t.Fatal(err)
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { _ = m.StartAll(ctx) }()

			waitUntil(t, "the bus to drain and the outbound path to settle", 15*time.Second, func() bool {
				return b.OutboundSize() == 0 && m.OutboundTaskCount() == 0
			})
			// The queue can drain before the last task has run; a short quiet
			// period lets the final delivery land.
			time.Sleep(50 * time.Millisecond)

			wantPerDest := asMap(t, c["per_destination"], name+".per_destination")
			gotPerDest := map[string][]string{}
			for _, call := range ch.rawCalls() {
				chat, _ := call[1].(string)
				gotPerDest[chat] = append(gotPerDest[chat], normaliseCall(call))
			}

			gotChats := make([]string, 0, len(gotPerDest))
			for k := range gotPerDest {
				gotChats = append(gotChats, k)
			}
			sort.Strings(gotChats)
			wantChats := sortedManagerKeys(wantPerDest)
			if !reflect.DeepEqual(gotChats, wantChats) {
				t.Fatalf("destinations = %v, want %v (calls: %v)", gotChats, wantChats, ch.callStrings())
			}
			for _, chat := range gotChats {
				want := jsonStrings(t, asList(t, wantPerDest[chat], "calls"), "calls")
				if !managerEqualStrings(gotPerDest[chat], want) {
					t.Errorf("chat %q calls =\n  %v\nwant\n  %v", chat, gotPerDest[chat], want)
				}
				compared += len(want)
			}

			cancel()
			stopManager(t, m)

			if want := asInt(t, c["outbound_tasks_after"], "outbound_tasks_after"); m.OutboundTaskCount() != want {
				t.Errorf("outbound tasks after = %d, want %d", m.OutboundTaskCount(), want)
			}
			if want := asInt(t, c["outbound_tails_after"], "outbound_tails_after"); m.OutboundTailCount() != want {
				t.Errorf("outbound tails after = %d, want %d", m.OutboundTailCount(), want)
			}
		})
	}
	t.Logf("manager differential: %d dispatched calls compared against the reference", compared)
}

// ---------------------------------------------------------------------------
// 9. Lifecycle
// ---------------------------------------------------------------------------

func TestManagerLifecycleMatchesPython(t *testing.T) {
	doc := loadManagerDump(t)
	want := asMap(t, doc["lifecycle"], "lifecycle")

	// compared counts the individual lifecycle assertions made below, so the
	// differential reports a number rather than only "ok".
	compared := 0
	check := func(what string, got, want any) {
		compared++
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", what, got, want)
		}
	}

	t.Run("start_all_no_channels", func(t *testing.T) {
		m, b := newCompatManager(t, managerConfig(3))
		if err := m.StartAll(context.Background()); err != nil {
			t.Fatal(err)
		}
		w := asMap(t, want["start_all_no_channels"], "case")
		check("Started()", m.Started(), asBool(t, w["started"], "started"))
		if asBool(t, w["dispatch_task_is_none"], "dispatch_task_is_none") {
			// The reference returns before creating the dispatcher. Observed
			// behaviourally: a published message must stay on the bus.
			if err := b.PublishOutbound(context.Background(), core.OutboundMessage{
				Channel: "mock", ChatID: "c", Content: "x",
			}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
			if got := b.OutboundSize(); got != 1 {
				t.Errorf("bus size = %d, want 1 (no dispatcher should be consuming)", got)
			}
		}
	})

	t.Run("stop_all_without_start", func(t *testing.T) {
		m, _ := newCompatManager(t, managerConfig(3))
		if err := m.StopAll(context.Background()); err != nil {
			t.Fatal(err)
		}
		w := asMap(t, want["stop_all_without_start"], "case")
		check("Started()", m.Started(), asBool(t, w["started"], "started"))
	})

	t.Run("start_all_with_channels", func(t *testing.T) {
		m, _ := newCompatManager(t, managerConfig(3))
		bad := newRecChannel("bad")
		bad.startErr = errors.New("nope")
		bad.manageRunning = true
		good := newRecChannel("good")
		good.manageRunning = true
		m.AddChannel("bad", bad)
		m.AddChannel("good", good)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _ = m.StartAll(ctx) }()

		w := asMap(t, want["start_all_with_channels"], "case")
		waitUntil(t, "both channels to be started", 5*time.Second, func() bool {
			bs, _, _ := bad.counters()
			gs, _, _ := good.counters()
			return bs >= 1 && gs >= 1
		})
		waitUntil(t, "the start failure to be recorded", 5*time.Second, func() bool {
			_, ok := m.ChannelError("bad")
			return ok
		})

		check("Started()", m.Started(), asBool(t, w["started_flag"], "started_flag"))
		wantErrors := asMap(t, w["channel_errors"], "channel_errors")
		got, _ := m.ChannelError("bad")
		check("recorded error", got, asString(t, wantErrors["bad"], "bad error"))

		// The status the reference reports at this point.
		wantStatus := asMap(t, w["status"], "status")
		status := m.GetStatus()
		for key, rawEntry := range wantStatus {
			entry := asMap(t, rawEntry, "status entry")
			gotEntry, ok := status[key]
			if !ok {
				t.Errorf("status is missing %q", key)
				continue
			}
			check(key+".state", gotEntry.State, asString(t, entry["state"], "state"))
			check(key+".running", gotEntry.Running, asBool(t, entry["running"], "running"))
		}

		cancel()
		stopManager(t, m)

		stopWant := asMap(t, want["stop_all"], "stop_all")
		_, badStopped, _ := bad.counters()
		_, goodStopped, _ := good.counters()
		check("bad.stopped", badStopped, asInt(t, stopWant["bad_stopped"], "bad_stopped"))
		check("good.stopped", goodStopped, asInt(t, stopWant["good_stopped"], "good_stopped"))
		check("Started()", m.Started(), asBool(t, stopWant["started_flag"], "started_flag"))
		check("outbound tasks", m.OutboundTaskCount(),
			asInt(t, stopWant["outbound_tasks_after"], "tasks"))
		check("outbound tails", m.OutboundTailCount(),
			asInt(t, stopWant["outbound_tails_after"], "tails"))
	})

	t.Run("start_error_message", func(t *testing.T) {
		m, _ := newCompatManager(t, managerConfig(3))
		ch := newRecChannel("bad")
		ch.startErr = errors.New("nope")
		ch.startErrMsg = "install the SDK"
		m.AddChannel("bad", ch)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _ = m.StartAll(ctx) }()
		waitUntil(t, "the failure to be recorded", 5*time.Second, func() bool {
			_, ok := m.ChannelError("bad")
			return ok
		})

		wantErrors := asMap(t,
			asMap(t, want["start_error_message"], "case")["channel_errors"], "channel_errors")
		got, _ := m.ChannelError("bad")
		check("recorded error", got, asString(t, wantErrors["bad"], "bad error"))
		cancel()
		stopManager(t, m)
	})

	t.Run("stop_unknown_channel", func(t *testing.T) {
		m, _ := newCompatManager(t, managerConfig(3))
		w := asMap(t, want["stop_unknown_channel"], "case")
		stopped, err := m.StopChannel(context.Background(), "nope")
		if err != nil {
			t.Fatal(err)
		}
		check("StopChannel(unknown)", stopped, asBool(t, w["result"], "result"))
	})

	t.Run("stop_channel_cancels_sends_first", func(t *testing.T) {
		m, _ := newCompatManager(t, managerConfig(3))
		ch := newRecChannel("mock")
		ch.manager = m
		m.AddChannel("mock", ch)

		blocked := make(chan struct{})
		ch.blockSend("a", blocked)
		if err := m.QueueOutbound(context.Background(), ch, core.OutboundMessage{
			Channel: "mock", ChatID: "a", Content: "1",
		}); err != nil {
			t.Fatal(err)
		}
		// The reference waits for the outbound TASK to be registered, not for
		// Send to be entered: _queue_outbound creates the task first, and the
		// task then waits on its predecessor before touching the channel.
		waitUntil(t, "the outbound task to be registered", 5*time.Second, func() bool {
			return m.OutboundTaskCount() == 1
		})

		w := asMap(t, want["stop_channel_cancels_sends_first"], "case")
		stopped, err := m.StopChannel(context.Background(), "mock")
		if err != nil {
			t.Fatal(err)
		}
		check("StopChannel", stopped, asBool(t, w["result"], "result"))
		_, _, sawNoTasks := ch.counters()
		if sawNoTasks == nil {
			t.Fatal("Stop was never called on the channel")
		}
		check("Stop saw no tasks", *sawNoTasks, asBool(t, w["stop_saw_no_tasks"], "stop_saw_no_tasks"))
		check("outbound tasks", m.OutboundTaskCount(),
			asInt(t, w["outbound_tasks_after"], "tasks"))
		check("outbound tails", m.OutboundTailCount(),
			asInt(t, w["outbound_tails_after"], "tails"))
	})

	t.Run("queue_drops_when_stopping", func(t *testing.T) {
		m, _ := newCompatManager(t, managerConfig(3))
		ch := newRecChannel("mock")
		ch.stopGate = make(chan struct{})
		ch.stopReached = make(chan struct{})
		m.AddChannel("mock", ch)

		// StopChannel marks the channel as stopping BEFORE it cancels the
		// outbound tasks and before it calls Stop, so holding Stop open keeps
		// the mark in place.
		stopDone := make(chan struct{})
		go func() {
			defer close(stopDone)
			_, _ = m.StopChannel(context.Background(), "mock")
		}()
		waitUntil(t, "Stop to be reached", 5*time.Second, func() bool {
			select {
			case <-ch.stopReached:
				return true
			default:
				return false
			}
		})

		if err := m.QueueOutbound(context.Background(), ch, core.OutboundMessage{
			Channel: "mock", ChatID: "c", Content: "dropped",
		}); err != nil {
			t.Fatal(err)
		}
		w := asMap(t, want["queue_drops_when_stopping"], "case")
		check("outbound tasks", m.OutboundTaskCount(), asInt(t, w["outbound_tasks"], "tasks"))
		check("channel calls", len(ch.rawCalls()), 0)

		close(ch.stopGate)
		<-stopDone
	})

	t.Run("queue_drops_when_replaced", func(t *testing.T) {
		m, _ := newCompatManager(t, managerConfig(3))
		stale := newRecChannel("mock")
		m.AddChannel("mock", stale)
		replacement := newRecChannel("mock")
		m.AddChannel("mock", replacement)

		if err := m.QueueOutbound(context.Background(), stale, core.OutboundMessage{
			Channel: "mock", ChatID: "c", Content: "dropped",
		}); err != nil {
			t.Fatal(err)
		}
		w := asMap(t, want["queue_drops_when_replaced"], "case")
		check("outbound tasks", m.OutboundTaskCount(), asInt(t, w["outbound_tasks"], "tasks"))
		check("replacement calls", len(replacement.callStrings()), 0)
	})

	t.Run("dispatcher_shutdown", func(t *testing.T) {
		m, b := newCompatManager(t, managerConfig(3))
		ch := newRecChannel("mock")
		m.AddChannel("mock", ch)
		ch.blockEverySend(make(chan struct{}))

		for _, content := range []string{"one", "two", "three"} {
			if err := b.PublishOutbound(context.Background(), core.OutboundMessage{
				Channel: "mock", ChatID: "chat", Content: content,
			}); err != nil {
				t.Fatal(err)
			}
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _ = m.StartAll(ctx) }()

		w := asMap(t, want["dispatcher_shutdown"], "case")
		wantBefore := asInt(t, w["tasks_before"], "tasks_before")
		waitUntil(t, "the outbound tasks to be registered", 15*time.Second, func() bool {
			return m.OutboundTaskCount() == wantBefore
		})
		check("outbound tails before", m.OutboundTailCount(),
			asInt(t, w["tails_before"], "tails_before"))

		cancel()
		stopManager(t, m)

		check("outbound tasks after", m.OutboundTaskCount(),
			asInt(t, w["tasks_after"], "tasks_after"))
		check("outbound tails after", m.OutboundTailCount(),
			asInt(t, w["tails_after"], "tails_after"))
	})

	if compared == 0 {
		t.Fatal("no lifecycle assertions were compared")
	}
	t.Logf("manager differential: %d lifecycle assertions compared against the reference", compared)
}

// ---------------------------------------------------------------------------
// The documented divergences
// ---------------------------------------------------------------------------

// TestManagerDivergencesAreExactlyDocumented pins the behaviours that could not
// be reproduced, so a future change that silently widens the gap fails here
// instead of passing unnoticed.
//
// There are exactly TWO divergences in the P0.5/P0.6 surface:
//
//  1. RuntimeModelUpdatedEvent is not ported (see internal/events' package
//     comment), so _dispatch_outbound_loop's websocket gate
//     (manager.py:816-821) is implemented by comparing EventName() with
//     "RuntimeModelUpdatedEvent". No Go event carries that name today, so the
//     gate is inert — observationally identical to Python for every event this
//     port can build. A Python SUBCLASS of RuntimeModelUpdatedEvent would
//     satisfy isinstance and be dropped; a Go event would have to declare the
//     name itself.
//
//  2. Optional capability probes are type assertions with a defined fallback,
//     where Python reads a plain attribute that raises AttributeError when it
//     is absent: `channel.is_running` in get_status (manager.py:1078) and
//     `ch.send_progress` / `ch.send_tool_hints` / `ch.show_reasoning` in the
//     progress and reasoning gates (manager.py:353, :797). Unreachable in
//     either language for a channel that subclasses BaseChannel, because the
//     base supplies all four; it exists so a Go channel that omits an optional
//     capability gets BaseChannel's default instead of a panic. The alternative
//     — reproducing the AttributeError — would turn a missing capability into a
//     crashed dispatcher, which is strictly worse than the reference.
//
// Out of scope rather than divergent: the restart-notice path
// (_notify_restart_done_if_needed and _send_restart_notice_when_started,
// manager.py:618-666) depends on nanobot.utils.restart, which is not ported, so
// StartAll never schedules it. The deadline arm of SendWithRetry that path uses
// IS implemented and is compared by TestManagerRetryDeadlineMatchesPython.
func TestManagerDivergencesAreExactlyDocumented(t *testing.T) {
	const documented = 2

	divergences := []string{
		"RuntimeModelUpdatedEvent gate keyed on EventName (type not ported)",
		"capability probes fall back to the BaseChannel default where Python would raise AttributeError",
	}
	if len(divergences) != documented {
		t.Fatalf("documented divergences = %d, want exactly %d", len(divergences), documented)
	}
	t.Logf("manager differential: %d documented divergence(s): %v", documented, divergences)

	// Divergence 2, exercised: a channel that exposes none of the optional
	// capabilities must be reported as not running and must receive progress,
	// rather than panicking.
	t.Run("capability_fallbacks", func(t *testing.T) {
		m, b := newCompatManager(t, managerConfig(3))
		bare := &bareChannel{name: "bare"}
		m.AddChannel("bare", bare)

		status := m.GetStatus()["bare"]
		if status.Running {
			t.Error("a channel with no IsRunning() reported running = true")
		}
		if status.State != channels.ChannelStateStopped {
			t.Errorf("state = %q, want stopped", status.State)
		}

		if err := b.PublishOutbound(context.Background(), events.OutboundMessageForEvent(
			"bare", "c", events.ProgressEvent{Content: "p"}, nil, nil,
		)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _ = m.StartAll(ctx) }()
		waitUntil(t, "the progress delivery", 15*time.Second, func() bool {
			return bare.sends() == 1
		})
		cancel()
		stopManager(t, m)
	})

	// The gate is exercised so the divergence is a tested claim rather than a
	// comment: an event NAMED RuntimeModelUpdatedEvent addressed to a registered
	// "websocket" channel must be delivered (the reference drops it only when
	// the channel is NOT registered).
	m, b := newCompatManager(t, managerConfig(3))
	ch := newRecChannel("websocket")
	m.AddChannel("websocket", ch)
	if err := b.PublishOutbound(context.Background(), core.OutboundMessage{
		Channel: "websocket", ChatID: "c", Content: "update",
		Event: namedEvent{name: "RuntimeModelUpdatedEvent"},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.StartAll(ctx) }()
	waitUntil(t, "the websocket delivery", 15*time.Second, func() bool {
		return len(ch.callStrings()) == 1
	})
	cancel()
	stopManager(t, m)
}

// namedEvent is a test-only event that declares an arbitrary EventName, which
// is how the unported RuntimeModelUpdatedEvent gate is exercised.
type namedEvent struct{ name string }

func (e namedEvent) EventName() string { return e.name }

// bareChannel implements only the required Channel methods: no RunningState and
// no DeliveryPolicy. It exists to exercise the documented capability fallbacks.
type bareChannel struct {
	name  string
	mu    sync.Mutex
	count int
}

func (c *bareChannel) Name() string                { return c.name }
func (c *bareChannel) DisplayName() string         { return c.name }
func (c *bareChannel) Start(context.Context) error { return nil }
func (c *bareChannel) Stop(context.Context) error  { return nil }
func (c *bareChannel) Send(context.Context, core.OutboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
	return nil
}

func (c *bareChannel) sends() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}
