package channels

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/events"
	"github.com/adrianolimagarcia/nanobot-go/internal/pairing"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

type recordingBus struct {
	mu   sync.Mutex
	msgs []core.InboundMessage
	err  error
}

func (b *recordingBus) PublishInbound(_ context.Context, msg core.InboundMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	b.msgs = append(b.msgs, msg)
	return nil
}

func (b *recordingBus) take() (core.InboundMessage, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.msgs) == 0 {
		return core.InboundMessage{}, false
	}
	msg := b.msgs[0]
	b.msgs = b.msgs[1:]
	return msg, true
}

func (b *recordingBus) size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.msgs)
}

// testChannel is the minimal concrete channel: the three methods the ABC
// requires, plus a record of what Send delivered.
type testChannel struct {
	*Base

	mu       sync.Mutex
	sent     []core.OutboundMessage
	sendErr  error
	startErr error
}

func (c *testChannel) Start(context.Context) error { return c.startErr }
func (c *testChannel) Stop(context.Context) error  { return nil }

func (c *testChannel) Send(_ context.Context, msg core.OutboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendErr != nil {
		return c.sendErr
	}
	c.sent = append(c.sent, msg)
	return nil
}

func (c *testChannel) sentMessages() []core.OutboundMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]core.OutboundMessage, len(c.sent))
	copy(out, c.sent)
	return out
}

// streamChannel additionally implements DeltaSender, which is what
// SupportsStreaming probes for.
type streamChannel struct {
	*testChannel

	mu        sync.Mutex
	deltas    []string
	deltaOpts []DeltaOptions
	deltaErr  error
}

func (c *streamChannel) SendDelta(
	_ context.Context, _ string, delta string, _ map[string]any, opts DeltaOptions,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deltaErr != nil {
		return c.deltaErr
	}
	c.deltas = append(c.deltas, delta)
	c.deltaOpts = append(c.deltaOpts, opts)
	return nil
}

// reasoningChannel implements both reasoning halves, so the default
// SendReasoning bridge has something to call.
type reasoningChannel struct {
	*testChannel

	mu      sync.Mutex
	deltas  []string
	ends    int
	deltaID *string
	endID   *string
	err     error
}

func (c *reasoningChannel) SendReasoningDelta(
	_ context.Context, _ string, delta string, _ map[string]any, streamID *string,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.deltas = append(c.deltas, delta)
	c.deltaID = streamID
	return nil
}

func (c *reasoningChannel) SendReasoningEnd(
	_ context.Context, _ string, _ map[string]any, streamID *string,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ends++
	c.endID = streamID
	return nil
}

// ---------------------------------------------------------------------------
// Construction helpers
// ---------------------------------------------------------------------------

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestStore(t *testing.T) *pairing.Store {
	t.Helper()
	store := pairing.NewStore(filepath.Join(t.TempDir(), "pairing.json"))
	store.SetLogger(discardLogger())
	return store
}

func newChannel(t *testing.T, cfg Section, store *pairing.Store, opts ...Option) (*testChannel, *recordingBus) {
	t.Helper()
	bus := &recordingBus{}
	c := &testChannel{}
	base := append([]Option{WithName("test"), WithLogger(discardLogger())}, opts...)
	if store != nil {
		base = append(base, WithPairingStore(store))
	}
	c.Base = NewBase(c, cfg, bus, base...)
	return c, bus
}

func newStreamingChannel(t *testing.T, cfg Section, store *pairing.Store) (*streamChannel, *recordingBus) {
	t.Helper()
	bus := &recordingBus{}
	c := &streamChannel{testChannel: &testChannel{}}
	c.Base = NewBase(c, cfg, bus, WithName("test"), WithLogger(discardLogger()), WithPairingStore(store))
	return c, bus
}

func mapSection(m map[string]any) Section { return NewMapSection(m) }

// ---------------------------------------------------------------------------
// Construction and defaults
// ---------------------------------------------------------------------------

func TestNewBaseDefaults(t *testing.T) {
	bus := &recordingBus{}
	c := &testChannel{}
	c.Base = NewBase(c, mapSection(nil), bus)

	if got := c.Name(); got != DefaultName {
		t.Errorf("Name() = %q, want %q", got, DefaultName)
	}
	if got := c.DisplayName(); got != DefaultDisplayName {
		t.Errorf("DisplayName() = %q, want %q", got, DefaultDisplayName)
	}
	if c.IsRunning() {
		t.Error("a freshly built channel reports itself running")
	}
	// Python's class defaults are all true (base.py:31-33).
	if !c.SendProgress() || !c.SendToolHints() || !c.ShowReasoning() {
		t.Errorf("delivery defaults = (%v, %v, %v), want all true",
			c.SendProgress(), c.SendToolHints(), c.ShowReasoning())
	}
	if _, ok := c.Config().Map(); !ok {
		t.Error("Config() did not preserve a nil map section")
	}
	if c.Bus() != bus {
		t.Error("Bus() did not return the bus it was built with")
	}
	if c.Logger() == nil {
		t.Error("Logger() is nil")
	}
	if c.PairingStore() == nil {
		t.Error("PairingStore() is nil; the process-wide store should be the default")
	}
}

func TestNewBaseOptions(t *testing.T) {
	store := newTestStore(t)
	c, _ := newChannel(t, mapSection(map[string]any{"allow_from": []any{"alice"}}), store,
		WithName("telegram"), WithDisplayName("Telegram"), WithDeliveryPolicy(false, true, false))

	if got := c.Name(); got != "telegram" {
		t.Errorf("Name() = %q, want telegram", got)
	}
	if got := c.DisplayName(); got != "Telegram" {
		t.Errorf("DisplayName() = %q, want Telegram", got)
	}
	if c.SendProgress() || !c.SendToolHints() || c.ShowReasoning() {
		t.Errorf("delivery policy = (%v, %v, %v), want (false, true, false)",
			c.SendProgress(), c.SendToolHints(), c.ShowReasoning())
	}
	if c.PairingStore() != store {
		t.Error("WithPairingStore did not take effect")
	}
}

func TestBaseSettersAndIdentity(t *testing.T) {
	c, _ := newChannel(t, mapSection(nil), newTestStore(t))

	c.SetName("renamed")
	if got := c.Name(); got != "renamed" {
		t.Errorf("Name() = %q after SetName, want renamed", got)
	}
	c.SetRunning(true)
	if !c.IsRunning() {
		t.Error("IsRunning() = false after SetRunning(true)")
	}
	c.SetRunning(false)
	if c.IsRunning() {
		t.Error("IsRunning() = true after SetRunning(false)")
	}
	c.SetSendProgress(false)
	c.SetSendToolHints(false)
	c.SetShowReasoning(false)
	if c.SendProgress() || c.SendToolHints() || c.ShowReasoning() {
		t.Error("a delivery setter did not take effect")
	}
	c.SetPairingStore(nil)
	if c.PairingStore() == nil {
		t.Error("SetPairingStore(nil) should fall back to the process-wide store, not nil")
	}
}

func TestBaseSatisfiesAlwaysAvailableInterfaces(t *testing.T) {
	c, _ := newChannel(t, mapSection(nil), newTestStore(t))

	// These are the capabilities the manager can rely on for every channel.
	var _ DeliveryPolicy = c
	var _ ProgressTransportDefaults = c
	var _ SendErrorPolicy = c
	var _ StartErrorMessenger = c
	var _ LoginProvider = c
	var _ ConfigDefaulter = c
	var _ FeatureRefresher = c
	var _ RunningState = c
	var _ ConfigProvider = c

	// The capability interfaces must NOT be satisfied: their presence is the
	// test, so a default implementation would make every channel look streaming.
	if _, ok := any(c).(DeltaSender); ok {
		t.Error("Base satisfies DeltaSender; SupportsStreaming would always be true")
	}
	if _, ok := any(c).(ReasoningDeltaSender); ok {
		t.Error("Base satisfies ReasoningDeltaSender")
	}
	if _, ok := any(c).(ReasoningEndSender); ok {
		t.Error("Base satisfies ReasoningEndSender")
	}
	if _, ok := any(c).(FileEditSender); ok {
		t.Error("Base satisfies FileEditSender")
	}
}

func TestBaseDefaultMethodResults(t *testing.T) {
	c, _ := newChannel(t, mapSection(nil), newTestStore(t))

	if progress, hints, ok := c.ProgressTransportDefaults(); progress || hints || ok {
		t.Errorf("ProgressTransportDefaults() = (%v, %v, %v), want all false", progress, hints, ok)
	}
	if !c.ShouldRetrySendError(errors.New("boom")) {
		t.Error("ShouldRetrySendError = false; the reference retries every failure")
	}
	if got := c.StartErrorMessage(errors.New("boom")); got != "" {
		t.Errorf("StartErrorMessage = %q, want empty (no channel-specific message)", got)
	}
	if ok, err := c.Login(context.Background(), false); !ok || err != nil {
		t.Errorf("Login = (%v, %v), want (true, nil)", ok, err)
	}
	if got := c.DefaultConfig(); len(got) != 1 || got["enabled"] != false {
		t.Errorf("DefaultConfig() = %v, want {enabled: false}", got)
	}
	if c.RefreshFeatureMetadata("", "") {
		t.Error("RefreshFeatureMetadata = true, want false")
	}
	if got := c.TranscribeAudio(context.Background(), "/x.ogg"); got != "" {
		t.Errorf("TranscribeAudio = %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// IsAllowed
// ---------------------------------------------------------------------------

func TestIsAllowedExactMatchOnly(t *testing.T) {
	store := newTestStore(t)
	c, _ := newChannel(t, mapSection(map[string]any{"allow_from": []any{"alice", "bob@example.com"}}), store)

	for _, sender := range []string{"alice", "bob@example.com"} {
		if !c.IsAllowed(sender) {
			t.Errorf("IsAllowed(%q) = false, want true", sender)
		}
	}
	for _, sender := range []string{"Alice", " alice", "alice ", "alice|bob@example.com", "", "alic"} {
		if c.IsAllowed(sender) {
			t.Errorf("IsAllowed(%q) = true; the reference matches exactly, with no trimming or folding", sender)
		}
	}
}

func TestIsAllowedWildcard(t *testing.T) {
	store := newTestStore(t)
	c, _ := newChannel(t, mapSection(map[string]any{"allow_from": []any{"*"}}), store)

	for _, sender := range []string{"anyone", "", "12345"} {
		if !c.IsAllowed(sender) {
			t.Errorf("IsAllowed(%q) = false with a wildcard allowlist", sender)
		}
	}
}

func TestIsAllowedEmptyListFallsThroughToPairingStore(t *testing.T) {
	store := newTestStore(t)
	c, _ := newChannel(t, mapSection(map[string]any{"allow_from": []any{}}), store)

	if c.IsAllowed("alice") {
		t.Fatal("IsAllowed(alice) = true on an empty store")
	}
	code, err := store.GenerateCode("test", "alice", pairing.DefaultTTLSeconds)
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	if _, _, err := store.ApproveCode(code); err != nil {
		t.Fatalf("ApproveCode: %v", err)
	}
	if !c.IsAllowed("alice") {
		t.Error("an approved sender is still denied")
	}
	if c.IsAllowed("mallory") {
		t.Error("an unapproved sender was allowed")
	}
}

func TestIsAllowedNoAllowlistKey(t *testing.T) {
	c, _ := newChannel(t, mapSection(map[string]any{"other": 1}), newTestStore(t))
	if c.IsAllowed("alice") {
		t.Error("a config without allowFrom authorized a sender")
	}
}

// ---------------------------------------------------------------------------
// supports_streaming
// ---------------------------------------------------------------------------

func TestSupportsStreamingRequiresBothConfigAndCapability(t *testing.T) {
	store := newTestStore(t)

	plain, _ := newChannel(t, mapSection(map[string]any{"streaming": true}), store)
	if plain.SupportsStreaming() {
		t.Error("a channel without SendDelta reported streaming")
	}

	for _, streaming := range []any{true, "yes", 1, []any{0}} {
		c, _ := newStreamingChannel(t, mapSection(map[string]any{"streaming": streaming}), store)
		if !c.SupportsStreaming() {
			t.Errorf("streaming=%#v: SupportsStreaming() = false, want true (bool() is a truthiness test)", streaming)
		}
	}
	for _, streaming := range []any{false, "", 0, nil, []any{}} {
		c, _ := newStreamingChannel(t, mapSection(map[string]any{"streaming": streaming}), store)
		if c.SupportsStreaming() {
			t.Errorf("streaming=%#v: SupportsStreaming() = true, want false", streaming)
		}
	}
}

// ---------------------------------------------------------------------------
// HandleMessage
// ---------------------------------------------------------------------------

func TestHandleMessagePublishesAllowedSender(t *testing.T) {
	c, bus := newChannel(t, mapSection(map[string]any{"allow_from": []any{"alice"}}), newTestStore(t))

	if err := c.HandleMessage(context.Background(), InboundRequest{
		SenderID: "alice", ChatID: "chat-1", Content: "hello", Media: []string{"u1"},
		Metadata: map[string]any{"k": "v"},
	}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	msg, ok := bus.take()
	if !ok {
		t.Fatal("nothing was published")
	}
	if msg.Channel != "test" || msg.SenderID != "alice" || msg.ChatID != "chat-1" || msg.Content != "hello" {
		t.Errorf("published %+v", msg)
	}
	if len(msg.Media) != 1 || msg.Media[0] != "u1" {
		t.Errorf("media = %v, want [u1]", msg.Media)
	}
	if msg.Metadata["k"] != "v" {
		t.Errorf("metadata = %v, want k=v", msg.Metadata)
	}
	if got := msg.SessionKey(); got != "test:chat-1" {
		t.Errorf("session key = %q, want test:chat-1", got)
	}
	if !msg.IsUserInput() {
		t.Error("IsUserInput() = false for a normal inbound message")
	}
	if len(c.sentMessages()) != 0 {
		t.Error("an allowed message triggered a pairing reply")
	}
}

func TestHandleMessageNormalizesEmptyCollections(t *testing.T) {
	c, bus := newChannel(t, mapSection(map[string]any{"allow_from": []any{"alice"}}), newTestStore(t))

	if err := c.HandleMessage(context.Background(), InboundRequest{SenderID: "alice", ChatID: "c", Content: "x"}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	msg, _ := bus.take()
	// Python: `media or []` and `metadata or {}` — the fields are never null.
	if msg.Media == nil {
		t.Error("Media is nil; it must be an empty slice so the JSON field is [] not null")
	}
	if msg.Metadata == nil {
		t.Error("Metadata is nil; it must be an empty map so the JSON field is {} not null")
	}
}

func TestHandleMessageDeniesNonDMWithoutReply(t *testing.T) {
	c, bus := newChannel(t, mapSection(map[string]any{"allow_from": []any{}}), newTestStore(t))

	if err := c.HandleMessage(context.Background(), InboundRequest{
		SenderID: "stranger", ChatID: "group", Content: "hi",
	}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if bus.size() != 0 {
		t.Error("a denied group message was published")
	}
	if len(c.sentMessages()) != 0 {
		t.Error("a denied group message received a pairing reply")
	}
}

func TestHandleMessageDMSendsPairingCode(t *testing.T) {
	store := newTestStore(t)
	c, bus := newChannel(t, mapSection(map[string]any{"allow_from": []any{}}), store)

	if err := c.HandleMessage(context.Background(), InboundRequest{
		SenderID: "stranger", ChatID: "dm-1", Content: "hi", IsDM: true,
	}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if bus.size() != 0 {
		t.Error("a denied DM was published to the bus")
	}

	sent := c.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want exactly 1 pairing reply", len(sent))
	}
	reply := sent[0]
	if reply.Channel != "test" || reply.ChatID != "dm-1" {
		t.Errorf("reply routed to %s/%s, want test/dm-1", reply.Channel, reply.ChatID)
	}
	code, ok := reply.Metadata[pairing.PairingCodeMetaKey].(string)
	if !ok || code == "" {
		t.Fatalf("reply metadata lacks %s: %v", pairing.PairingCodeMetaKey, reply.Metadata)
	}
	if reply.Content != pairing.FormatPairingReply(code) {
		t.Errorf("reply content = %q, want the formatted pairing reply", reply.Content)
	}
	if reply.Media == nil || reply.Buttons == nil {
		t.Error("the reply must carry empty media and buttons, not nil")
	}

	// The code must be live, and a second DM must reuse it rather than minting
	// another one.
	if err := c.HandleMessage(context.Background(), InboundRequest{
		SenderID: "stranger", ChatID: "dm-1", Content: "hi again", IsDM: true,
	}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	sent = c.sentMessages()
	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want 2", len(sent))
	}
	second, _ := sent[1].Metadata[pairing.PairingCodeMetaKey].(string)
	if second != code {
		t.Errorf("a repeated DM minted a new code (%q then %q); generate_code is idempotent per sender", code, second)
	}

	// Approving the code must stop the pairing replies.
	if _, ok, err := store.ApproveCode(code); err != nil || !ok {
		t.Fatalf("ApproveCode = (_, %v, %v)", ok, err)
	}
	if err := c.HandleMessage(context.Background(), InboundRequest{
		SenderID: "stranger", ChatID: "dm-1", Content: "now approved", IsDM: true,
	}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if got := len(c.sentMessages()); got != 2 {
		t.Errorf("an approved DM still received a pairing reply (%d sent)", got)
	}
	if bus.size() != 1 {
		t.Errorf("published %d messages, want 1", bus.size())
	}
}

func TestHandleMessageAuthorizationIDScopesTheCheck(t *testing.T) {
	c, bus := newChannel(t, mapSection(map[string]any{"allow_from": []any{"group-42"}}), newTestStore(t))

	// The sender is a stranger but the GROUP is allowed: authorization uses the
	// authorization id while the recorded sender stays the real one.
	authID := "group-42"
	if err := c.HandleMessage(context.Background(), InboundRequest{
		SenderID: "member-7", ChatID: "group-42", Content: "hi", AuthorizationID: &authID,
	}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	msg, ok := bus.take()
	if !ok {
		t.Fatal("an allowed group was not published")
	}
	if msg.SenderID != "member-7" {
		t.Errorf("SenderID = %q, want the real sender member-7", msg.SenderID)
	}

	// An explicit empty authorization id authorizes against "" and therefore
	// denies, even though the sender is on the list.
	empty := ""
	if err := c.HandleMessage(context.Background(), InboundRequest{
		SenderID: "group-42", ChatID: "c", Content: "hi", AuthorizationID: &empty,
	}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if bus.size() != 0 {
		t.Error("an empty authorization id did not deny")
	}
}

func TestHandleMessageStampsWantsStream(t *testing.T) {
	store := newTestStore(t)

	streaming, bus := newStreamingChannel(t, mapSection(map[string]any{
		"allow_from": []any{"alice"}, "streaming": true,
	}), store)
	if err := streaming.HandleMessage(context.Background(), InboundRequest{
		SenderID: "alice", ChatID: "c", Content: "x", Metadata: map[string]any{"k": "v"},
	}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	msg, _ := bus.take()
	if msg.Metadata[WantsStreamMetaKey] != true {
		t.Errorf("metadata = %v, want %s=true", msg.Metadata, WantsStreamMetaKey)
	}
	if msg.Metadata["k"] != "v" {
		t.Errorf("stamping the stream flag dropped the caller's metadata: %v", msg.Metadata)
	}

	// A channel that supports streaming but has it disabled in config must not
	// stamp anything.
	off, bus2 := newStreamingChannel(t, mapSection(map[string]any{
		"allow_from": []any{"alice"}, "streaming": false,
	}), store)
	if err := off.HandleMessage(context.Background(), InboundRequest{
		SenderID: "alice", ChatID: "c", Content: "x",
	}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	msg2, _ := bus2.take()
	if _, ok := msg2.Metadata[WantsStreamMetaKey]; ok {
		t.Errorf("metadata = %v, want no %s", msg2.Metadata, WantsStreamMetaKey)
	}
}

func TestHandleMessageSessionKeyOverride(t *testing.T) {
	c, bus := newChannel(t, mapSection(map[string]any{"allow_from": []any{"alice"}}), newTestStore(t))

	key := "shared-session"
	if err := c.HandleMessage(context.Background(), InboundRequest{
		SenderID: "alice", ChatID: "c", Content: "x", SessionKey: &key, RequireExistingSession: true,
	}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	msg, _ := bus.take()
	if got := msg.SessionKey(); got != "shared-session" {
		t.Errorf("session key = %q, want shared-session", got)
	}
	if !msg.RequireExistingSess {
		t.Error("RequireExistingSession was not carried onto the message")
	}

	// An EMPTY override is falsy in Python, so the derived key wins.
	blank := ""
	if err := c.HandleMessage(context.Background(), InboundRequest{
		SenderID: "alice", ChatID: "c", Content: "x", SessionKey: &blank,
	}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	msg, _ = bus.take()
	if got := msg.SessionKey(); got != "test:c" {
		t.Errorf("session key with an empty override = %q, want the derived test:c", got)
	}
}

func TestHandleMessageErrors(t *testing.T) {
	// No bus configured: the failure is reported when a message is handled,
	// because a channel constructor cannot return an error.
	c := &testChannel{}
	c.Base = NewBase(c, mapSection(map[string]any{"allow_from": []any{"alice"}}), nil,
		WithName("test"), WithLogger(discardLogger()), WithPairingStore(newTestStore(t)))
	if err := c.HandleMessage(context.Background(), InboundRequest{SenderID: "alice", ChatID: "c"}); !errors.Is(err, ErrNoBus) {
		t.Errorf("HandleMessage without a bus = %v, want ErrNoBus", err)
	}

	// A Base built without its channel cannot reach Send.
	bare := &Base{}
	if err := bare.HandleMessage(context.Background(), InboundRequest{}); !errors.Is(err, ErrNoChannel) {
		t.Errorf("HandleMessage on a bare Base = %v, want ErrNoChannel", err)
	}

	// A Send failure must propagate so the manager can retry.
	c2, _ := newChannel(t, mapSection(map[string]any{"allow_from": []any{}}), newTestStore(t))
	c2.sendErr = errors.New("send failed")
	err := c2.HandleMessage(context.Background(), InboundRequest{
		SenderID: "stranger", ChatID: "dm", Content: "hi", IsDM: true,
	})
	if err == nil || err.Error() != "send failed" {
		t.Errorf("HandleMessage = %v, want the Send error", err)
	}

	// A bus failure must propagate too.
	c3, bus3 := newChannel(t, mapSection(map[string]any{"allow_from": []any{"alice"}}), newTestStore(t))
	bus3.err = errors.New("bus down")
	if err := c3.HandleMessage(context.Background(), InboundRequest{SenderID: "alice", ChatID: "c"}); err == nil {
		t.Error("HandleMessage swallowed a bus failure")
	}
}

func TestHandleMessageDropsDMWhenPairingStoreIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pairing.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	store := pairing.NewStore(path)
	store.SetLogger(discardLogger())

	c, bus := newChannel(t, mapSection(map[string]any{"allow_from": []any{}}), store)
	err := c.HandleMessage(context.Background(), InboundRequest{
		SenderID: "stranger", ChatID: "dm", Content: "hi", IsDM: true,
	})
	if err != nil {
		t.Errorf("HandleMessage = %v, want nil (the reference swallows the store failure)", err)
	}
	if len(c.sentMessages()) != 0 {
		t.Error("a pairing reply was sent from an unavailable store")
	}
	if bus.size() != 0 {
		t.Error("the message was published despite the store failure")
	}
}

// ---------------------------------------------------------------------------
// Reasoning bridge
// ---------------------------------------------------------------------------

func TestSendReasoningForwardsWholeBlock(t *testing.T) {
	bus := &recordingBus{}
	c := &reasoningChannel{testChannel: &testChannel{}}
	c.Base = NewBase(c, mapSection(nil), bus, WithName("test"), WithLogger(discardLogger()), WithPairingStore(newTestStore(t)))

	streamID := "stream-1"
	if err := c.SendReasoning(context.Background(), core.OutboundMessage{
		ChatID: "chat", Content: "thinking...", Event: events.ProgressEvent{StreamID: &streamID},
	}); err != nil {
		t.Fatalf("SendReasoning: %v", err)
	}
	if len(c.deltas) != 1 || c.deltas[0] != "thinking..." {
		t.Errorf("deltas = %v, want one delta carrying the whole block", c.deltas)
	}
	if c.ends != 1 {
		t.Errorf("ends = %d, want 1", c.ends)
	}
	if c.deltaID == nil || *c.deltaID != "stream-1" {
		t.Errorf("delta stream id = %v, want stream-1", c.deltaID)
	}

	// An empty content is a no-op: no delta, no end marker.
	c.deltas, c.ends = nil, 0
	if err := c.SendReasoning(context.Background(), core.OutboundMessage{ChatID: "chat"}); err != nil {
		t.Fatalf("SendReasoning: %v", err)
	}
	if len(c.deltas) != 0 || c.ends != 0 {
		t.Errorf("empty content produced deltas=%v ends=%d, want none", c.deltas, c.ends)
	}
}

func TestSendReasoningStopsAtAFailedDelta(t *testing.T) {
	c := &reasoningChannel{testChannel: &testChannel{}, err: errors.New("delta failed")}
	c.Base = NewBase(c, mapSection(nil), &recordingBus{}, WithLogger(discardLogger()), WithPairingStore(newTestStore(t)))

	err := c.SendReasoning(context.Background(), core.OutboundMessage{ChatID: "chat", Content: "text"})
	if err == nil || err.Error() != "delta failed" {
		t.Fatalf("SendReasoning = %v, want the delta error", err)
	}
	if c.ends != 0 {
		t.Error("the end marker was sent after a failed delta")
	}
}

func TestSendReasoningIsANoOpWithoutTheCapabilities(t *testing.T) {
	c, _ := newChannel(t, mapSection(nil), newTestStore(t))
	if err := c.SendReasoning(context.Background(), core.OutboundMessage{ChatID: "chat", Content: "text"}); err != nil {
		t.Errorf("SendReasoning on a plain channel = %v, want nil", err)
	}
}

// ---------------------------------------------------------------------------
// EventStreamID
// ---------------------------------------------------------------------------

func TestEventStreamID(t *testing.T) {
	streamID := "s-1"
	other := "s-2"

	for _, tc := range []struct {
		name  string
		event core.AgentEvent
		want  *string
	}{
		{"nil", nil, nil},
		{"progress", events.ProgressEvent{StreamID: &streamID}, &streamID},
		{"progress without id", events.ProgressEvent{}, nil},
		{"file edit", events.FileEditEvent{ProgressEvent: events.ProgressEvent{StreamID: &streamID}}, &streamID},
		{"stream delta", events.StreamDeltaEvent{StreamID: &streamID}, &streamID},
		{"stream end", events.StreamEndEvent{StreamID: &other}, &other},
		{"unrelated", events.ContextCompactionEvent{}, nil},
		// Pointer forms. EventName is defined on a VALUE receiver
		// (internal/events/outbound.go:74), so *StreamDeltaEvent satisfies
		// core.AgentEvent as well; a value-only type switch silently returned nil
		// for these and dropped the stream id, where Python's getattr() would
		// have found it.
		{"pointer stream delta", &events.StreamDeltaEvent{StreamID: &streamID}, &streamID},
		{"pointer stream end", &events.StreamEndEvent{StreamID: &other}, &other},
		{"pointer progress", &events.ProgressEvent{StreamID: &streamID}, &streamID},
		{"pointer progress without id", &events.ProgressEvent{}, nil},
		{"pointer unrelated", &events.ContextCompactionEvent{}, nil},
	} {
		got := EventStreamID(tc.event)
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("%s: EventStreamID = %q, want nil", tc.name, *got)
		case tc.want != nil && got == nil:
			t.Errorf("%s: EventStreamID = nil, want %q", tc.name, *tc.want)
		case tc.want != nil && got != nil && *got != *tc.want:
			t.Errorf("%s: EventStreamID = %q, want %q", tc.name, *got, *tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Section
// ---------------------------------------------------------------------------

func TestSectionAccessors(t *testing.T) {
	raw := map[string]any{"allow_from": []any{"alice"}, "streaming": true}
	m := NewMapSection(raw)
	if !m.IsMap() {
		t.Error("NewMapSection produced a non-map section")
	}
	got, ok := m.Map()
	if !ok || len(got) != 2 {
		t.Errorf("Map() = (%v, %v)", got, ok)
	}
	if _, ok := m.Object(); ok {
		t.Error("a map section also reported an object section")
	}

	o := NewObjectSection(ObjectSection{AllowFrom: []any{"bob"}, Streaming: "yes"})
	if o.IsMap() {
		t.Error("an object section reported itself as a map")
	}
	if _, ok := o.Map(); ok {
		t.Error("Map() succeeded on an object section")
	}
	obj, ok := o.Object()
	if !ok || obj.Streaming != "yes" {
		t.Errorf("Object() = (%+v, %v)", obj, ok)
	}

	// A nil map is still a map section: an explicitly empty config behaves the
	// same as a missing one, but the distinction must not panic.
	if !NewMapSection(nil).IsMap() {
		t.Error("NewMapSection(nil) is not a map section")
	}
}

func TestSectionCamelCaseAlias(t *testing.T) {
	// Python's base reads `allow_from`, and a dict config uses the snake_case
	// key; the camelCase spelling is accepted only as an alias.
	snake := NewMapSection(map[string]any{"allow_from": []any{"alice"}})
	if v := snake.allowValue(); v == nil {
		t.Error("allow_from was not read")
	}
	camel := NewMapSection(map[string]any{"allowFrom": []any{"alice"}})
	if v := camel.allowValue(); v == nil {
		t.Error("allowFrom alias was not read")
	}
	// The object path accepts BOTH spellings (getattr with a fallback).
	if v := NewObjectSection(ObjectSection{AllowFrom: []any{"alice"}}).allowValue(); v == nil {
		t.Error("ObjectSection.AllowFrom was not read")
	}
	// streaming is read from the snake_case key only.
	if v := NewMapSection(map[string]any{"streaming": true}).streamingValue(); v != true {
		t.Errorf("streaming = %v, want true", v)
	}
	if v := NewMapSection(map[string]any{"streamingEnabled": true}).streamingValue(); v != nil {
		t.Errorf("streamingEnabled = %v, want nil", v)
	}
	if v := NewObjectSection(ObjectSection{Streaming: true}).streamingValue(); v != true {
		t.Errorf("ObjectSection.Streaming = %v, want true", v)
	}
}

func TestContainsTokenSemantics(t *testing.T) {
	// A string allowlist is a SUBSTRING test in Python, not a list of tokens.
	if !containsToken("alice,bob", "lic") {
		t.Error(`containsToken("alice,bob", "lic") = false; a string allowlist is a substring test`)
	}
	if containsToken("alice,bob", "carol") {
		t.Error("a substring allowlist matched an absent token")
	}
	// A list requires an exact element match, and a non-string element never
	// matches a string token.
	if !containsToken([]any{"alice", "bob"}, "bob") {
		t.Error("a list allowlist did not match a member")
	}
	if containsToken([]any{123, true, nil}, "123") {
		t.Error(`a non-string element matched the token "123"; Python's "123" == 123 is False`)
	}
	// A dict is a KEY membership test.
	if !containsToken(map[string]any{"alice": 1}, "alice") {
		t.Error("a dict allowlist did not match a key")
	}
	if containsToken(map[string]any{"alice": 1}, "1") {
		t.Error("a dict allowlist matched a value instead of a key")
	}
	// A nil or absent allowlist matches nothing.
	if containsToken(nil, "") || containsToken(nil, "*") {
		t.Error("a nil allowlist matched a token")
	}
}

func TestPyTruthy(t *testing.T) {
	for _, v := range []any{nil, false, 0, 0.0, "", []any{}, map[string]any{}} {
		if pyTruthy(v) {
			t.Errorf("pyTruthy(%#v) = true, want false", v)
		}
	}
	for _, v := range []any{true, 1, -1, 0.5, "0", "false", []any{0}, map[string]any{"k": nil}} {
		if !pyTruthy(v) {
			t.Errorf("pyTruthy(%#v) = false, want true", v)
		}
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

// TestRunningFlagIsRaceFree is meaningful under -race: a plain bool would be
// flagged when the manager reads the flag while the channel's Start writes it.
func TestRunningFlagIsRaceFree(t *testing.T) {
	c, _ := newChannel(t, mapSection(nil), newTestStore(t))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.SetRunning(i%2 == 0)
				_ = c.IsRunning()
			}
		}(i)
	}
	wg.Wait()
}

func TestConcurrentHandleMessage(t *testing.T) {
	store := newTestStore(t)
	c, bus := newChannel(t, mapSection(map[string]any{"allow_from": []any{"alice"}}), store)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.HandleMessage(context.Background(), InboundRequest{
				SenderID: "alice", ChatID: "c", Content: "x",
			}); err != nil {
				t.Errorf("HandleMessage: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := bus.size(); got != 16 {
		t.Errorf("published %d messages, want 16", got)
	}
}
