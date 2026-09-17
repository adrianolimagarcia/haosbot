package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/adrianolimagarcia/nanobot-go/internal/agent"
	"github.com/adrianolimagarcia/nanobot-go/internal/bpe"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/events"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/session"
)

const safetyBuffer = 1024

// Consolidator coordinates session Memory checkpoints through an Archiver.
// Port of Consolidator (memory.py:1072-1297).
type Consolidator struct {
	store                *MemoryStore
	sessions             SessionManager
	buildMessages        BuildMessagesFunc
	getToolDefinitions   GetToolDefinitionsFunc
	resolvePromptContext ResolvePromptContextFunc
	archiver             Archiver

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

// NewConsolidator creates a new Consolidator.
func NewConsolidator(
	store *MemoryStore,
	sessions SessionManager,
	buildMessages BuildMessagesFunc,
	getToolDefinitions GetToolDefinitionsFunc,
	resolvePromptContext ResolvePromptContextFunc,
) *Consolidator {
	return &Consolidator{
		store:                store,
		sessions:             sessions,
		buildMessages:        buildMessages,
		getToolDefinitions:   getToolDefinitions,
		resolvePromptContext: resolvePromptContext,
		archiver: NewMemoryArchiver(
			store,
			buildMessages,
			getToolDefinitions,
			resolvePromptContext,
		),
		locks: make(map[string]*sync.Mutex),
	}
}

// SetArchiver sets or overrides the Archiver implementation.
func (c *Consolidator) SetArchiver(a Archiver) {
	c.archiver = a
}

// GetLock returns the shared consolidation lock for one session.
func (c *Consolidator) GetLock(sessionKey string) *sync.Mutex {
	c.locksMu.Lock()
	defer c.locksMu.Unlock()
	lk, exists := c.locks[sessionKey]
	if !exists {
		lk = &sync.Mutex{}
		c.locks[sessionKey] = lk
	}
	return lk
}

func (c *Consolidator) _fullReplyHistory(sess *session.Session) []map[string]any {
	if sess == nil {
		return nil
	}
	msgs := sess.Messages()
	var maps []map[string]any
	for _, m := range msgs {
		maps = append(maps, map[string]any{
			"role":    string(m.Role),
			"content": m.Content.Text,
		})
	}
	return maps
}

func (c *Consolidator) EstimateSessionPromptTokens(sess *session.Session, runtime LLMRuntime) (int, string) {
	if sess == nil {
		return 0, ""
	}
	history := c._fullReplyHistory(sess)
	var channel *string
	for i := 0; i < len(sess.Key()); i++ {
		if sess.Key()[i] == ':' {
			ch := sess.Key()[:i]
			channel = &ch
			break
		}
	}
	summary := session.SessionSummaryFromMetadata(sess.Metadata(), sess.UpdatedAt())
	probeMessages := c.buildMessages(BuildMessagesOptions{
		History:        history,
		CurrentMessage: "[token-probe]",
		Channel:        channel,
		SessionSummary: summary,
	})

	var coreMsgs []core.Message
	for _, m := range probeMessages {
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		coreMsgs = append(coreMsgs, core.Message{
			Role:    core.Role(role),
			Content: core.TextContent(content),
		})
	}

	// Convert tool definitions to provider.ToolSchema
	var schemas []provider.ToolSchema
	for _, t := range c.getToolDefinitions() {
		name, _ := t["name"].(string)
		desc, _ := t["description"].(string)
		var rawParams json.RawMessage
		if p, ok := t["parameters"]; ok {
			if b, err := json.Marshal(p); err == nil {
				rawParams = b
			}
		}
		schemas = append(schemas, provider.ToolSchema{
			Name:        name,
			Description: desc,
			Parameters:  rawParams,
		})
	}

	return agent.EstimatePromptTokensChain(
		runtime.Provider,
		runtime.Model,
		coreMsgs,
		schemas,
	)
}

func (c *Consolidator) InputTokenBudget(runtime LLMRuntime) int {
	maxOutput := max(0, runtime.Generation.MaxTokens)
	return runtime.ContextWindowTokens - maxOutput - safetyBuffer
}

func (c *Consolidator) SummarizeTranscript(
	ctx context.Context,
	acceptedMessages []map[string]any,
	previousSummary *string,
	runtime LLMRuntime,
	sessionKey string,
	tools []map[string]any,
	providerState *ProviderConversationState,
) (*string, error) {
	var sourceMessages []map[string]any
	for _, m := range acceptedMessages {
		role, _ := m["role"].(string)
		if role != "system" {
			sourceMessages = append(sourceMessages, m)
		}
	}
	if len(sourceMessages) == 0 {
		return nil, nil
	}

	maxOutputTokens := max(0, runtime.Generation.MaxTokens)
	inputTokenBudget := runtime.ContextWindowTokens - maxOutputTokens
	checkpointTokens := min(
		maxOutputTokens,
		max(1, (inputTokenBudget-safetyBuffer)/2),
	)

	fallbackMax := max(1, checkpointTokens)
	summary, err := c.archiver.Archive(ctx, sourceMessages, ArchiveOptions{
		Runtime:           runtime,
		SessionKey:        sessionKey,
		History:           acceptedMessages,
		RequestTools:      tools,
		PreviousSummary:   previousSummary,
		InputTokenBudget:  &inputTokenBudget,
		FallbackMaxTokens: &fallbackMax,
		ProviderState:     providerState,
	})
	if err != nil || summary == nil {
		return nil, err
	}

	truncated := bpe.TruncateTextToTokens(*summary, max(1, maxOutputTokens))
	return &truncated, nil
}

func (c *Consolidator) SummarizeProviderCompaction(
	ctx context.Context,
	state ProviderConversationState,
	fallbackMessages []map[string]any,
	previousSummary *string,
	runtime LLMRuntime,
	sessionKey string,
	tools []map[string]any,
) (*string, error) {
	return c.SummarizeTranscript(
		ctx,
		fallbackMessages,
		previousSummary,
		runtime,
		sessionKey,
		tools,
		&state,
	)
}

func (c *Consolidator) ArchiveSession(
	ctx context.Context,
	sess *session.Session,
	archiveEnd int,
	runtime LLMRuntime,
) (*string, error) {
	return c.archiver.ArchiveSession(
		ctx,
		sess,
		archiveEnd,
		runtime,
		c.InputTokenBudget(runtime),
	)
}

func (c *Consolidator) CompactIdleSession(
	ctx context.Context,
	sessionKey string,
	runtime LLMRuntime,
	maxSuffix int,
	sink events.EventSink,
) (*string, error) {
	lock := c.GetLock(sessionKey)
	lock.Lock()
	defer lock.Unlock()

	c.sessions.Invalidate(sessionKey)
	sess, err := c.sessions.GetOrCreate(sessionKey)
	if err != nil {
		return nil, err
	}

	archiveStart := sess.LastArchived()
	msgs := sess.Messages()
	if archiveStart >= len(msgs) {
		empty := ""
		return &empty, nil // No new messages
	}

	// Emit compaction started event if sink accepts it
	compactionID := fmt.Sprintf("compaction-%d", len(msgs))
	if sink.Accepts(events.ContextCompactionEvent{}) {
		sink.Emit(events.ContextCompactionEvent{
			CompactionID: compactionID,
			Phase:        events.CompactionStarted,
		})
	}

	summaryPtr, err := c.ArchiveSession(ctx, sess, len(msgs), runtime)
	if err != nil || summaryPtr == nil {
		if sink.Accepts(events.ContextCompactionEvent{}) {
			sink.Emit(events.ContextCompactionEvent{
				CompactionID: compactionID,
				Phase:        events.CompactionFailed,
			})
		}
		return nil, err
	}

	summary := *summaryPtr
	if summary == "" {
		if sink.Accepts(events.ContextCompactionEvent{}) {
			sink.Emit(events.ContextCompactionEvent{
				CompactionID: compactionID,
				Phase:        events.CompactionFailed,
			})
		}
		return nil, nil
	}

	// Commit summary checkpoint
	lastActive := sess.UpdatedAt()
	sess.CommitSummaryCheckpoint(summary, nil, &lastActive)
	if err := sess.Save(); err != nil {
		if sink.Accepts(events.ContextCompactionEvent{}) {
			sink.Emit(events.ContextCompactionEvent{
				CompactionID: compactionID,
				Phase:        events.CompactionFailed,
			})
		}
		return nil, err
	}

	if sink.Accepts(events.ContextCompactionEvent{}) {
		sink.Emit(events.ContextCompactionEvent{
			CompactionID: compactionID,
			Phase:        events.CompactionSucceeded,
		})
	}

	return &summary, nil
}
