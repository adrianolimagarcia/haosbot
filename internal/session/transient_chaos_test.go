package session

import (
	"path/filepath"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

func TestTransientSessionNeverTouchesDurableStore(t *testing.T) {
	root := t.TempDir()
	durable, err := NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := durable.List()
	if err != nil {
		t.Fatal(err)
	}

	transient := NewTransientStore()
	key := "webui:tmp_chaos_non_persistent"
	if !IsTransientKey(key) {
		t.Fatalf("%q must be recognized as transient", key)
	}
	s, err := transient.Open(key)
	if err != nil {
		t.Fatal(err)
	}
	s.AddMessage(core.Message{Role: core.RoleUser, Content: core.TextContent("ephemeral")})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if got := len(s.Messages()); got != 1 {
		t.Fatalf("transient messages=%d want 1", got)
	}

	after, err := durable.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("durable session count changed: before=%d after=%d", len(before), len(after))
	}
	transient.Delete(key)
	if transient.Count() != 0 {
		t.Fatal("transient session survived explicit cleanup")
	}
}

func TestTransientStoreConcurrentOpenIsDeduplicated(t *testing.T) {
	store := NewTransientStore()
	const workers = 32
	out := make(chan *TransientSession, workers)
	for i := 0; i < workers; i++ {
		go func() {
			s, err := store.Open("webui:tmp_same")
			if err != nil {
				t.Errorf("Open: %v", err)
				return
			}
			out <- s
		}()
	}
	var first *TransientSession
	for i := 0; i < workers; i++ {
		s := <-out
		if first == nil {
			first = s
		} else if s != first {
			t.Fatal("concurrent Open returned duplicate transient sessions")
		}
	}
	if store.Count() != 1 {
		t.Fatalf("store count=%d want 1", store.Count())
	}
}
