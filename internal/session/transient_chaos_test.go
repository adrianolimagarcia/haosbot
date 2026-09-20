package session

import (
	"os"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

func TestTransientSessionNeverTouchesFilesystem(t *testing.T) {
	root := t.TempDir()
	old, err := os.Getwd()
	if err != nil { t.Fatal(err) }
	if err := os.Chdir(root); err != nil { t.Fatal(err) }
	t.Cleanup(func(){ _ = os.Chdir(old) })

	transient := NewTransientStore()
	key := "webui:tmp_chaos_non_persistent"
	if !IsTransientKey(key) { t.Fatalf("%q must be recognized as transient", key) }
	s, err := transient.Open(key)
	if err != nil { t.Fatal(err) }
	s.AddMessage(core.Message{Role: core.RoleUser, Content: core.TextContent("ephemeral")})
	if err := s.Save(); err != nil { t.Fatal(err) }
	if err := s.AppendMessagesDurable([]core.Message{{Role: core.RoleAssistant, Content: core.TextContent("still ephemeral")}}); err != nil { t.Fatal(err) }
	if got := len(s.Messages()); got != 2 { t.Fatalf("transient messages=%d want 2", got) }
	entries, err := os.ReadDir(root)
	if err != nil { t.Fatal(err) }
	if len(entries) != 0 { t.Fatalf("transient Save created filesystem entries: %v", entries) }
	transient.Delete(key)
	if transient.Count() != 0 { t.Fatal("transient session survived explicit cleanup") }
}

func TestTransientStoreConcurrentOpenIsDeduplicated(t *testing.T) {
	store := NewTransientStore()
	const workers = 32
	out := make(chan *TransientSession, workers)
	for i := 0; i < workers; i++ {
		go func() {
			s, err := store.Open("webui:tmp_same")
			if err != nil { t.Errorf("Open: %v", err); return }
			out <- s
		}()
	}
	var first *TransientSession
	for i := 0; i < workers; i++ {
		s := <-out
		if first == nil { first = s } else if s != first { t.Fatal("concurrent Open returned duplicate transient sessions") }
	}
	if store.Count() != 1 { t.Fatalf("store count=%d want 1", store.Count()) }
}
