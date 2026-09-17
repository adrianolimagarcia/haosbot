package memory

import (
	"context"

	"github.com/adrianolimagarcia/nanobot-go/internal/provider"
	"github.com/adrianolimagarcia/nanobot-go/internal/session"
)

// This file freezes the shared contract for the Memory checkpoint subsystem so
// that MemoryArchiver and Consolidator can be implemented against it
// independently. The shapes mirror the reference callables in
// MemoryArchiver.__init__ (memory.py:766-776) and Consolidator.__init__
// (memory.py:1077-1098).

// GenerationSettings mirrors GenerationSettings (providers/base.py:612-617).
// The reference defaults live in provider.DefaultTemperature/DefaultMaxTokens.
type GenerationSettings struct {
	Temperature     float64
	MaxTokens       int
	ReasoningEffort string
}

// LLMRuntime is one captured provider/model configuration used for an entire
// execution. Port of LLMRuntime (utils/llm_runtime.py:14-28).
//
// The reference freezes mutable selection and generation values into this
// value; consumers must use these fields instead of consulting
// provider.generation after admission. The port's Provider interface has no
// generation accessor, so Capture takes the settings explicitly instead of
// reading them off the provider.
type LLMRuntime struct {
	Provider            provider.Provider
	Model               string
	Generation          GenerationSettings
	ContextWindowTokens int
	// ModelPreset is the reference's model_preset; empty string is None.
	ModelPreset string
	// SnapshotSignature is the reference's snapshot_signature tuple; nil is
	// None.
	SnapshotSignature []any
}

// BuildMessagesOptions carries the keyword arguments the reference passes to
// its build_messages callable (memory.py:1181-1189).
type BuildMessagesOptions struct {
	History        []map[string]any
	CurrentMessage string
	Channel        *string
	SessionSummary *session.SessionSummary
}

// BuildMessagesFunc mirrors the reference's “build_messages“ callable.
type BuildMessagesFunc func(opts BuildMessagesOptions) []map[string]any

// GetToolDefinitionsFunc mirrors the reference's get_tool_definitions callable.
type GetToolDefinitionsFunc func() []map[string]any

// ResolvePromptContextFunc mirrors the reference's optional
// resolve_prompt_context callable, which returns (prompt, path) for a session.
// A nil result path is the empty string.
type ResolvePromptContextFunc func(sess *session.Session) (prompt *string, path string, err error)

// SessionManager is the session access the Consolidator needs
// (memory.py:1085-1086, 1233-1234).
type SessionManager interface {
	// GetOrCreate returns the session for key, creating it if needed.
	GetOrCreate(key string) (*session.Session, error)
	// Invalidate drops any cached in-memory state for key.
	Invalidate(key string)
}

// StoreSessions adapts *session.Store to SessionManager.
type StoreSessions struct {
	Store *session.Store
}

// GetOrCreate opens (creating if absent) the session for key.
func (s StoreSessions) GetOrCreate(key string) (*session.Session, error) {
	return s.Store.Open(key)
}

// Invalidate is a no-op: this port's Store holds no in-memory session cache to
// drop, so there is nothing to invalidate. The reference calls
// SessionManager.invalidate to drop a cached Session object before re-reading
// it; re-reading from disk here is equivalent because Open always reloads.
func (s StoreSessions) Invalidate(string) {}

// ProviderConversationState is the provider-native compaction payload
// (ProviderConversationState). The provider-native compaction subsystem is not
// ported, so the memory subsystem only ever receives nil and routes to the raw
// fallback; the type exists so the tri-state plumbing matches the reference.
type ProviderConversationState struct {
	// State is the opaque provider-native payload. nil means absent.
	State any
}

// ArchiveOptions carries the keyword arguments of MemoryArchiver.archive
// (memory.py:826-841).
type ArchiveOptions struct {
	Runtime           LLMRuntime
	SessionKey        string
	History           []map[string]any
	RequestTools      []map[string]any
	PreviousSummary   *string
	InputTokenBudget  *int
	FallbackMaxTokens *int
	ProviderState     *ProviderConversationState
}

// Archiver is the surface the Consolidator consumes. Declaring it as an
// interface lets the Consolidator and the MemoryArchiver be implemented and
// tested independently: the Consolidator takes this interface, and the
// MemoryArchiver satisfies it.
type Archiver interface {
	// Archive appends the archive prompt to H and persists its summary
	// (memory.py:826). A nil result with a nil error is the reference's None.
	Archive(ctx context.Context, sourceMessages []map[string]any, opts ArchiveOptions) (*string, error)

	// ArchiveSession archives a captured session prefix without mutating the
	// session (memory.py:996).
	ArchiveSession(
		ctx context.Context,
		sess *session.Session,
		archiveEnd int,
		runtime LLMRuntime,
		inputTokenBudget int,
	) (*string, error)
}
