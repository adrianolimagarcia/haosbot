package memory

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/adrianolimagarcia/nanobot-go/internal/bpe"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/session"
)

// MemoryArchiver writes durable transcript batches to the Memory ingestion journal.
// Port of MemoryArchiver (memory.py:758-1071).
type MemoryArchiver struct {
	store                *MemoryStore
	buildMessages        BuildMessagesFunc
	getToolDefinitions   GetToolDefinitionsFunc
	resolvePromptContext ResolvePromptContextFunc
}

// NewMemoryArchiver creates a new MemoryArchiver.
func NewMemoryArchiver(
	store *MemoryStore,
	buildMessages BuildMessagesFunc,
	getToolDefinitions GetToolDefinitionsFunc,
	resolvePromptContext ResolvePromptContextFunc,
) *MemoryArchiver {
	return &MemoryArchiver{
		store:                store,
		buildMessages:        buildMessages,
		getToolDefinitions:   getToolDefinitions,
		resolvePromptContext: resolvePromptContext,
	}
}

func (a *MemoryArchiver) _rawCheckpoint(
	messages []map[string]any,
	sessionKey string,
	previousSummary *string,
	maxTokens int,
) (string, error) {
	raw, err := a.store.RawArchive(messages, nil, sessionKey)
	if err != nil {
		return "", err
	}
	return combineRawCheckpoint(raw, previousSummary, maxTokens), nil
}

func combineRawCheckpoint(raw string, previousSummary *string, maxTokens int) string {
	tokenLimit := max(1, maxTokens)
	if previousSummary == nil || *previousSummary == "" {
		return bpe.TruncateTextToTokens(raw, tokenLimit)
	}

	combined := fmt.Sprintf(
		"[Previous archived context]\n%s\n\n[Newly archived raw context]\n%s",
		*previousSummary,
		raw,
	)
	bounded := bpe.TruncateTextToTokens(combined, tokenLimit)
	if bounded == combined {
		return combined
	}

	sectionLimit := max(1, (tokenLimit-32)/2)
	prevTrunc := bpe.TruncateTextToTokens(*previousSummary, sectionLimit)
	rawTrunc := bpe.TruncateTextToTokens(raw, sectionLimit)

	sectionCombined := fmt.Sprintf(
		"[Previous archived context]\n%s\n\n[Newly archived raw context]\n%s",
		prevTrunc,
		rawTrunc,
	)
	return bpe.TruncateTextToTokens(sectionCombined, tokenLimit)
}

func (a *MemoryArchiver) Archive(
	ctx context.Context,
	sourceMessages []map[string]any,
	opts ArchiveOptions,
) (*string, error) {
	if len(sourceMessages) == 0 {
		return nil, nil
	}

	rawFallback := func() (string, error) {
		maxT := 4096
		if opts.FallbackMaxTokens != nil {
			maxT = *opts.FallbackMaxTokens
		}
		prev := opts.PreviousSummary
		return a._rawCheckpoint(sourceMessages, opts.SessionKey, prev, maxT)
	}

	if opts.ProviderState != nil {
		// Documented gap: provider-native compaction is not ported.
		// Route to raw fallback when provider state is present.
		fb, err := rawFallback()
		if err != nil {
			return nil, err
		}
		return &fb, nil
	}

	channel := ""
	for i := 0; i < len(opts.SessionKey); i++ {
		if opts.SessionKey[i] == ':' {
			channel = opts.SessionKey[:i]
			break
		}
	}

	var sessionSummary *session.SessionSummary
	if opts.PreviousSummary != nil {
		sessionSummary = &session.SessionSummary{Text: *opts.PreviousSummary}
	}

	probeMessages := a.buildMessages(BuildMessagesOptions{
		History:        opts.History,
		CurrentMessage: "[archive]",
		Channel:        &channel,
		SessionSummary: sessionSummary,
	})

	tools := opts.RequestTools
	if tools == nil {
		tools = a.getToolDefinitions()
	}

	// Build tool schemas for provider
	var schemas []provider.ToolSchema
	for _, t := range tools {
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

	promptMessages := append([]map[string]any{}, probeMessages...)
	promptMessages = append(promptMessages, sourceMessages...)

	// Convert map messages to core.Message
	var coreMsgs []core.Message
	for _, m := range promptMessages {
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		coreMsgs = append(coreMsgs, core.Message{
			Role:    core.Role(role),
			Content: core.TextContent(content),
		})
	}

	if opts.InputTokenBudget != nil {
		_ = *opts.InputTokenBudget
	}

	resp, err := opts.Runtime.Provider.Chat(ctx, provider.ChatRequest{
		Messages:    coreMsgs,
		Tools:       schemas,
		Model:       opts.Runtime.Model,
		MaxTokens:   opts.Runtime.Generation.MaxTokens,
		Temperature: opts.Runtime.Generation.Temperature,
	})
	if err != nil {
		fb, fbErr := rawFallback()
		if fbErr != nil {
			return nil, err
		}
		return &fb, nil
	}

	if resp.FinishReason == core.FinishError || resp.FinishReason == core.FinishLength || len(resp.ToolCalls) > 0 {
		fb, err := rawFallback()
		if err != nil {
			return nil, err
		}
		return &fb, nil
	}

	summary := resp.Content
	if summary == "" {
		fb, err := rawFallback()
		if err != nil {
			return nil, err
		}
		return &fb, nil
	}

	normalized := a.store.normalizeHistoryEntry(summary, nil)
	if normalized == "" {
		fb, err := rawFallback()
		if err != nil {
			return nil, err
		}
		return &fb, nil
	}

	return &normalized, nil
}

func (a *MemoryArchiver) ArchiveSession(
	ctx context.Context,
	sess *session.Session,
	archiveEnd int,
	runtime LLMRuntime,
	inputTokenBudget int,
) (*string, error) {
	msgs := sess.Messages()
	if archiveEnd > len(msgs) {
		archiveEnd = len(msgs)
	}
	lastArchived := sess.LastArchived()
	if lastArchived < 0 {
		lastArchived = 0
	}

	var sourceMessages []map[string]any
	for i := lastArchived; i < archiveEnd && i < len(msgs); i++ {
		m := msgs[i]
		if session.IsSummaryCheckpoint(m) {
			continue
		}
		sourceMessages = append(sourceMessages, map[string]any{
			"role":    string(m.Role),
			"content": m.Content.Text,
		})
	}

	if len(sourceMessages) == 0 {
		return nil, nil
	}

	sessionSummary := session.SessionSummaryFromMetadata(sess.Metadata(), sess.UpdatedAt())
	var prev *string
	if sessionSummary != nil && sessionSummary.Text != "" {
		prev = &sessionSummary.Text
	}

	historyMsgs := sess.GetHistory(0, 0, false, false)
	var historyMaps []map[string]any
	for _, hm := range historyMsgs {
		historyMaps = append(historyMaps, map[string]any{
			"role":    string(hm.Role),
			"content": hm.Content.Text,
		})
	}

	return a.Archive(ctx, sourceMessages, ArchiveOptions{
		Runtime:          runtime,
		SessionKey:       sess.Key(),
		History:          historyMaps,
		RequestTools:     a.getToolDefinitions(),
		PreviousSummary:  prev,
		InputTokenBudget: &inputTokenBudget,
	})
}
