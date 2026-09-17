// ChannelManager: lifecycle and outbound dispatch for the channel subsystem.
//
// Port of nanobot/channels/manager.py:82 (ChannelManager) at upstream 1bb712d3.
// This file holds P0.6 (lifecycle + status); manager_outbound.go holds P0.5
// (the outbound path). The Python source is 1101 lines; the parts that belong
// to other tasks are listed here rather than silently omitted.
//
// SCOPE — deliberately absent, with the reason:
//
//   - Channel DISCOVERY and construction: `_init_channels` (manager.py:246-311),
//     `_build_channel` (:183-245), `_channel_section` (:152-182),
//     `_validate_allow_from` (:328-345), `_mark_channel_error` (:313-322),
//     `_mark_runtime_error` (:324-326). The manager receives channel values;
//     discovery is P0.7 (contracts.py + plugin.py + registry.py). The error
//     record those methods write is exposed here as SetChannelError.
//   - Hot reload: `apply_channel_feature_action` (manager.py:429-596).
//   - The restart notice: `_notify_restart_done_if_needed` (:618-623) and
//     `_send_restart_notice_when_started` (:625-666). They depend on
//     nanobot.utils.restart, which is not ported. The `deadline` arm of
//     sendWithRetry IS ported because it is part of _send_with_retry's contract
//     (:1032-1036, :1044-1045), but it currently has no caller in this port.
//   - The WebUI constructor surface (`webui_*` callbacks, manager.py:100-135)
//     and `_default_webui_dist` (:49-56).
//   - `_resolve_bool_override` (:355-371) and `_BOOL_CAMEL_ALIASES` (:67-71),
//     which are only used by `_build_channel`.
//
// CONCURRENCY — the reference is single-threaded asyncio; this port is not, so
// every piece of shared state below is mutex-guarded and no lock is ever held
// across a call into channel code (which is arbitrary user code). Where the
// Python relies on the event loop's serialization, the Go equivalent is an
// explicit snapshot taken under the lock and acted on outside it.
package channels

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// ---------------------------------------------------------------------------
// Constants (manager.py:59-65)
// ---------------------------------------------------------------------------

const (
	// OutboundConcurrency is the maximum number of sends in flight at once.
	// Mirrors _OUTBOUND_CONCURRENCY (manager.py:61).
	OutboundConcurrency = 32
	// OutboundPendingLimit bounds queued-but-not-yet-sending outbound messages.
	// Mirrors _OUTBOUND_PENDING_LIMIT (manager.py:62).
	OutboundPendingLimit = 256
	// OriginReplyFingerprintsMaxSize bounds the duplicate-suppression memory.
	// Mirrors ORIGIN_REPLY_FINGERPRINTS_MAX_SIZE (manager.py:65).
	OriginReplyFingerprintsMaxSize = 1000
)

// genericStartError is the fallback recorded when a channel fails to start and
// has no channel-specific message. Mirrors the literal at manager.py:385.
const genericStartError = "Channel failed to start. Check gateway logs."

// Runtime states reported by GetStatus. These are the string values at
// manager.py:1079-1086.
const (
	// ChannelStateFailed means the channel recorded a startup error.
	ChannelStateFailed = "failed"
	// ChannelStateRunning means the channel reports itself running.
	ChannelStateRunning = "running"
	// ChannelStateStarting means the start task has not finished yet.
	ChannelStateStarting = "starting"
	// ChannelStateStopped means there is no start task and the channel is idle.
	ChannelStateStopped = "stopped"
)

// websocketChannelName is the channel name the dispatcher special-cases
// (manager.py:816-821) and the restart notice skips (manager.py:639-644).
const websocketChannelName = "websocket"

// outboundPollInterval is the bus poll timeout. Mirrors the `timeout=1.0` at
// manager.py:783. It is a housekeeping tick, not a failure: an expired poll
// continues the loop (manager.py:847-848).
const outboundPollInterval = time.Second

// ---------------------------------------------------------------------------
// Bus surface
// ---------------------------------------------------------------------------

// OutboundBus is the bus surface the manager needs.
//
// *bus.Bus satisfies it. The interface is deliberately narrower than *bus.Bus
// so the manager cannot publish inbound messages or reach the local-subscriber
// registry, and so a test can drive the dispatcher without a real bus.
//
// The reference reaches into `self.bus.outbound` directly for the non-blocking
// read coalescing needs (manager.py:959); ConsumeOutbound is
// `bus.consume_outbound()` and TryConsumeOutbound is `outbound.get_nowait()`.
type OutboundBus interface {
	// ConsumeOutbound blocks until a message is available or ctx is done.
	ConsumeOutbound(ctx context.Context) (core.OutboundMessage, error)
	// TryConsumeOutbound takes the next message without blocking.
	TryConsumeOutbound() (core.OutboundMessage, bool)
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// ChannelStatus is one entry of get_status (manager.py:1087-1095).
//
// The field order is the reference's dict insertion order — enabled, running,
// state, owner, instance_id, then error only when there is one — and is
// preserved by encoding/json so a serialized status is comparable byte for
// byte with the Python one.
type ChannelStatus struct {
	Enabled    bool   `json:"enabled"`
	Running    bool   `json:"running"`
	State      string `json:"state"`
	Owner      string `json:"owner"`
	InstanceID string `json:"instance_id"`
	// Error is present only when the channel recorded a startup failure. The
	// reference keys the presence of this field on the TRUTHINESS of the stored
	// error string, so an empty error is the same as no error (manager.py:1094).
	Error string `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Options
// ---------------------------------------------------------------------------

// ManagerOption configures a ChannelManager.
type ManagerOption func(*ChannelManager)

// WithManagerLogger overrides the diagnostic logger.
func WithManagerLogger(logger *slog.Logger) ManagerOption {
	return func(m *ChannelManager) {
		if logger != nil {
			m.logger = logger
		}
	}
}

// WithOutboundLimits overrides the admission and concurrency limits.
//
// Python's concurrency tests monkeypatch the module constants
// _OUTBOUND_PENDING_LIMIT and _OUTBOUND_CONCURRENCY before constructing the
// manager (tests/channels/test_channel_manager_concurrency.py:90-91), because
// the semaphores are sized from them at construction. A Go constant cannot be
// patched, so the same override is an explicit constructor option. A value <= 0
// keeps the default.
func WithOutboundLimits(pending, concurrency int) ManagerOption {
	return func(m *ChannelManager) {
		if pending > 0 {
			m.pendingLimit = pending
		}
		if concurrency > 0 {
			m.sendLimit = concurrency
		}
	}
}

// WithSendRetryDelays overrides the send-retry backoff schedule.
//
// The Go analogue of monkeypatching _SEND_RETRY_DELAYS (manager.py:60), which
// tests/channels/test_channel_manager_concurrency.py:59 does. An empty or nil
// slice keeps the default. The index-clamping rule is unchanged: attempt n uses
// index min(n-1, len-1), and a trailing delay repeats (manager.py:1043).
func WithSendRetryDelays(delays []time.Duration) ManagerOption {
	return func(m *ChannelManager) {
		if len(delays) == 0 {
			return
		}
		nanos := make([]int64, len(delays))
		for i, d := range delays {
			nanos[i] = int64(d)
		}
		m.retryDelays = nanos
	}
}

// ---------------------------------------------------------------------------
// ChannelManager
// ---------------------------------------------------------------------------

// channelSpec is one entry of `_channel_runtime_specs`: the plugin that owns a
// runtime plus the instance id within it (manager.py:138, contracts.py:544-554).
type channelSpec struct {
	owner      string
	instanceID string
}

// channelTask is one `_channel_tasks` entry: Python's asyncio.Task plus the
// handle needed to cancel it.
//
// Python's `task.cancel()` asks the loop to inject CancelledError at the next
// await point; `done` closed means the start coroutine has finished, which is
// what `task.done()` reports.
type channelTask struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// finished reports whether the start task has completed (Python: task.done()).
func (t *channelTask) finished() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// ChannelManager coordinates channels: it starts and stops them, reports their
// runtime state, and dispatches outbound messages to them.
type ChannelManager struct {
	config *config.Config
	bus    OutboundBus
	logger *slog.Logger

	pendingLimit int
	sendLimit    int
	retryDelays  []int64 // nanoseconds

	// mu guards every field below. It is never held across a call into channel
	// code or across a blocking operation.
	mu        sync.Mutex
	channels  map[string]Channel
	order     []string // channel insertion order, mirroring Python's dict order
	owners    map[string]string
	specs     map[string]channelSpec
	specOrder []string
	errs      map[string]string
	tasks     map[string]*channelTask
	stopping  map[string]bool

	// fingerprints is the bounded duplicate-suppression memory
	// (_origin_reply_fingerprints, manager.py:148). It has its own lock because
	// it is touched only from the dispatcher goroutine and the tests.
	fingerprints *fingerprintStore

	started atomic.Bool

	// dispatchCancel stops the outbound dispatcher; dispatchDone closes when it
	// has returned (including its final cancellation sweep).
	dispatchCancel context.CancelFunc
	dispatchDone   chan struct{}

	// outbound state (P0.5). See manager_outbound.go.
	outbound outboundState
}

// New builds a ChannelManager.
//
// cfg may be nil, in which case the package defaults are used — the same thing
// a freshly constructed Python Config carries. NOTE: a zero-value
// &config.Config{} has Channels.SendMaxRetries == 0, which the reference reads
// literally (`max(0, 1)` -> one attempt, manager.py:1012); use
// config.DefaultConfig() or a loaded config to get the default of 3.
//
// bus is the outbound queue the dispatcher drains. A nil bus is reported when
// the dispatcher is started, not at construction.
func New(cfg *config.Config, bus OutboundBus, opts ...ManagerOption) *ChannelManager {
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	m := &ChannelManager{
		config:       cfg,
		bus:          bus,
		logger:       slog.Default(),
		pendingLimit: OutboundPendingLimit,
		sendLimit:    OutboundConcurrency,
		retryDelays:  defaultRetryDelaysNanos(),
		channels:     map[string]Channel{},
		owners:       map[string]string{},
		specs:        map[string]channelSpec{},
		errs:         map[string]string{},
		tasks:        map[string]*channelTask{},
		stopping:     map[string]bool{},
		fingerprints: newFingerprintStore(OriginReplyFingerprintsMaxSize),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	m.outbound.init(m.pendingLimit, m.sendLimit)
	return m
}

func (m *ChannelManager) loggerOf() *slog.Logger {
	if m.logger != nil {
		return m.logger
	}
	return slog.Default()
}

// Config returns the configuration the manager was built with.
func (m *ChannelManager) Config() *config.Config { return m.config }

// ---------------------------------------------------------------------------
// Channel registry
//
// Python mutates `self.channels` (a dict) directly and relies on its insertion
// order for start order, stop order, `enabled_channels` and `get_status`. Go
// maps are unordered, so the order is tracked explicitly. Assignment to an
// existing name keeps the original position, exactly as a Python dict does.
// ---------------------------------------------------------------------------

// AddChannel registers a channel under its runtime name.
//
// Python: `manager.channels[name] = channel`. The owner and instance id are
// left unset, so GetStatus falls back to (name, "default") for this runtime —
// which is what `get_status`'s setdefault does for a channel that has no
// `_channel_runtime_specs` entry (manager.py:1066-1070).
func (m *ChannelManager) AddChannel(name string, ch Channel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setChannelLocked(name, ch)
}

// AddChannelInstance registers a channel that belongs to a plugin instance.
//
// Python: the `_init_channels` body that fills `self.channels[runtime_name]`,
// `self._channel_owners[runtime_name]` and `self._channel_runtime_specs` from
// `channel_instance_specs` (manager.py:296-303, contracts.py:337-385).
func (m *ChannelManager) AddChannelInstance(runtimeName, owner, instanceID string, ch Channel) {
	m.mu.Lock()
	m.setChannelLocked(runtimeName, ch)
	m.mu.Unlock()
	m.SetChannelRuntimeSpec(runtimeName, owner, instanceID)
}

func (m *ChannelManager) setChannelLocked(name string, ch Channel) {
	if _, ok := m.channels[name]; !ok {
		m.order = append(m.order, name)
	}
	m.channels[name] = ch
}

// SetChannelRuntimeSpec records a runtime's plugin owner and instance id.
//
// Python: the `_channel_runtime_specs[runtime_name] = (owner, instance_id)`
// assignment that `_init_channels` performs from `channel_instance_specs`
// (manager.py:295-299). It is separate from AddChannelInstance because a
// runtime whose plugin failed to load has a spec and NO channel, and
// get_status must still report it (manager.py:1064-1077).
func (m *ChannelManager) SetChannelRuntimeSpec(runtimeName, owner, instanceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.owners[runtimeName] = owner
	if _, ok := m.specs[runtimeName]; !ok {
		m.specOrder = append(m.specOrder, runtimeName)
	}
	m.specs[runtimeName] = channelSpec{owner: owner, instanceID: instanceID}
}

// RemoveChannel drops a runtime. Python: `del manager.channels[name]` (used by
// the disable arm of the hot-reload path, manager.py:466-485).
func (m *ChannelManager) RemoveChannel(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.channels[name]; !ok {
		return
	}
	delete(m.channels, name)
	for i, n := range m.order {
		if n == name {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	delete(m.owners, name)
	if _, ok := m.specs[name]; ok {
		delete(m.specs, name)
		for i, n := range m.specOrder {
			if n == name {
				m.specOrder = append(m.specOrder[:i], m.specOrder[i+1:]...)
				break
			}
		}
	}
	delete(m.errs, name)
}

// GetChannel returns a channel by runtime name. Port of get_channel
// (manager.py:1058-1060).
func (m *ChannelManager) GetChannel(name string) (Channel, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.channels[name]
	return ch, ok
}

func (m *ChannelManager) channel(name string) (Channel, bool) { return m.GetChannel(name) }

// EnabledChannels lists the registered runtime names in insertion order. Port
// of the `enabled_channels` property (manager.py:1098-1101), which is
// `list(self.channels.keys())`.
func (m *ChannelManager) EnabledChannels() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.order...)
}

// SetChannelError records a runtime's startup failure. Python:
// `self._channel_errors[name] = message` (manager.py:385, :326).
func (m *ChannelManager) SetChannelError(runtimeName, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errs[runtimeName] = message
}

// ChannelError returns a runtime's recorded startup failure.
func (m *ChannelManager) ChannelError(runtimeName string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.errs[runtimeName]
	return e, ok
}

// Started reports whether StartAll has run without a matching StopAll.
func (m *ChannelManager) Started() bool { return m.started.Load() }

// ---------------------------------------------------------------------------
// Lifecycle (P0.6)
// ---------------------------------------------------------------------------

// StartAll starts the outbound dispatcher and every channel, then blocks until
// they all finish. Port of start_all (manager.py:598-616).
//
// A channel that fails to start does NOT stop the gateway: _start_channel
// records the error and the remaining channels keep running (manager.py:383-389).
//
// Python's `await asyncio.gather(*tasks, return_exceptions=True)` means this
// call does not return while any channel is alive — channels are long-running.
// Callers that do not want to block should run StartAll in its own goroutine,
// which is exactly what the gateway does (cli/gateway_runtime.py:940-941).
//
// ctx cancellation stops every channel task and returns ctx.Err(), matching
// gather's cancellation propagation. A nil/zero-channel manager returns
// immediately WITHOUT setting the started flag (manager.py:600-602).
func (m *ChannelManager) StartAll(ctx context.Context) error {
	names := m.EnabledChannels()
	if len(names) == 0 {
		m.loggerOf().Warn("No channels enabled")
		return nil
	}
	if m.bus == nil {
		return ErrNoBus
	}

	m.started.Store(true)

	// Start the outbound dispatcher (manager.py:606). Its context is NOT the
	// caller's: Python creates the task without linking it to the caller, so a
	// cancelled StartAll does not by itself stop the dispatcher — StopAll does.
	dispatchCtx, dispatchCancel := context.WithCancel(context.Background())
	dispatchDone := make(chan struct{})
	m.mu.Lock()
	m.dispatchCancel = dispatchCancel
	m.dispatchDone = dispatchDone
	m.mu.Unlock()
	go func() {
		defer close(dispatchDone)
		m.dispatchOutbound(dispatchCtx)
	}()

	// One task per channel (manager.py:609-611).
	type running struct {
		name string
		task *channelTask
	}
	var wg sync.WaitGroup
	started := make([]running, 0, len(names))
	for _, name := range names {
		ch, ok := m.channel(name)
		if !ok {
			continue
		}
		m.loggerOf().Info("Starting channel...", "channel", name)
		taskCtx, cancel := context.WithCancel(context.Background())
		task := &channelTask{cancel: cancel, done: make(chan struct{})}
		m.mu.Lock()
		m.tasks[name] = task
		m.mu.Unlock()
		started = append(started, running{name: name, task: task})
		wg.Add(1)
		go func(name string, ch Channel, task *channelTask, taskCtx context.Context) {
			defer wg.Done()
			defer close(task.done)
			m.startChannel(taskCtx, name, ch)
		}(name, ch, task, taskCtx)
	}

	// `await asyncio.gather(*tasks, return_exceptions=True)`.
	allDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(allDone)
	}()

	select {
	case <-allDone:
		return nil
	case <-ctx.Done():
		for _, r := range started {
			r.task.cancel()
		}
		<-allDone
		return ctx.Err()
	}
}

// startChannel runs one channel's Start and records a failure.
// Port of _start_channel (manager.py:373-389).
//
// The prior error is cleared FIRST, so a restart of a previously failed channel
// is reported as starting rather than failed (manager.py:378).
func (m *ChannelManager) startChannel(ctx context.Context, name string, ch Channel) {
	m.mu.Lock()
	delete(m.errs, name)
	m.mu.Unlock()

	err := ch.Start(ctx)
	if err == nil {
		return
	}
	// Python: `except asyncio.CancelledError: raise`. When the task was
	// cancelled the loop would have raised CancelledError at the await point
	// rather than returning a value, so nothing is recorded here.
	if ctx.Err() != nil {
		return
	}
	public := startErrorMessage(ch, err)
	if public == "" {
		public = genericStartError
	}
	m.SetChannelError(name, public)
	if public != genericStartError {
		m.loggerOf().Error("Failed to start channel",
			"channel", name, "error", public)
		return
	}
	m.loggerOf().Error("Failed to start channel", "channel", name, "error", err)
}

// StopAll stops the outbound dispatcher and every channel.
// Port of stop_all (manager.py:668-681).
//
// The channel list is snapshotted before the loop, matching `list(self.channels)`.
// An error aborts the loop and is returned, matching Python's propagation of a
// CancelledError out of `await self._stop_channel(name)`.
func (m *ChannelManager) StopAll(ctx context.Context) error {
	m.loggerOf().Info("Stopping all channels...")
	m.started.Store(false)

	// Stop dispatcher (manager.py:674-677).
	m.mu.Lock()
	cancel, done := m.dispatchCancel, m.dispatchDone
	m.dispatchCancel, m.dispatchDone = nil, nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		if done != nil {
			<-done
		}
	}

	for _, name := range m.EnabledChannels() {
		if _, err := m.StopChannel(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

// StopChannel stops one channel: first its in-flight outbound sends, then the
// runtime. Port of _stop_channel (manager.py:397-403).
//
// The stopping mark is what makes _queue_outbound drop a message addressed to a
// channel that is being torn down (manager.py:735-737).
func (m *ChannelManager) StopChannel(ctx context.Context, name string) (bool, error) {
	m.mu.Lock()
	m.stopping[name] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.stopping, name)
		m.mu.Unlock()
	}()

	m.cancelOutbound(ctx, name)
	return m.stopChannelRuntime(ctx, name)
}

// stopChannelRuntime stops the channel and then its start task.
// Port of _stop_channel_runtime (manager.py:405-427).
//
// Returns false when the runtime is not registered — and in that case the start
// task entry is dropped anyway (manager.py:407-409).
func (m *ChannelManager) stopChannelRuntime(ctx context.Context, name string) (bool, error) {
	ch, ok := m.channel(name)
	if !ok || ch == nil {
		m.mu.Lock()
		delete(m.tasks, name)
		m.mu.Unlock()
		return false, nil
	}

	m.mu.Lock()
	task := m.tasks[name]
	delete(m.tasks, name)
	m.mu.Unlock()

	err := ch.Stop(ctx)
	// Python re-raises CancelledError when the CURRENT task is itself being
	// cancelled (manager.py:415-419); the start-task cancellation below is then
	// skipped, because the raise happens inside the except block. Reproduced
	// rather than "fixed": the divergence is documented, not silent.
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if err != nil {
		m.loggerOf().Error("Error stopping channel", "channel", name, "error", err)
	} else {
		m.loggerOf().Info("Stopped channel", "channel", name)
	}

	if task != nil && !task.finished() {
		task.cancel()
		<-task.done
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// Status (manager.py:1062-1096)
// ---------------------------------------------------------------------------

// GetStatus returns the runtime state of every known runtime, including ones
// that failed to start. Port of get_status (manager.py:1062-1096).
//
// The key set is the union of the registered runtime specs and the registered
// channels: a channel with no spec is reported under (owner=name,
// instance_id="default"), which is what the reference's setdefault does.
func (m *ChannelManager) GetStatus() map[string]ChannelStatus {
	type entry struct {
		name string
		spec channelSpec
	}

	m.mu.Lock()
	entries := make([]entry, 0, len(m.specOrder)+len(m.order))
	seen := make(map[string]bool, len(m.specOrder)+len(m.order))
	for _, name := range m.specOrder {
		spec, ok := m.specs[name]
		if !ok {
			continue
		}
		entries = append(entries, entry{name: name, spec: spec})
		seen[name] = true
	}
	for _, name := range m.order {
		if seen[name] {
			continue
		}
		owner, ok := m.owners[name]
		if !ok {
			owner = name
		}
		entries = append(entries, entry{name: name, spec: channelSpec{owner: owner, instanceID: "default"}})
		seen[name] = true
	}
	channels := make(map[string]Channel, len(m.channels))
	for k, v := range m.channels {
		channels[k] = v
	}
	tasks := make(map[string]*channelTask, len(m.tasks))
	for k, v := range m.tasks {
		tasks[k] = v
	}
	errs := make(map[string]string, len(m.errs))
	for k, v := range m.errs {
		errs[k] = v
	}
	m.mu.Unlock()

	status := make(map[string]ChannelStatus, len(entries))
	for _, e := range entries {
		ch := channels[e.name]
		task := tasks[e.name]
		errMsg := errs[e.name]
		running := ch != nil && channelIsRunning(ch)

		var state string
		switch {
		case errMsg != "":
			state = ChannelStateFailed
		case running:
			state = ChannelStateRunning
		case task != nil && !task.finished():
			state = ChannelStateStarting
		default:
			state = ChannelStateStopped
		}

		st := ChannelStatus{
			Enabled:    true,
			Running:    running,
			State:      state,
			Owner:      e.spec.owner,
			InstanceID: e.spec.instanceID,
		}
		if errMsg != "" {
			st.Error = errMsg
		}
		status[e.name] = st
	}
	return status
}

// StatusOrder returns the runtime names in get_status's iteration order.
//
// Python's status dict preserves insertion order; a Go map does not, so the
// order is exposed separately for callers that serialize it.
func (m *ChannelManager) StatusOrder() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.specOrder)+len(m.order))
	seen := make(map[string]bool, len(m.specOrder)+len(m.order))
	for _, name := range m.specOrder {
		if _, ok := m.specs[name]; !ok {
			continue
		}
		out = append(out, name)
		seen[name] = true
	}
	for _, name := range m.order {
		if !seen[name] {
			out = append(out, name)
			seen[name] = true
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Optional-hook helpers
// ---------------------------------------------------------------------------

// channelIsRunning is `channel.is_running` (base.py:338-341). A channel that
// does not expose the property is reported as not running, which is the
// reference's value for a channel that never set `_running`.
func channelIsRunning(ch Channel) bool {
	if rs, ok := ch.(RunningState); ok {
		return rs.IsRunning()
	}
	return false
}

// startErrorMessage is `channel.start_error_message(exc)` (base.py:121-127).
// An empty string means "no channel-specific message", which is how the
// reference's None behaves at its only call site (manager.py:385).
func startErrorMessage(ch Channel, err error) string {
	if sm, ok := ch.(StartErrorMessenger); ok {
		return sm.StartErrorMessage(err)
	}
	return ""
}

// shouldRetrySendError is `channel.should_retry_send_error(exc)`
// (base.py:112-119). BaseChannel's default is True, so a channel that does not
// expose the hook retries.
func shouldRetrySendError(ch Channel, err error) bool {
	if p, ok := ch.(SendErrorPolicy); ok {
		return p.ShouldRetrySendError(err)
	}
	return true
}

// deliveryPolicy returns the channel's progress policy. Every Python channel
// inherits BaseChannel's class attributes, so a channel without the hook keeps
// the permissive defaults.
// deliveryPolicyReader is the READ-ONLY half of DeliveryPolicy.
//
// Python reads three plain attributes — ch.send_progress, ch.send_tool_hints and
// ch.show_reasoning (manager.py:353, :797) — so any object that exposes them
// qualifies, whatever it does about writing them. Requiring the setters as well
// would make a channel whose values are computed rather than stored (say, from
// its own config section) fall through to the `true, true, true` default, which
// is a silently wrong answer — the manager would deliver progress the channel
// never asked for — rather than a missing capability.
type deliveryPolicyReader interface {
	SendProgress() bool
	SendToolHints() bool
	ShowReasoning() bool
}

// deliveryPolicy reads a channel's progress/reasoning policy.
//
// A channel that does not implement the read-only surface keeps the global
// defaults, which is what BaseChannel's own defaults are.
func deliveryPolicy(ch Channel) (progress, toolHints, reasoning bool) {
	progress, toolHints, reasoning = true, true, true
	if dp, ok := ch.(deliveryPolicyReader); ok {
		progress = dp.SendProgress()
		toolHints = dp.SendToolHints()
		reasoning = dp.ShowReasoning()
	}
	return progress, toolHints, reasoning
}

// shouldSendProgress is _should_send_progress (manager.py:347-353).
//
// An unknown channel returns false, which is why a progress event addressed to
// a channel that is not registered is dropped rather than warned about.
func (m *ChannelManager) shouldSendProgress(channelName string, toolHint bool) bool {
	ch, ok := m.channel(channelName)
	if !ok {
		m.loggerOf().Debug("Progress check for unknown channel", "channel", channelName)
		return false
	}
	progress, hints, _ := deliveryPolicy(ch)
	if toolHint {
		return hints
	}
	return progress
}
