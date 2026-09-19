package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	micrographrag "github.com/adrianolimagarcia/micrographrag-go"

	"github.com/adrianolimagarcia/nanobot-go/internal/bus"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/observability"
	"github.com/adrianolimagarcia/nanobot-go/internal/prompt"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

func (l *Loop) graphMemoryContext(ctx context.Context, store *micrographrag.Store, query string) (string, error) {
	// Start from the STORE'S OWN defaults and override only the limit.
	//
	// Passing a bare literal here was a silent no-op. micrographrag's
	// normalizeSearchOptions (search.go:27-58) fills Limit/FTSLimit/GraphDepth
	// and the weights from the defaults but NEVER copies EnableFTS, EnableVector
	// or EnableGraph, and Search only consults an engine whose flag is set. A
	// non-zero options struct therefore disabled every engine: measured against
	// a store holding one matching memory,
	//     SearchOptions{}               -> 1 result
	//     SearchOptions{Limit: 6}       -> 0 results, nil error   <-- what this
	//     SearchOptions{Limit:6,EnableFTS:true} -> 1 result
	// so the retrieved-memory block was always empty and never reported a
	// failure. DefaultSearchOptions carries the flags the store was actually
	// opened with, which is what we want; Limit is the only thing to tune.
	opts := store.DefaultSearchOptions()
	opts.Limit = 6
	results, err := store.Search(ctx, query, opts)
	if err != nil {
		return "", err
	}
	limit := l.cfg.GraphMemoryMaxChars
	if limit <= 0 {
		limit = 6000
	}
	var b strings.Builder
	for i, result := range results {
		if b.Len() >= limit {
			break
		}
		content := strings.ReplaceAll(strings.ReplaceAll(result.Content, "<", "&lt;"), ">", "&gt;")
		remaining := limit - b.Len()
		if len(content) > remaining {
			content = content[:remaining]
		}
		fmt.Fprintf(&b, "[%d] chunk=%d score=%.4f source=derived\n%s\n\n", i+1, result.ChunkID, result.Score, content)
	}
	return b.String(), nil
}

type Transcript interface {
	Key() string
	Messages() []core.Message
	AddMessage(m core.Message)
	Clear()
	Save() error
}

type TranscriptStore interface {
	Open(key string) (Transcript, error)
}

// transcriptJournalAppender is implemented by the session store's latency
// fast-path. Falling back to AddMessage+Save keeps custom/test transcript
// implementations compatible.
type transcriptJournalAppender interface {
	AppendMessagesDurable([]core.Message) error
}

func persistTranscriptMessages(t Transcript, messages []core.Message) error {
	if len(messages) == 0 {
		return nil
	}
	if appender, ok := t.(transcriptJournalAppender); ok {
		return appender.AppendMessagesDurable(messages)
	}
	for _, message := range messages {
		t.AddMessage(message)
	}
	return t.Save()
}

type transcriptMessageMutator interface {
	SetMessage(int, core.Message) error
}

type transcriptKeyLister interface {
	List() ([]string, error)
}

type graphMemoryPending struct {
	TurnID     string `json:"turn_id"`
	SessionKey string `json:"session_key"`
	Content    string `json:"content"`
}

const graphMemoryPendingExtra = "_haosbot_graph_memory_pending"

// ErrTurnActive is returned when another non-command turn is already running
// for the same session. Cancelling a paid provider request just because a
// browser submitted a duplicate is both wasteful and a source of transcript
// races, so the loop rejects the duplicate instead.
var ErrTurnActive = errors.New("agent: session already has an active turn")

func (l *Loop) RecoverPendingGraphMemory() error {
	if l.cfg.GraphMemoryEnqueueWithIDError == nil {
		return nil
	}
	lister, ok := l.cfg.Store.(transcriptKeyLister)
	if !ok {
		return nil
	}
	keys, err := lister.List()
	if err != nil {
		return fmt.Errorf("agent: list sessions for GraphRAG recovery: %w", err)
	}
	for _, key := range keys {
		transcript, err := l.cfg.Store.Open(key)
		if err != nil {
			return fmt.Errorf("agent: open session %q for GraphRAG recovery: %w", key, err)
		}
		if err := l.reconcilePendingGraphMemory(transcript); err != nil {
			return err
		}
	}
	return nil
}

func (l *Loop) reconcilePendingGraphMemory(transcript Transcript) error {
	if l.cfg.GraphMemoryEnqueueWithIDError == nil {
		return nil
	}
	mutator, ok := transcript.(transcriptMessageMutator)
	if !ok {
		return nil
	}
	messages := transcript.Messages()
	changed := false
	for i := range messages {
		raw, exists := messages[i].Extra(graphMemoryPendingExtra)
		if !exists {
			continue
		}
		var pending graphMemoryPending
		if err := json.Unmarshal(raw, &pending); err != nil {
			return fmt.Errorf("agent: decode pending GraphRAG job: %w", err)
		}
		if pending.TurnID == "" || pending.SessionKey == "" {
			return errors.New("agent: pending GraphRAG job is missing identity")
		}
		if err := l.cfg.GraphMemoryEnqueueWithIDError(pending.TurnID, pending.SessionKey, pending.Content); err != nil {
			return fmt.Errorf("agent: recover GraphRAG job %s: %w", pending.TurnID, err)
		}
		messages[i].DeleteExtra(graphMemoryPendingExtra)
		if err := mutator.SetMessage(i, messages[i]); err != nil {
			return fmt.Errorf("agent: clear recovered GraphRAG marker: %w", err)
		}
		changed = true
	}
	if changed {
		if err := transcript.Save(); err != nil {
			return fmt.Errorf("agent: persist recovered GraphRAG marker: %w", err)
		}
	}
	return nil
}

type sessionTranscript interface {
	Transcript
	CommitSummaryCheckpoint(summary string, insertAt *int, lastActive *time.Time)
	GetHistory(maxMessages, maxTokens int, extendToUser, includeRuntimeContext bool) []core.Message
	Metadata() map[string]any
	UpdatedAt() time.Time
	LastArchived() int
}

const DefaultWebUIAutoSummarizeTokens = 120_000

// Summarization call bounds.
//
// The summarize call is a plain completion, so it must not be shaped like the
// agent turn it is replacing. Two measured facts about the deployed provider
// (an OpenAI-compatible proxy) drive these numbers:
//
//   - A NON-streaming request whose max_tokens is large never comes back. The
//     same payload answered in 7 s at max_tokens=128 and 16 s at 512, but did
//     not return within 90 s at 1024 and was still absent after 300 s at 1024.
//     The OpenAI client caps a non-streaming call at requestTimeout=120 s
//     (client.go:54), so the 2048-token summarize request this code used to
//     send was guaranteed to abort and burn two thirds of the gateway's 180 s
//     turn budget (api/agent_turn.go:44) before the real turn even started.
//   - The SAME request streamed returns 200 in 32 s with finish_reason=stop.
//
// So the summarize call streams whenever the provider supports it, and its
// output is capped at a size that is useful for a checkpoint summary without
// inviting an unbounded generation. DefaultSummarizeTimeout is a second,
// independent bound: even a stalled stream may not consume the whole turn.
const (
	// DefaultSummarizeMaxTokens caps the checkpoint summary length.
	DefaultSummarizeMaxTokens = 1024
	// DefaultSummarizeTimeout bounds the whole summarize call. It is kept well
	// below the gateway's 180 s turn deadline so a slow summary degrades into
	// "keep the full history" instead of "the turn never answers".
	DefaultSummarizeTimeout = 90 * time.Second
	// DefaultSummarizeInputTokens caps how much transcript is folded into one
	// summarize call. Older turns beyond the budget are dropped from the
	// request and the prompt says so, rather than growing the call without
	// bound.
	DefaultSummarizeInputTokens = 48_000
	// summarizeRetryGrowthRatio and summarizeRetryCooldown add hysteresis
	// around a FAILED attempt. Without them a session that is over the
	// threshold but cannot be summarized retries on every single turn, which
	// makes the session permanently unusable.
	summarizeRetryGrowthRatio = 1.25
	summarizeRetryCooldown    = 10 * time.Minute
)

// summarizeAttempt records the last summarize attempt for one session.
type summarizeAttempt struct {
	tokens int
	at     time.Time
	failed bool
}

func pyTruthy(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	s := string(trimmed)
	return s != "false" && s != "null" && s != "0" && s != `""` && s != "[]" && s != "{}"
}

func isSummaryCheckpointMessage(m core.Message) bool {
	if raw, ok := m.Extra("_hidden_history"); !ok || !pyTruthy(raw) {
		return false
	}
	return m.Content.IsText() && m.Content.Text == "Continue the active task from the working-memory checkpoint above."
}

func isCommandEcho(m core.Message) bool {
	if raw, ok := m.Extra("_command"); ok && pyTruthy(raw) {
		return true
	}
	return false
}

func sessionSummaryFromMeta(meta map[string]any) (string, string) {
	if meta == nil {
		return "", ""
	}
	raw, ok := meta["_last_summary"].(map[string]any)
	if !ok {
		return "", ""
	}
	text, _ := raw["text"].(string)
	lastActive, _ := raw["last_active"].(string)
	return text, lastActive
}

func isWebInteraction(msg core.InboundMessage, key string) bool {
	if msg.Channel == "webui" || strings.HasPrefix(key, "webui:") {
		return true
	}
	if msg.Metadata != nil {
		if src, ok := msg.Metadata["source"].(string); ok && src == "webui" {
			return true
		}
	}
	return false
}

type LoopConfig struct {
	Bus      *bus.Bus
	Store    TranscriptStore
	Provider provider.Provider
	Tools    *tools.Registry
	Prompt   *prompt.Builder
	Runner   *Runner

	ContextWindowTokens int
	AutoSummarizeTokens int
	// AutoSummarizeMaxTokens, AutoSummarizeTimeout and
	// AutoSummarizeInputTokens bound the summarize call itself. Zero selects
	// the Default* constants above.
	AutoSummarizeMaxTokens   int
	AutoSummarizeTimeout     time.Duration
	AutoSummarizeInputTokens int
	Model               string
	MaxTokens           int
	Temperature         float64
	Workspace           string
	ProjectWorkspace    string
	MaxIterations       int
	MaxToolResultChars  int
	ConcurrentTools     bool
	SequentialTools     bool
	ReasoningEffort     string
	IncludeMemory       bool
	SystemPrompt        string

	GraphMemory           *micrographrag.Store
	GraphMemoryForSession    func(context.Context, string) (*micrographrag.Store, error)
	GraphMemoryEnqueue            func(string, string) bool
	GraphMemoryEnqueueWithID      func(string, string, string) bool
	GraphMemoryEnqueueWithIDError func(string, string, string) error
	GraphMemoryMaxChars           int
	Metrics                       *observability.Registry
}

type activeTurn struct {
	generation uint64
	cancel     context.CancelFunc
}

type backgroundSummaryTask struct {
	generation uint64
	cancel     context.CancelFunc
}

type Loop struct {
	cfg LoopConfig

	mu                  sync.Mutex
	active              map[string]activeTurn
	nextGeneration      uint64
	backgroundSummaries map[string]backgroundSummaryTask
	nextBackground      uint64

	summarizeMu       sync.Mutex
	summarizeAttempts map[string]summarizeAttempt
}

func NewLoop(cfg LoopConfig) (*Loop, error) {
	if cfg.Bus == nil {
		return nil, errors.New("agent: LoopConfig.Bus is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("agent: LoopConfig.Store is required")
	}
	if cfg.Provider == nil {
		return nil, errors.New("agent: LoopConfig.Provider is required")
	}
	if cfg.Prompt == nil {
		cfg.Prompt = prompt.New(cfg.Workspace)
	}
	if cfg.Runner == nil {
		cfg.Runner = NewRunner()
	}
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = DefaultMaxToolIterations
	}
	if cfg.MaxToolResultChars <= 0 {
		cfg.MaxToolResultChars = DefaultMaxToolResultChars
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 8192
	}
	if cfg.Temperature == 0 {
		cfg.Temperature = 0.1
	}
	if !cfg.SequentialTools {
		cfg.ConcurrentTools = true
	}
	return &Loop{
		cfg:                 cfg,
		active:              map[string]activeTurn{},
		backgroundSummaries: map[string]backgroundSummaryTask{},
		summarizeAttempts:   map[string]summarizeAttempt{},
	}, nil
}

// graphStoreForSession returns the derived-memory store for a session together
// with the function that releases it. The release function is never nil.
//
// A provider may pin the store it hands out — cmd/haosbot's graphStorePool
// evicts and closes least-recently-used stores once it reaches its limit — and
// GraphMemoryForSession's signature has no room for a release value, so the pin
// is bound to the lifetime of the context that is passed in: the provider
// releases the store when that context is cancelled.
//
// The context handed to the provider is therefore derived with WithoutCancel
// plus WithCancel: it must outlive the retrieval (a cancelled request does not
// mean the search has stopped), and it must be cancelled exactly when the loop
// is done with the store, which is what the returned release function does.
func (l *Loop) graphStoreForSession(ctx context.Context, key string) (*micrographrag.Store, func()) {
	if l.cfg.GraphMemoryForSession == nil {
		return l.cfg.GraphMemory, func() {}
	}
	pinCtx, unpin := context.WithCancel(context.WithoutCancel(ctx))
	store, err := l.cfg.GraphMemoryForSession(pinCtx, key)
	if err != nil || store == nil {
		unpin()
		return nil, func() {}
	}
	return store, unpin
}

// graphMemoryRetrieve acquires the session store, runs the search and releases
// the store before returning, so the pool can evict it again and no eviction
// can close it while it is being read.
func (l *Loop) graphMemoryRetrieve(ctx context.Context, key, query string) string {
	store, release := l.graphStoreForSession(ctx, key)
	if store == nil {
		return ""
	}
	defer release()
	out, err := l.graphMemoryContext(ctx, store, query)
	if err != nil {
		return ""
	}
	return out
}

func (l *Loop) Run(ctx context.Context) error {
	for {
		msg, err := l.cfg.Bus.ConsumeInbound(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			if errors.Is(err, bus.ErrClosed) {
				return nil
			}
			return err
		}
		if err := l.Handle(ctx, msg); err != nil {
			_ = l.cfg.Bus.PublishOutbound(ctx, core.OutboundMessage{
				Channel: msg.Channel,
				ChatID:  msg.ChatID,
				Content: fmt.Sprintf("Error: %v", err),
			})
		}
	}
}

func (l *Loop) Handle(ctx context.Context, msg core.InboundMessage) error {
	out, err := l.ProcessMessage(ctx, msg)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return l.cfg.Bus.PublishOutbound(ctx, *out)
}

func (l *Loop) ProcessMessage(ctx context.Context, msg core.InboundMessage) (*core.OutboundMessage, error) {
	return l.processMessage(ctx, msg, nil)
}

// ProcessMessageWithHook runs a stateful turn and forwards runner progress to
// hook. It is used by the WebUI streaming endpoint; callers that need only the
// final response should use ProcessMessage.
func (l *Loop) ProcessMessageWithHook(ctx context.Context, msg core.InboundMessage, hook Hook) (*core.OutboundMessage, error) {
	return l.processMessage(ctx, msg, hook)
}

// closeUnansweredTurn records a turn that ended without a final answer.
//
// The user message is saved durably before the runner starts, but the turn's
// own messages are appended only once the runner returns cleanly. A turn that
// dies to the request deadline or to a cancellation therefore left that user
// message as the last entry of the transcript, and the next turn saw an
// unanswered question, answered the stale question, and left the new one
// unanswered in turn. Appending a terminal marker closes the turn so the
// transcript never ends on an unanswered user message.
//
// Only the marker is persisted, never res.Messages: a cancelled run can stop
// between an assistant tool_call and its matching tool result, and replaying
// that pair half-written would send the provider a malformed request on the
// next turn.
func (l *Loop) closeUnansweredTurn(transcript Transcript, turnID, reason string) {
	marker := *core.NewMessage(core.RoleAssistant,
		fmt.Sprintf("[turn ended without an answer: %s]", reason))
	marker.Timestamp = isoLocal(time.Now())
	marker.SetExtra("turn_id", mustRawAny(turnID))
	if err := persistTranscriptMessages(transcript, []core.Message{marker}); err != nil {
		slog.Warn("agent: could not persist unanswered-turn marker",
			"session", transcript.Key(), "turn_id", turnID, "error", err)
	}
}

func (l *Loop) processMessage(ctx context.Context, msg core.InboundMessage, hook Hook) (*core.OutboundMessage, error) {
	turnStarted := time.Now()
	if l.cfg.Metrics != nil {
		l.cfg.Metrics.IncTurns()
		defer func() { l.cfg.Metrics.ObserveTurn(time.Since(turnStarted)) }()
	}
	key := msg.SessionKey()
	isCommand := msg.IsUserInput() && msg.Channel != "system" && strings.HasPrefix(strings.TrimSpace(msg.Content), "/")
	cmd := ""
	if isCommand {
		cmd = firstWord(msg.Content)
	}
	// Mutating session commands must share the same exclusivity gate as normal
	// turns. /stop, /status and /help remain callable while a turn is active,
	// but /new and /compact cannot race provider/tool/persistence work on the
	// same transcript.
	requiresExclusiveTurn := !isCommand || cmd == "/new" || cmd == "/compact"
	var runCtx context.Context = ctx
	var cancel context.CancelFunc
	var generation uint64
	if requiresExclusiveTurn {
		runCtx, cancel = context.WithCancel(ctx)
		var accepted bool
		generation, accepted = l.tryRegisterActive(key, cancel)
		if !accepted {
			cancel()
			if l.cfg.Metrics != nil { l.cfg.Metrics.IncTurnErrors() }
			return nil, ErrTurnActive
		}
		defer func() {
			l.unregisterActive(key, generation)
			cancel()
		}()
	}

	transcript, err := l.cfg.Store.Open(key)
	if err != nil {
		return nil, fmt.Errorf("agent: open session %q: %w", key, err)
	}
	var persistence time.Duration
	persistMessages := func(messages []core.Message) error {
		started := time.Now()
		err := persistTranscriptMessages(transcript, messages)
		persistence += time.Since(started)
		return err
	}
	if l.cfg.Metrics != nil {
		defer func() { l.cfg.Metrics.ObservePersistence(persistence) }()
	}
	// GraphRAG recovery is performed once during runtime startup. It is
	// deliberately NOT reconciled here: SQLite/outbox recovery must never sit
	// in front of Provider.ChatStream on an interactive turn.

	if isCommand {
		if handled, out := l.dispatchCommand(ctx, transcript, msg); handled {
			return out, nil
		}
		if cmd := firstWord(msg.Content); cmd != "" {
			return &core.OutboundMessage{
				Channel:  msg.Channel,
				ChatID:   msg.ChatID,
				Content:  fmt.Sprintf("Unknown command: %s\n\nUse /help to list available commands.", cmd),
				Metadata: msg.Metadata,
			}, nil
		}
	}

	var promptSummary *prompt.Summary
	var sessTranscript sessionTranscript
	if st, ok := transcript.(sessionTranscript); ok {
		sessTranscript = st
		if text, lastActive := sessionSummaryFromMeta(st.Metadata()); text != "" {
			promptSummary = &prompt.Summary{
				Text:       text,
				LastActive: lastActive,
			}
		}
	}

	systemPrompt := l.cfg.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = l.cfg.Prompt.BuildSystemPrompt(
			msg.Channel, promptSummary, l.cfg.ProjectWorkspace, l.cfg.IncludeMemory)
	}
	// Long-term MEMORY.md is already included by Prompt.BuildSystemPrompt.
	// GraphRAG is a derived index and is only queried explicitly through the
	// memory_search tool; normal turns never wait for graph/vector/FTS work.

	var history []core.Message
	if sessTranscript != nil {
		history = sessTranscript.GetHistory(0, 0, false, true)
	} else {
		history = transcript.Messages()
	}
	turnID := deterministicTurnID(key, len(history), msg.Content)
	// A request can be retried after the user message was durably saved but
	// before the provider result was saved. Reuse that message's turn ID and
	// avoid appending a second logical user turn.
	reusedUser := false
	if len(history) > 0 {
		last := history[len(history)-1]
		if last.Role == core.RoleUser && last.Content.IsText() && last.Content.Text == msg.Content {
			if raw, ok := last.Extra("turn_id"); ok {
				var persistedID string
				if json.Unmarshal(raw, &persistedID) == nil && persistedID != "" {
					turnID = persistedID
					history = history[:len(history)-1]
					reusedUser = true
				}
			}
		}
	}
	modelMessages := make([]core.Message, 0, len(history)+2)
	modelMessages = append(modelMessages, *core.NewMessage(core.RoleSystem, systemPrompt))
	modelMessages = append(modelMessages, history...)
	modelMessages = append(modelMessages, *core.NewMessage(core.RoleUser, msg.Content))

	isWeb := isWebInteraction(msg, key)
	// Automatic summarization is scheduled after the response. No summary
	// provider call is allowed on the pre-provider path.

	if !reusedUser {
		userMsg := *core.NewMessage(core.RoleUser, msg.Content)
		userMsg.Timestamp = isoLocal(time.Now())
		userMsg.SetExtra("turn_id", mustRawAny(turnID))
		if len(msg.Media) > 0 {
			userMsg.SetExtra("media", mustRawAny(msg.Media))
		}
		if err := persistMessages([]core.Message{userMsg}); err != nil {
			return nil, fmt.Errorf("agent: persist user message: %w", err)
		}
	}

	if l.cfg.Metrics != nil {
		l.cfg.Metrics.ObservePreProvider(time.Since(turnStarted))
	}
	res, err := l.cfg.Runner.Run(runCtx, RunSpec{
		Messages:            modelMessages,
		Tools:               l.cfg.Tools,
		Provider:            l.cfg.Provider,
		Model:               l.cfg.Model,
		MaxIterations:       l.cfg.MaxIterations,
		MaxToolResultChars:  l.cfg.MaxToolResultChars,
		MaxTokens:           l.cfg.MaxTokens,
		ContextWindowTokens: l.cfg.ContextWindowTokens,
		Workspace:           l.cfg.Workspace,
		Temperature:         l.cfg.Temperature,
		ReasoningEffort:     l.cfg.ReasoningEffort,
		ConcurrentTools:     l.cfg.ConcurrentTools,
		SessionKey:          key,
		Hook:                hook,
		Metrics:             l.cfg.Metrics,
	})
	if err != nil {
		if l.cfg.Metrics != nil {
			l.cfg.Metrics.IncTurnErrors()
			if errors.Is(err, context.DeadlineExceeded) {
				l.cfg.Metrics.IncProviderTimeouts()
			}
		}
		l.closeUnansweredTurn(transcript, turnID, err.Error())
		return nil, fmt.Errorf("agent: run: %w", err)
	}
	if res.StopReason == StopCanceled && runCtx.Err() != nil {
		l.closeUnansweredTurn(transcript, turnID, runCtx.Err().Error())
		return nil, runCtx.Err()
	}
	if l.cfg.Metrics != nil {
		for _, message := range res.Messages {
			l.cfg.Metrics.AddToolCalls(len(message.ToolCalls))
		}
	}

	if len(res.Messages) >= len(modelMessages) {
		newMessages := append([]core.Message(nil), res.Messages[len(modelMessages):]...)
		graphContent := msg.Content + "\n" + res.FinalContent

		// The recovery marker is written into the SAME append-only journal batch
		// as the assistant/tool records. Once those bytes are durable the turn
		// may return immediately; Memory Fabric/GraphRAG acknowledgement is
		// background work and can never hold the active-turn gate.
		pendingAttached := false
		if l.cfg.GraphMemoryEnqueueWithIDError != nil && len(newMessages) > 0 {
			last := len(newMessages) - 1
			newMessages[last].SetExtra(graphMemoryPendingExtra, mustRawAny(graphMemoryPending{
				TurnID: turnID, SessionKey: key, Content: graphContent,
			}))
			pendingAttached = true
		}
		if err := persistMessages(newMessages); err != nil {
			return nil, fmt.Errorf("agent: persist turn: %w", err)
		}

		if pendingAttached && l.cfg.GraphMemoryEnqueueWithIDError != nil {
			l.enqueuePendingGraphMemoryAsync(turnID, key, graphContent)
		} else if l.cfg.GraphMemoryEnqueueWithIDError != nil {
			go func() {
				if err := l.cfg.GraphMemoryEnqueueWithIDError(turnID, key, graphContent); err != nil {
					slog.Warn("agent: async GraphRAG enqueue failed", "turn_id", turnID, "session", key, "error", err)
				}
			}()
		} else if l.cfg.GraphMemoryEnqueueWithID != nil {
			go func() {
				if !l.cfg.GraphMemoryEnqueueWithID(turnID, key, graphContent) {
					slog.Warn("agent: async GraphRAG enqueue rejected", "turn_id", turnID, "session", key)
				}
			}()
		} else if l.cfg.GraphMemoryEnqueue != nil {
			go func() {
				if !l.cfg.GraphMemoryEnqueue(key, graphContent) {
					slog.Warn("agent: async GraphRAG enqueue rejected", "turn_id", turnID, "session", key)
				}
			}()
		} else if store, release := l.graphStoreForSession(context.Background(), key); store != nil {
			// Compatibility fallback for single-store tests. Production uses
			// the durable Memory Fabric callbacks above.
			go func(store *micrographrag.Store, sourceKey, text string) {
				defer release()
				_, _ = store.AddMemory(context.Background(), micrographrag.MemoryInput{
					Kind: 1, Source: "haosbot/session/" + sourceKey,
					Title: "Agent turn " + sourceKey, Content: text,
				})
			}(store, key, graphContent)
		}
	}

	if sessTranscript != nil {
		l.scheduleBackgroundSummary(key, sessTranscript, isWeb)
	}

	content := res.FinalContent
	if content == "" && res.Error != "" {
		content = res.Error
	}
	return &core.OutboundMessage{
		Channel:  msg.Channel,
		ChatID:   msg.ChatID,
		Content:  content,
		Metadata: msg.Metadata,
	}, nil
}

func (l *Loop) enqueuePendingGraphMemoryAsync(turnID, key, content string) {
	callback := l.cfg.GraphMemoryEnqueueWithIDError
	go func() {
		if err := callback(turnID, key, content); err != nil {
			// Leave the recovery marker in the journal. Startup recovery uses
			// the deterministic turn ID, so retry is idempotent.
			slog.Warn("agent: async GraphRAG enqueue failed; marker retained",
				"turn_id", turnID, "session", key, "error", err)
		}
	}()
}

func deterministicTurnID(sessionKey string, historyLen int, content string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(sessionKey))
	_, _ = h.Write([]byte{0})
	_, _ = fmt.Fprintf(h, "%d", historyLen)
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(content))
	return "turn-" + hex.EncodeToString(h.Sum(nil)[:16])
}

func (l *Loop) autoSummarizeThreshold(isWeb bool) int {
	if l.cfg.AutoSummarizeTokens > 0 {
		return l.cfg.AutoSummarizeTokens
	}
	if env := os.Getenv("HAOSBOT_AUTO_SUMMARIZE_TOKENS"); env != "" {
		if n, err := strconv.Atoi(env); err == nil && n > 0 {
			return n
		}
	}
	if isWeb {
		return DefaultWebUIAutoSummarizeTokens
	}
	return 0
}

func (l *Loop) summarizeMaxTokens() int {
	if l.cfg.AutoSummarizeMaxTokens > 0 {
		return l.cfg.AutoSummarizeMaxTokens
	}
	return DefaultSummarizeMaxTokens
}

func (l *Loop) summarizeTimeout() time.Duration {
	if l.cfg.AutoSummarizeTimeout > 0 {
		return l.cfg.AutoSummarizeTimeout
	}
	return DefaultSummarizeTimeout
}

func (l *Loop) summarizeInputTokens() int {
	if l.cfg.AutoSummarizeInputTokens > 0 {
		return l.cfg.AutoSummarizeInputTokens
	}
	return DefaultSummarizeInputTokens
}

// summarizeSourceBudget reports how many transcript characters may be folded
// into one summarize call. It converts the token budget with a deliberately
// conservative 1 token ~= 2 characters ratio, which is the worst case the
// tokenizer produces for JSON-heavy transcripts.
func (l *Loop) summarizeSourceBudget() int {
	return l.summarizeInputTokens() * 2
}

// shouldAttemptSummarize applies hysteresis around a FAILED attempt.
//
// The threshold alone is not enough to decide. Once a session is over it the
// estimate stays over it on every following turn (the runner trims the
// model-facing copy, never the stored transcript), so an attempt that keeps
// failing would be retried forever and every retry costs the turn its whole
// deadline. After a failure a retry needs either real growth or a cooldown.
func (l *Loop) shouldAttemptSummarize(key string, tokens int) bool {
	l.summarizeMu.Lock()
	defer l.summarizeMu.Unlock()
	prev, ok := l.summarizeAttempts[key]
	if !ok || !prev.failed {
		return true
	}
	if tokens >= int(float64(prev.tokens)*summarizeRetryGrowthRatio) {
		return true
	}
	return time.Since(prev.at) >= summarizeRetryCooldown
}

func (l *Loop) recordSummarizeAttempt(key string, tokens int, failed bool) {
	l.summarizeMu.Lock()
	defer l.summarizeMu.Unlock()
	l.summarizeAttempts[key] = summarizeAttempt{tokens: tokens, at: time.Now(), failed: failed}
}

// summarize performs the checkpoint-summary completion.
//
// It streams whenever the provider can, for the reason documented on the
// DefaultSummarize* constants, and bounds the whole call independently of the
// caller's deadline.
func (l *Loop) summarize(ctx context.Context, req provider.ChatRequest) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, l.summarizeTimeout())
	defer cancel()

	sp, ok := l.cfg.Provider.(provider.StreamingProvider)
	if !ok {
		resp, err := l.cfg.Provider.Chat(ctx, req)
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}

	stream, err := sp.ChatStream(ctx, req)
	if err != nil {
		return "", err
	}

	// The provider closes the channel; keep reading until it does even after a
	// cancellation, or the producer goroutine leaks.
	var text strings.Builder
	var done *core.Response
	var streamErr error
	for ev := range stream {
		switch ev.Kind {
		case core.StreamText:
			text.WriteString(ev.Text)
		case core.StreamDone:
			done = ev.Response
		default:
			// Reasoning, tool-call fragments and usage are not part of the
			// summary.
		}
		if ev.Err != nil && streamErr == nil {
			streamErr = ev.Err
		}
	}
	if streamErr != nil {
		return "", streamErr
	}
	if text.Len() > 0 {
		return text.String(), nil
	}
	if done != nil {
		return done.Content, nil
	}
	return "", nil
}

func (l *Loop) autoSummarize(ctx context.Context, sess sessionTranscript, reusedUser bool) error {
	if l.cfg.Provider == nil {
		return errors.New("agent: autoSummarize: no provider configured")
	}

	msgs := sess.Messages()
	archiveEnd := len(msgs)
	if reusedUser && archiveEnd > 0 {
		archiveEnd--
	}
	lastArchived := sess.LastArchived()
	if lastArchived < 0 {
		lastArchived = 0
	}
	if lastArchived > archiveEnd {
		lastArchived = archiveEnd
	}

	var sourceMsgs []core.Message
	for i := lastArchived; i < archiveEnd; i++ {
		m := msgs[i]
		if isSummaryCheckpointMessage(m) {
			continue
		}
		if isCommandEcho(m) {
			continue
		}
		if m.Content.Text == "" && len(m.ToolCalls) == 0 {
			continue
		}
		sourceMsgs = append(sourceMsgs, m)
	}

	if len(sourceMsgs) == 0 {
		return nil
	}

	// Render the transcript, then keep only the newest entries that fit the
	// summarize input budget. The request has to stay bounded: an unbounded
	// summarize call is part of what made the session unusable.
	rendered := make([]string, len(sourceMsgs))
	for i, m := range sourceMsgs {
		role := strings.ToUpper(string(m.Role))
		text := m.Content.Text
		if len(m.ToolCalls) > 0 {
			var tcNames []string
			for _, tc := range m.ToolCalls {
				tcNames = append(tcNames, tc.Name)
			}
			text += fmt.Sprintf(" [tools: %s]", strings.Join(tcNames, ", "))
		}
		if len(text) > 4000 {
			text = text[:4000] + "... [truncated]"
		}
		rendered[i] = fmt.Sprintf("%s: %s\n\n", role, text)
	}

	// Walk backwards so the newest turns are always the ones kept. The last
	// entry is kept unconditionally, so firstKept never reaches len(rendered).
	budget := l.summarizeSourceBudget()
	used := 0
	firstKept := len(rendered)
	for i := len(rendered) - 1; i >= 0; i-- {
		if used+len(rendered[i]) > budget && i != len(rendered)-1 {
			break
		}
		used += len(rendered[i])
		firstKept = i
	}
	dropped := firstKept

	formattedConversation := strings.Join(rendered[firstKept:], "")

	var prevSummary string
	if text, _ := sessionSummaryFromMeta(sess.Metadata()); text != "" {
		prevSummary = text
	}

	const summarizeSystemPrompt = `You are an expert conversational summarizer for an AI assistant.
Your task is to create a concise, rich, and well-structured replacement checkpoint summary of the conversation history.

Guidelines:
- When a previous summary is present, merge and synthesize it with the new conversation history.
- Preserve key user requirements, preferences, decisions made, architecture choices, file paths, code details, and unresolved blockers.
- Structure with clear bullet points.
- Do not invent facts that are not present in the conversation.
- Keep the summary under 900 words.`

	var userPrompt strings.Builder
	if prevSummary != "" {
		userPrompt.WriteString("## Previous Summary\n")
		userPrompt.WriteString(prevSummary)
		userPrompt.WriteString("\n\n")
	}
	if dropped > 0 {
		fmt.Fprintf(&userPrompt,
			"NOTE: the transcript below is the most recent portion only; %d earlier message(s) were elided because of a size limit. Do not claim to know what those contained.\n\n",
			dropped)
	}
	userPrompt.WriteString("## Conversation History to Summarize\n")
	userPrompt.WriteString(formattedConversation)
	userPrompt.WriteString("\n\nPlease provide the updated comprehensive summary:")

	req := provider.ChatRequest{
		Messages: []core.Message{
			*core.NewMessage(core.RoleSystem, summarizeSystemPrompt),
			*core.NewMessage(core.RoleUser, userPrompt.String()),
		},
		Model:       l.cfg.Model,
		MaxTokens:   l.summarizeMaxTokens(),
		Temperature: 0.2,
	}

	started := time.Now()
	summary, err := l.summarize(ctx, req)
	if l.cfg.Metrics != nil {
		l.cfg.Metrics.ObserveProviderTotal(time.Since(started))
	}
	if err != nil {
		return fmt.Errorf("provider chat: %w", err)
	}
	summaryText := strings.TrimSpace(summary)
	if summaryText == "" {
		return errors.New("provider returned empty summary")
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	var insertAt *int
	if reusedUser {
		idx := archiveEnd
		insertAt = &idx
	}
	lastActive := sess.UpdatedAt()
	sess.CommitSummaryCheckpoint(summaryText, insertAt, &lastActive)
	return sess.Save()
}

// scheduleBackgroundSummary proactively compacts long sessions after a turn has
// completed. It never runs in the request critical path. A new turn cancels the
// task before registering itself, and autoSummarize checks cancellation again
// immediately before committing its checkpoint.
func (l *Loop) scheduleBackgroundSummary(key string, sess sessionTranscript, isWeb bool) {
	threshold := l.autoSummarizeThreshold(isWeb)
	if threshold <= 0 || sess == nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	l.mu.Lock()
	if prev, ok := l.backgroundSummaries[key]; ok {
		prev.cancel()
	}
	l.nextBackground++
	generation := l.nextBackground
	l.backgroundSummaries[key] = backgroundSummaryTask{generation: generation, cancel: cancel}
	l.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			l.mu.Lock()
			if current, ok := l.backgroundSummaries[key]; ok && current.generation == generation {
				delete(l.backgroundSummaries, key)
			}
			l.mu.Unlock()
		}()

		// Wait until the turn that scheduled us has fully released the session.
		// A new turn cancels this context before it registers itself.
		for {
			if err := ctx.Err(); err != nil {
				return
			}
			l.mu.Lock()
			_, active := l.active[key]
			l.mu.Unlock()
			if !active {
				break
			}
			timer := time.NewTimer(25 * time.Millisecond)
			select {
			case <-ctx.Done():
				if !timer.Stop() { <-timer.C }
				return
			case <-timer.C:
			}
		}

		history := sess.GetHistory(0, 0, false, true)
		if len(history) == 0 {
			return
		}
		var toolSchemas []provider.ToolSchema
		if l.cfg.Tools != nil && l.cfg.Tools.Len() > 0 {
			toolSchemas = l.cfg.Tools.Schemas()
		}
		estimated, _ := EstimatePromptTokens(history, toolSchemas)
		// Start early enough that the next turn normally finds a ready
		// checkpoint rather than crossing the hard threshold first.
		proactive := threshold * 3 / 4
		if proactive <= 0 { proactive = threshold }
		if estimated < proactive || !l.shouldAttemptSummarize(key, estimated) {
			return
		}

		started := time.Now()
		err := l.autoSummarize(ctx, sess, false)
		l.recordSummarizeAttempt(key, estimated, err != nil)
		if l.cfg.Metrics != nil {
			l.cfg.Metrics.ObserveBackgroundSummary(time.Since(started), err == nil)
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("agent: background auto-summarize failed",
				"session", key, "tokens", estimated, "error", err)
		}
	}()
}

func (l *Loop) dispatchCommand(ctx context.Context, t Transcript, msg core.InboundMessage) (bool, *core.OutboundMessage) {
	cmd := firstWord(msg.Content)
	reply := func(s string) (bool, *core.OutboundMessage) {
		return true, &core.OutboundMessage{
			Channel: msg.Channel, ChatID: msg.ChatID, Content: s, Metadata: msg.Metadata,
		}
	}

	switch cmd {
	case "/new":
		// /new is registered as an exclusive turn before dispatch reaches here,
		// so no provider/tool/persistence path can still be mutating this session.
		t.Clear()
		if err := t.Save(); err != nil {
			return reply(fmt.Sprintf("Error: could not reset session: %v", err))
		}
		return reply("New session started.")
	case "/compact":
		st, ok := t.(sessionTranscript)
		if !ok {
			return reply("Current session store does not support compaction.")
		}
		if err := l.autoSummarize(ctx, st, false); err != nil {
			return reply(fmt.Sprintf("Compaction failed: %v", err))
		}
		return reply("Context compacted successfully.")
	case "/help":
		return reply(helpText())
	case "/stop":
		if l.cancelActive(t.Key()) {
			return reply("Stopped the active turn.")
		}
		return reply("No active turn to stop.")
	case "/status":
		return reply(fmt.Sprintf("Session: %s\nMessages: %d", t.Key(), len(t.Messages())))
	}
	return false, nil
}

func helpText() string {
	return "Available commands:\n" +
		"/new - Reset this chat and start a fresh conversation.\n" +
		"/compact - Summarize and compact conversation history.\n" +
		"/status - Show session status.\n" +
		"/stop - Cancel the active agent turn for this chat.\n" +
		"/help - Show this help."
}

func (l *Loop) registerActive(key string, cancel context.CancelFunc) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if bg, ok := l.backgroundSummaries[key]; ok {
		bg.cancel()
		delete(l.backgroundSummaries, key)
	}
	if prev, ok := l.active[key]; ok {
		prev.cancel()
	}
	l.nextGeneration++
	generation := l.nextGeneration
	l.active[key] = activeTurn{generation: generation, cancel: cancel}
	return generation
}

func (l *Loop) tryRegisterActive(key string, cancel context.CancelFunc) (uint64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if bg, ok := l.backgroundSummaries[key]; ok {
		bg.cancel()
		delete(l.backgroundSummaries, key)
	}
	if _, ok := l.active[key]; ok {
		return 0, false
	}
	l.nextGeneration++
	generation := l.nextGeneration
	l.active[key] = activeTurn{generation: generation, cancel: cancel}
	return generation, true
}

func (l *Loop) unregisterActive(key string, generation uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	current, ok := l.active[key]
	if ok && current.generation == generation {
		delete(l.active, key)
	}
}

func (l *Loop) cancelActive(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	current, ok := l.active[key]
	if ok {
		// Keep the generation registered until the canceled turn actually
		// unwinds and unregisterActive runs. Deleting it here would let a new
		// turn enter the same session while the old provider/tool/persistence
		// path is still returning, reintroducing the exact same-session race
		// the active-turn gate is meant to prevent.
		current.cancel()
	}
	return ok
}

func firstWord(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func isoLocal(t time.Time) string {
	if t.Nanosecond() == 0 {
		return t.Format("2006-01-02T15:04:05")
	}
	return t.Format("2006-01-02T15:04:05.000000")
}

func mustRawAny(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}
