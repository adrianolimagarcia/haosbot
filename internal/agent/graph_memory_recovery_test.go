package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/memoryfabric"
)

// recoveryTranscript is a Transcript that also implements
// transcriptMessageMutator, so reconcilePendingGraphMemory acts on it.
// The fakes in loop_test.go implement neither List nor SetMessage, and
// without both the recovery path returns early without doing anything.
type recoveryTranscript struct {
	key      string
	messages []core.Message
	saves    int
	saveErr  error
}

func (t *recoveryTranscript) Key() string              { return t.key }
func (t *recoveryTranscript) Messages() []core.Message { return t.messages }
func (t *recoveryTranscript) AddMessage(m core.Message) {
	t.messages = append(t.messages, m)
}
func (t *recoveryTranscript) Clear() { t.messages = nil }
func (t *recoveryTranscript) Save() error {
	t.saves++
	return t.saveErr
}
func (t *recoveryTranscript) SetMessage(i int, m core.Message) error {
	if i < 0 || i >= len(t.messages) {
		return fmt.Errorf("SetMessage: index %d out of range", i)
	}
	t.messages[i] = m
	return nil
}

type recoveryStore struct {
	transcripts map[string]*recoveryTranscript
}

func (s *recoveryStore) List() ([]string, error) {
	keys := make([]string, 0, len(s.transcripts))
	for k := range s.transcripts {
		keys = append(keys, k)
	}
	return keys, nil
}

func (s *recoveryStore) Open(key string) (Transcript, error) {
	t, ok := s.transcripts[key]
	if !ok {
		return nil, fmt.Errorf("recoveryStore: unknown session %q", key)
	}
	return t, nil
}

func pendingMessage(turnID, sessionKey, content string) core.Message {
	raw, err := json.Marshal(graphMemoryPending{TurnID: turnID, SessionKey: sessionKey, Content: content})
	if err != nil {
		panic(err)
	}
	m := core.NewMessage(core.RoleUser, "heartbeat")
	m.SetExtra(graphMemoryPendingExtra, raw)
	return *m
}

func hasPendingMarker(m core.Message) bool {
	_, ok := m.Extra(graphMemoryPendingExtra)
	return ok
}

// captureSlog redirects the default slog logger to a buffer for the test.
// reconcilePendingGraphMemory logs through the package-level slog, so the
// test has to swap the default and restore it afterwards.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestRecoverPendingGraphMemorySkipsConflict(t *testing.T) {
	store := &recoveryStore{transcripts: map[string]*recoveryTranscript{
		"session-1": {
			key: "session-1",
			messages: []core.Message{
				pendingMessage("turn-conflict", "session-1", "second version"),
				pendingMessage("turn-good", "session-1", "good version"),
			},
		},
	}}
	loop := newTestLoop(t, store, nil)
	var enqueued []string
	loop.cfg.GraphMemoryEnqueueWithIDError = func(turnID, sessionKey, content string) error {
		enqueued = append(enqueued, turnID)
		if turnID == "turn-conflict" {
			// Production shape: AppendTurn wraps ErrRecordConflict with %w.
			return fmt.Errorf("memoryfabric: record %s already exists with different identity or content: %w", turnID, memoryfabric.ErrRecordConflict)
		}
		return nil
	}
	logs := captureSlog(t)

	if err := loop.RecoverPendingGraphMemory(); err != nil {
		t.Fatalf("conflicting job must not fail recovery: %v", err)
	}
	tr := store.transcripts["session-1"]
	if hasPendingMarker(tr.messages[0]) {
		t.Fatal("conflicting marker was not dropped")
	}
	if hasPendingMarker(tr.messages[1]) {
		t.Fatal("reconciled marker was not cleared")
	}
	if tr.saves != 1 {
		t.Fatalf("expected exactly one Save after dropping markers, got %d", tr.saves)
	}
	if len(enqueued) != 2 || enqueued[0] != "turn-conflict" || enqueued[1] != "turn-good" {
		t.Fatalf("expected both jobs attempted in order, got %v", enqueued)
	}
	if !strings.Contains(logs.String(), "skipping conflicting GraphRAG job during recovery; marker dropped") {
		t.Fatalf("expected skip WARN in logs, got: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "turn-conflict") {
		t.Fatalf("expected WARN to name the conflicting turn, got: %s", logs.String())
	}
}

func TestRecoverPendingGraphMemoryKeepsOtherErrorsFatal(t *testing.T) {
	store := &recoveryStore{transcripts: map[string]*recoveryTranscript{
		"session-1": {
			key: "session-1",
			messages: []core.Message{
				pendingMessage("turn-io", "session-1", "content"),
			},
		},
	}}
	loop := newTestLoop(t, store, nil)
	loop.cfg.GraphMemoryEnqueueWithIDError = func(turnID, sessionKey, content string) error {
		return errors.New("simulated I/O failure")
	}

	err := loop.RecoverPendingGraphMemory()
	if err == nil {
		t.Fatal("non-conflict error must still fail recovery")
	}
	if !strings.Contains(err.Error(), "agent: recover GraphRAG job turn-io") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasPendingMarker(store.transcripts["session-1"].messages[0]) {
		t.Fatal("marker of a failed non-conflict job must be retained for retry")
	}
}
