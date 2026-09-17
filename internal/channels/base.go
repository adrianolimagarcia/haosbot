// Package channels ports the nanobot channel abstraction:
// nanobot/channels/base.py at commit 1bb712d3 (v0.3.5).
//
// SCOPE — what is here and what is deliberately not.
//
// Ported: the Channel interface (Python's three abstractmethods), the optional
// hook surface, is_allowed, _handle_message, supports_streaming, the
// send_reasoning bridge, and the per-instance delivery flags.
//
// NOT ported, because they belong to other tasks: ChannelManager (P0.5), the
// pending queue and injection callbacks (P0.3), the plugin manifest/registry
// (P0.7), and every concrete channel. BaseChannel is self-contained apart from
// the pairing store, which it needs for is_allowed and the DM pairing reply.
//
// DESIGN — why an interface plus an embedded struct, not a function-valued
// struct.
//
// Python's BaseChannel is an ABC whose subclasses inherit five concrete helpers
// and may override eight optional hooks; two of those hooks (`send_delta`, and
// the reasoning pair) have a no-op default that the class itself must be able to
// DETECT, because `supports_streaming` is defined as
// `type(self).send_delta is not BaseChannel.send_delta` (base.py:235). Go's
// method promotion is static and cannot ask "did the outer type override this?"
// — but an interface assertion can, exactly:
//
//	_, ok := self.(DeltaSender)
//
// So the split is forced by the reference's own semantics:
//
//   - hooks whose default is a real value (`login`, `should_retry_send_error`,
//     `start_error_message`, `progress_transport_defaults`, `default_config`,
//     `refresh_feature_metadata`) are methods on Base. A channel that shadows
//     the method wins the interface assertion, so the behaviour matches Python
//     without any registration step;
//   - hooks whose default is a no-op that must stay DETECTABLE (`send_delta`,
//     `send_reasoning_delta`, `send_reasoning_end`, `send_file_edit_events`)
//     are capability interfaces that Base must NOT implement. A channel opts in
//     by implementing the method.
//
// Base therefore holds a back-reference to its outer channel, set once by
// NewBase. That reference is what lets HandleMessage call the concrete Send
// (as Python's `await self.send(...)` does), lets SupportsStreaming see the
// concrete method set, and lets SendReasoning reach the concrete streaming
// pair. The alternative — passing the channel as an extra argument to every
// helper — pushes the same knowledge into every call site and makes it possible
// to pass the wrong channel.
package channels

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/events"
	"github.com/adrianolimagarcia/nanobot-go/internal/pairing"
)

// WantsStreamMetaKey is the inbound metadata key Base sets when the channel
// supports streaming. Upstream hardcodes the string at base.py:308; naming it
// here keeps the consumer (the agent loop) from repeating a literal.
const WantsStreamMetaKey = "_wants_stream"

// DefaultName and DefaultDisplayName are the class attributes of BaseChannel
// (base.py:29-30).
const (
	DefaultName        = "base"
	DefaultDisplayName = "Base"
)

// InboundPublisher is the bus surface a channel needs.
//
// *bus.Bus satisfies it. The interface exists so a channel cannot reach the
// outbound queue or the local-subscriber registry, and so tests can capture
// published messages without constructing a bus.
type InboundPublisher interface {
	PublishInbound(ctx context.Context, msg core.InboundMessage) error
}

// Channel is the interface every channel implements.
//
// Start, Stop and Send are Python's three @abstractmethods (base.py:74-102). A
// concrete channel must define all three to be usable, which reproduces the
// ABC's instantiation guard at compile time. Name and DisplayName are provided
// by Base and describe the runtime identity; Python mutates `channel.name` for
// multi-instance channels (manager.py:218), which maps to SetName.
type Channel interface {
	// Name is the runtime identity. It must equal the plugin name unless the
	// channel is a multi-instance runtime (plugin.py:90-93, manager.py:217-218).
	Name() string
	// DisplayName is the human-facing label.
	DisplayName() string
	// Start connects to the platform and listens. It must not return until the
	// channel is stopped.
	Start(ctx context.Context) error
	// Stop stops the channel and releases its resources.
	Stop(ctx context.Context) error
	// Send delivers one outbound message.
	//
	// It MUST return an error on delivery failure so the manager can apply its
	// single retry policy (base.py:99-101); swallowing a failure here silently
	// disables retries for the whole channel.
	Send(ctx context.Context, msg core.OutboundMessage) error
}

// ---------------------------------------------------------------------------
// Optional capability interfaces
//
// Base deliberately does NOT implement any method in this group: their presence
// IS the capability test.
// ---------------------------------------------------------------------------

// DeltaOptions are the keyword-only parameters of send_delta (base.py:129-139).
type DeltaOptions struct {
	// StreamID identifies the stream a stateful channel must key its buffer by.
	StreamID *string
	// StreamEnd marks the final chunk of a segment.
	StreamEnd bool
	// Resuming marks a segment that continues a previously interrupted stream.
	Resuming bool
	// MergeNext marks a resumable provider boundary whose next text segment
	// belongs to the same user-visible message.
	MergeNext bool
}

// DeltaSender is implemented by channels that stream assistant text.
//
// Python: overriding `send_delta`. The manager probes the method's signature for
// a `merge_next` parameter before passing it (manager.py:888-899); in Go the
// option set is fixed, so every DeltaSender receives MergeNext and the probe
// has no equivalent. That is a deliberate divergence: a Go channel cannot be an
// older plugin that predates the parameter.
type DeltaSender interface {
	SendDelta(ctx context.Context, chatID, delta string, metadata map[string]any, opts DeltaOptions) error
}

// ReasoningDeltaSender is implemented by channels that stream model reasoning.
// Python: overriding `send_reasoning_delta` (base.py:153).
type ReasoningDeltaSender interface {
	SendReasoningDelta(ctx context.Context, chatID, delta string, metadata map[string]any, streamID *string) error
}

// ReasoningEndSender is implemented by channels that flush a buffered reasoning
// group. Python: overriding `send_reasoning_end` (base.py:173).
type ReasoningEndSender interface {
	SendReasoningEnd(ctx context.Context, chatID string, metadata map[string]any, streamID *string) error
}

// FileEditSender is implemented by channels with a rich activity surface.
// Python: overriding `send_file_edit_events` (base.py:188).
type FileEditSender interface {
	SendFileEditEvents(ctx context.Context, chatID string, edits []map[string]any, metadata map[string]any) error
}

// ---------------------------------------------------------------------------
// Interfaces Base always satisfies
//
// Each of these has a real default in Python, so a channel that overrides the
// corresponding method shadows Base's and the assertion returns the override.
// They are named so the manager can ask for one capability at a time instead of
// requiring the whole surface.
// ---------------------------------------------------------------------------

// DeliveryPolicy is the per-instance progress policy.
//
// These three booleans are NOT class constants at dispatch time: the manager
// overwrites them on each instance at construction (manager.py:219-231) and
// reads the instance value later (manager.py:353, :799, :804-811).
type DeliveryPolicy interface {
	SendProgress() bool
	SetSendProgress(v bool)
	SendToolHints() bool
	SetSendToolHints(v bool)
	ShowReasoning() bool
	SetShowReasoning(v bool)
}

// ProgressTransportDefaults mirrors progress_transport_defaults (base.py:104).
// ok is false when the channel keeps the global policy (Python returns None).
type ProgressTransportDefaults interface {
	ProgressTransportDefaults() (progress, toolHints bool, ok bool)
}

// SendErrorPolicy mirrors should_retry_send_error (base.py:112).
type SendErrorPolicy interface {
	ShouldRetrySendError(err error) bool
}

// StartErrorMessenger mirrors start_error_message (base.py:121). An empty
// string means "no channel-specific message", which is how Python's None
// behaves at the only call site, `public_error or <generic>` (manager.py:385).
type StartErrorMessenger interface {
	StartErrorMessage(err error) string
}

// LoginProvider mirrors login (base.py:62).
type LoginProvider interface {
	Login(ctx context.Context, force bool) (bool, error)
}

// ConfigDefaulter mirrors default_config (base.py:323).
type ConfigDefaulter interface {
	DefaultConfig() map[string]any
}

// FeatureRefresher mirrors refresh_feature_metadata (base.py:328).
type FeatureRefresher interface {
	RefreshFeatureMetadata(configPath string, instanceID string) bool
}

// RunningState mirrors is_running (base.py:338).
type RunningState interface {
	IsRunning() bool
}

// ConfigProvider exposes the channel configuration section.
type ConfigProvider interface {
	Config() Section
}

// ---------------------------------------------------------------------------
// Base
// ---------------------------------------------------------------------------

// Base is the shared implementation every channel embeds. It is the Go name for
// Python's BaseChannel.
//
// Construction:
//
//	type Telegram struct{ *channels.Base }
//
//	func New(cfg channels.Section, bus channels.InboundPublisher) *Telegram {
//	    t := &Telegram{}
//	    t.Base = channels.NewBase(t, cfg, bus, channels.WithName("telegram"))
//	    return t
//	}
//
// The channel must pass itself: Base uses that reference to reach the concrete
// Send and to detect the streaming capabilities. Nothing else needs registering.
type Base struct {
	self Channel

	name        string
	displayName string
	config      Section
	bus         InboundPublisher
	store       *pairing.Store
	logger      *slog.Logger

	sendProgress  bool
	sendToolHints bool
	showReasoning bool

	// running is written by the channel's own Start/Stop and read by the
	// manager. Python's plain bool is safe under the GIL; Go needs the
	// synchronization, and a channel that forgets it would be a data race the
	// race detector reports rather than a silently stale status.
	running atomic.Bool
}

// Option configures a Base at construction.
type Option func(*Base)

// WithName sets the runtime identity (Python: `name`, base.py:29).
func WithName(name string) Option { return func(b *Base) { b.name = name } }

// WithDisplayName sets the human-facing label (Python: `display_name`).
func WithDisplayName(name string) Option { return func(b *Base) { b.displayName = name } }

// WithPairingStore overrides the pairing store. The default is the process-wide
// store (pairing.Default()), which reads ~/.nanobot/pairing.json.
func WithPairingStore(store *pairing.Store) Option {
	return func(b *Base) { b.store = store }
}

// WithLogger overrides the diagnostic logger. The default is slog.Default().
func WithLogger(logger *slog.Logger) Option { return func(b *Base) { b.logger = logger } }

// WithDeliveryPolicy sets the initial progress/reasoning flags. Python's class
// defaults are all true (base.py:31-33).
func WithDeliveryPolicy(sendProgress, sendToolHints, showReasoning bool) Option {
	return func(b *Base) {
		b.sendProgress = sendProgress
		b.sendToolHints = sendToolHints
		b.showReasoning = showReasoning
	}
}

// NewBase builds the shared state for a channel.
//
// self must be the channel being constructed. config and bus are required;
// passing a nil bus is reported when a message is handled rather than at
// construction, because a channel's constructor cannot return an error.
func NewBase(self Channel, config Section, bus InboundPublisher, opts ...Option) *Base {
	b := &Base{
		self:          self,
		name:          DefaultName,
		displayName:   DefaultDisplayName,
		config:        config,
		bus:           bus,
		sendProgress:  true,
		sendToolHints: true,
		showReasoning: true,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(b)
		}
	}
	return b
}

// ---------------------------------------------------------------------------
// Identity and configuration
// ---------------------------------------------------------------------------

// Name is the runtime identity (Python: `name`).
func (b *Base) Name() string { return b.name }

// SetName renames the runtime. The manager does this for multi-instance
// channels (manager.py:218).
func (b *Base) SetName(name string) { b.name = name }

// DisplayName is the human-facing label (Python: `display_name`).
func (b *Base) DisplayName() string { return b.displayName }

// Config returns the configuration section.
func (b *Base) Config() Section { return b.config }

// Bus returns the publisher this channel was constructed with.
func (b *Base) Bus() InboundPublisher { return b.bus }

// Logger returns the diagnostic logger in use.
func (b *Base) Logger() *slog.Logger { return b.loggerOf() }

// PairingStore returns the pairing store in use.
func (b *Base) PairingStore() *pairing.Store { return b.storeOf() }

// SetPairingStore replaces the pairing store.
func (b *Base) SetPairingStore(store *pairing.Store) { b.store = store }

func (b *Base) loggerOf() *slog.Logger {
	if b.logger != nil {
		return b.logger
	}
	return slog.Default()
}

func (b *Base) storeOf() *pairing.Store {
	if b.store != nil {
		return b.store
	}
	return pairing.Default()
}

// ---------------------------------------------------------------------------
// Delivery policy (Python: send_progress / send_tool_hints / show_reasoning)
// ---------------------------------------------------------------------------

// SendProgress reports whether progress events may be delivered.
func (b *Base) SendProgress() bool { return b.sendProgress }

// SetSendProgress overwrites the progress policy for this instance.
func (b *Base) SetSendProgress(v bool) { b.sendProgress = v }

// SendToolHints reports whether tool-hint progress may be delivered.
func (b *Base) SendToolHints() bool { return b.sendToolHints }

// SetSendToolHints overwrites the tool-hint policy for this instance.
func (b *Base) SetSendToolHints(v bool) { b.sendToolHints = v }

// ShowReasoning reports whether reasoning events may be delivered.
func (b *Base) ShowReasoning() bool { return b.showReasoning }

// SetShowReasoning overwrites the reasoning policy for this instance.
func (b *Base) SetShowReasoning(v bool) { b.showReasoning = v }

// IsRunning reports whether the channel is running (base.py:338-341).
func (b *Base) IsRunning() bool { return b.running.Load() }

// SetRunning records the running state. Python's channels assign `self._running`
// directly in start/stop; there is no base-class setter upstream.
func (b *Base) SetRunning(v bool) { b.running.Store(v) }

// ---------------------------------------------------------------------------
// Optional hooks with real defaults
// ---------------------------------------------------------------------------

// ProgressTransportDefaults returns channel-owned defaults for progress and
// tool-hint messages. Port of progress_transport_defaults (base.py:104-110).
//
// ok is false when the channel keeps the global policy.
func (b *Base) ProgressTransportDefaults() (progress, toolHints bool, ok bool) {
	return false, false, false
}

// ShouldRetrySendError reports whether the manager may retry a failed delivery.
// Port of should_retry_send_error (base.py:112-119).
func (b *Base) ShouldRetrySendError(err error) bool { return true }

// StartErrorMessage returns an actionable public message for a startup failure.
// Port of start_error_message (base.py:121-127). An empty string keeps the
// manager's generic fallback.
func (b *Base) StartErrorMessage(err error) string { return "" }

// Login performs channel-specific interactive login. Port of login
// (base.py:62-72).
func (b *Base) Login(ctx context.Context, force bool) (bool, error) { return true, nil }

// DefaultConfig returns the default config for onboard. Port of default_config
// (base.py:323-326).
func (b *Base) DefaultConfig() map[string]any { return map[string]any{"enabled": false} }

// RefreshFeatureMetadata refreshes persisted display metadata after a settings
// action. Port of refresh_feature_metadata (base.py:328-336).
func (b *Base) RefreshFeatureMetadata(configPath string, instanceID string) bool { return false }

// TranscribeAudio transcribes an audio file.
//
// GAP (reported, not stubbed silently): the reference delegates to
// nanobot.audio.transcription, which is NOT ported — there is no audio package
// in this port. The reference returns "" on ANY failure (base.py:58-60), so an
// always-empty result is the same observable behaviour a deployment without
// transcription configured would see, but a channel that actually needs
// transcription must implement this method itself. No caller in this port
// invokes it yet.
func (b *Base) TranscribeAudio(ctx context.Context, filePath string) string {
	b.loggerOf().Debug("Audio transcription is not ported; returning empty", "file_path", filePath)
	return ""
}

// ---------------------------------------------------------------------------
// Authorization
// ---------------------------------------------------------------------------

// IsAllowed checks sender permission: star > allowlist > pairing store > deny.
// Port of is_allowed (base.py:237-253).
//
// The allowlist is matched EXACTLY, character for character: no trimming, no
// case folding, no splitting on separators. That is not an accident of the
// reference — it is the difference between "attacker|allow@email.com" being
// denied and being authorized (test_base_channel.py:28-32), so this port does
// not "helpfully" normalize either side.
func (b *Base) IsAllowed(senderID string) bool {
	allowList := b.config.allowValue()
	if containsToken(allowList, "*") {
		return true
	}
	if containsToken(allowList, senderID) {
		return true
	}
	return b.storeOf().IsApproved(b.name, senderID)
}

// SupportsStreaming reports whether config enables streaming AND this channel
// implements SendDelta. Port of supports_streaming (base.py:225-235).
//
// `bool(streaming)` is a truthiness test, not an isinstance test: a config value
// of "yes" enables streaming. The second half is the capability probe that Go
// expresses as an interface assertion.
func (b *Base) SupportsStreaming() bool {
	if !pyTruthy(b.config.streamingValue()) {
		return false
	}
	_, ok := b.self.(DeltaSender)
	return ok
}

// ---------------------------------------------------------------------------
// Reasoning bridge
// ---------------------------------------------------------------------------

// SendReasoning delivers a complete reasoning block. Port of send_reasoning
// (base.py:201-223).
//
// The default implementation reuses the streaming pair so a channel only has to
// implement the delta/end methods: one delta carrying the whole content
// followed immediately by an end marker. If the channel implements neither, this
// is a no-op, which is what two no-op base methods do in Python.
//
// An empty content returns without calling anything, and a delta failure
// prevents the end marker — both matching the reference's control flow.
func (b *Base) SendReasoning(ctx context.Context, msg core.OutboundMessage) error {
	if msg.Content == "" {
		return nil
	}
	streamID := EventStreamID(msg.Event)
	if sender, ok := b.self.(ReasoningDeltaSender); ok {
		if err := sender.SendReasoningDelta(ctx, msg.ChatID, msg.Content, msg.Metadata, streamID); err != nil {
			return err
		}
	}
	if ender, ok := b.self.(ReasoningEndSender); ok {
		return ender.SendReasoningEnd(ctx, msg.ChatID, msg.Metadata, streamID)
	}
	return nil
}

// EventStreamID extracts the stream id carried by an event.
//
// Python uses getattr(msg.event, "stream_id", None), which is true for any event
// that happens to carry the attribute — value or reference alike. In this port
// exactly three event types do (ProgressEvent, StreamDeltaEvent, StreamEndEvent);
// ProgressOf is used rather than a type assertion so the FileEditEvent subclass is
// covered.
//
// Both the value and the pointer form are matched. EventName is defined on a
// VALUE receiver (internal/events/outbound.go:74), so *StreamDeltaEvent satisfies
// core.AgentEvent too; a value-only type switch silently returned nil for it and
// dropped the stream id. Nothing constructs a pointer event today, so the gap was
// latent rather than live — but it was real enough that SendOnce grew a
// normalisation workaround for it, and this is the correct place to close it.
func EventStreamID(event core.AgentEvent) *string {
	if event == nil {
		return nil
	}
	if progress, ok := events.ProgressOf(event); ok {
		return progress.StreamID
	}
	switch e := event.(type) {
	case events.StreamDeltaEvent:
		return e.StreamID
	case *events.StreamDeltaEvent:
		return e.StreamID
	case events.StreamEndEvent:
		return e.StreamID
	case *events.StreamEndEvent:
		return e.StreamID
	}
	return nil
}

// ---------------------------------------------------------------------------
// Inbound handling
// ---------------------------------------------------------------------------

// InboundRequest is the argument set of BaseChannel._handle_message
// (base.py:255-266).
//
// SenderID and ChatID are the identities recorded on the inbound message;
// AuthorizationID, when set, is the identity the permission check runs against
// without changing what is recorded.
type InboundRequest struct {
	// SenderID is the member who sent the message.
	SenderID string
	// ChatID is the conversation the message belongs to.
	ChatID string
	// Content is the message text.
	Content string
	// Media holds media URLs or local paths.
	Media []string
	// Metadata carries channel-specific context.
	Metadata map[string]any
	// SessionKey overrides the derived session key when non-nil.
	SessionKey *string
	// IsDM marks a direct message, which is what makes an unapproved sender
	// receive a pairing code instead of silence.
	IsDM bool
	// AuthorizationID is the entity access is scoped to (a group or room) when
	// it differs from the sender. nil means sender-based authorization; an
	// explicit empty string authorizes against "" and therefore denies.
	AuthorizationID *string
	// RequireExistingSession drops the message unless a session already exists.
	RequireExistingSession bool
}

// ErrNoBus is returned when a channel was constructed without a message bus.
var ErrNoBus = errors.New("channels: no bus configured")

// ErrNoChannel is returned when Base was constructed without its channel.
var ErrNoChannel = errors.New("channels: Base has no channel reference (use NewBase)")

// HandleMessage is the universal inbound contract: every channel funnels its
// traffic through it. Port of _handle_message (base.py:255-321).
//
//  1. Authorize against AuthorizationID when set, else SenderID.
//  2. On denial: a DM receives a pairing code (or is dropped when the pairing
//     store is unavailable), a non-DM is dropped. Nothing is published.
//  3. On success: stamp _wants_stream when the channel supports streaming,
//     build the InboundMessage and publish it.
//
// IsDM and AuthorizationID are consumed here and never copied onto the
// InboundMessage.
func (b *Base) HandleMessage(ctx context.Context, req InboundRequest) error {
	if b.self == nil {
		return ErrNoChannel
	}

	permissionID := req.SenderID
	if req.AuthorizationID != nil {
		permissionID = *req.AuthorizationID
	}

	if !b.IsAllowed(permissionID) {
		if !req.IsDM {
			b.loggerOf().Warn(
				"Access denied for sender. Add them to allowFrom list in config to grant access.",
				"channel", b.name, "sender_id", req.SenderID)
			return nil
		}

		code, err := b.storeOf().GenerateCode(b.name, req.SenderID, pairing.DefaultTTLSeconds)
		if err != nil {
			if pairing.IsStoreIOError(err) {
				// Transient pairing-store I/O failure: skip the pairing reply
				// for this message rather than crash the handler. The store is
				// left untouched, so previously approved senders survive.
				b.loggerOf().Warn("Pairing store unavailable; dropping DM",
					"channel", b.name, "sender_id", req.SenderID, "error", err)
				return nil
			}
			return err
		}

		reply := core.OutboundMessage{
			Channel:  b.name,
			ChatID:   req.ChatID,
			Content:  pairing.FormatPairingReply(code),
			Media:    []string{},
			Buttons:  [][]string{},
			Metadata: map[string]any{pairing.PairingCodeMetaKey: code},
		}
		if err := b.self.Send(ctx, reply); err != nil {
			return err
		}
		b.loggerOf().Info("Sent pairing code", "channel", b.name,
			"sender_id", req.SenderID, "chat_id", req.ChatID)
		return nil
	}

	// Python: meta = metadata or {} — an EMPTY metadata dict is falsy, so it is
	// replaced by a fresh dict, while a non-empty one is shared with the caller.
	// The aliasing is observable (a later mutation of the caller's map reaches
	// the published message), so it is reproduced rather than "fixed".
	meta := req.Metadata
	if len(meta) == 0 {
		meta = map[string]any{}
	}
	if b.SupportsStreaming() {
		// Python: meta = {**meta, "_wants_stream": True} — always a copy, so
		// enabling streaming also removes the aliasing above.
		streamed := make(map[string]any, len(meta)+1)
		for k, v := range meta {
			streamed[k] = v
		}
		streamed[WantsStreamMetaKey] = true
		meta = streamed
	}

	// Python: media or [] — an empty list is falsy, so the field is always a
	// list, never null, in the JSON that reaches the agent.
	media := req.Media
	if len(media) == 0 {
		media = []string{}
	}

	if b.bus == nil {
		return ErrNoBus
	}
	msg := core.InboundMessage{
		Channel:             b.name,
		SenderID:            req.SenderID,
		ChatID:              req.ChatID,
		Content:             req.Content,
		Timestamp:           time.Now(),
		Media:               media,
		Metadata:            meta,
		SessionKeyOverride:  req.SessionKey,
		RequireExistingSess: req.RequireExistingSession,
	}
	return b.bus.PublishInbound(ctx, msg)
}
