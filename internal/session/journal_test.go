package session

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

func TestAppendMessagesDurableReplaysJournalAndCompacts(t *testing.T) {
	root := t.TempDir()
	store := NewStore("", root)
	sess, err := store.Open("webui:test-session-1234567890")
	if err != nil { t.Fatal(err) }

	first := *core.NewMessage(core.RoleUser, "hello")
	if err := sess.AppendMessagesDurable([]core.Message{first}); err != nil { t.Fatal(err) }
	if _, err := os.Stat(store.journalPath(sess.Key())); err != nil { t.Fatalf("journal missing: %v", err) }

	// A fresh store proves recovery does not depend on the in-memory object.
	reopenedStore := NewStore("", root)
	reopened, err := reopenedStore.Open(sess.Key())
	if err != nil { t.Fatal(err) }
	if got := reopened.Messages(); len(got) != 1 || got[0].Content.Text != "hello" {
		t.Fatalf("journal replay mismatch: %#v", got)
	}

	if err := reopened.Save(); err != nil { t.Fatal(err) }
	if _, err := os.Stat(reopenedStore.journalPath(sess.Key())); !os.IsNotExist(err) {
		t.Fatalf("journal should be removed after compaction, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(reopenedStore.Dir(), StorageKey(sess.Key())+sessionSuffix)); err != nil {
		t.Fatalf("canonical session missing: %v", err)
	}
}

func TestListIncludesJournalOnlySession(t *testing.T) {
	store := NewStore("", t.TempDir())
	key := "webui:journal-only-1234567890"
	sess, err := store.Open(key)
	if err != nil { t.Fatal(err) }
	if err := sess.AppendMessagesDurable([]core.Message{*core.NewMessage(core.RoleUser, "x")}); err != nil { t.Fatal(err) }

	keys, err := store.List()
	if err != nil { t.Fatal(err) }
	if len(keys) != 1 || keys[0] != key {
		t.Fatalf("List()=%v, want [%s]", keys, key)
	}
}


func TestJournalReplaySkipsCanonicalOverlap(t *testing.T) {
	root := t.TempDir()
	store := NewStore("", root)
	key := "webui:journal-overlap-1234567890"
	sess, err := store.Open(key)
	if err != nil { t.Fatal(err) }

	first := *core.NewMessage(core.RoleUser, "first")
	first.Timestamp = "2026-09-19T10:00:00"
	second := *core.NewMessage(core.RoleAssistant, "second")
	second.Timestamp = "2026-09-19T10:00:01"
	if err := sess.AppendMessagesDurable([]core.Message{first, second}); err != nil { t.Fatal(err) }
	if err := sess.Save(); err != nil { t.Fatal(err) }

	third := *core.NewMessage(core.RoleUser, "third")
	third.Timestamp = "2026-09-19T10:00:02"
	var journalBytes bytes.Buffer
	for _, m := range []core.Message{second, third} {
		msg := m
		if err := encodeMessage(&journalBytes, &msg); err != nil { t.Fatal(err) }
		journalBytes.WriteByte('\n')
	}
	if err := os.WriteFile(store.journalPath(key), journalBytes.Bytes(), 0o600); err != nil { t.Fatal(err) }

	fresh := NewStore("", root)
	reopened, err := fresh.Open(key)
	if err != nil { t.Fatal(err) }
	got := reopened.Messages()
	if len(got) != 3 {
		t.Fatalf("message count=%d want 3: %#v", len(got), got)
	}
	if got[0].Content.Text != "first" || got[1].Content.Text != "second" || got[2].Content.Text != "third" {
		t.Fatalf("unexpected journal overlap replay: %#v", got)
	}
}
