package session

import (
	"fmt"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

func TestTransientStoreEvictsIdleSessions(t *testing.T) {
	store := NewTransientStore()
	stale, err := store.Open("webui:tmp_stale")
	if err != nil {
		t.Fatal(err)
	}
	stale.AddMessage(core.Message{Role: core.RoleUser, Content: core.TextContent("antigo")})

	// Age the session past the TTL without sleeping for hours.
	stale.mu.Lock()
	stale.updatedAt = time.Now().Add(-2 * transientTTL)
	stale.mu.Unlock()

	if _, err := store.Open("webui:tmp_fresh"); err != nil {
		t.Fatal(err)
	}
	if n := store.Count(); n != 1 {
		t.Fatalf("Count()=%d, want the idle session evicted", n)
	}
	if store.sessions["webui:tmp_stale"] != nil {
		t.Fatal("idle session survived eviction")
	}
}

func TestTransientStoreCapsSessionCount(t *testing.T) {
	store := NewTransientStore()
	for i := 0; i < maxTransientSessions+20; i++ {
		if _, err := store.Open(fmt.Sprintf("webui:tmp_%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if n := store.Count(); n > maxTransientSessions {
		t.Fatalf("Count()=%d, want at most %d", n, maxTransientSessions)
	}
}

// At capacity, opening a session must not evict the session being opened, and
// the most recently used ones must be the ones that survive.
func TestTransientStoreOpenKeepsTheRequestedSession(t *testing.T) {
	store := NewTransientStore()
	for i := 0; i < maxTransientSessions; i++ {
		if _, err := store.Open(fmt.Sprintf("webui:tmp_%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	recent, err := store.Open("webui:tmp_000")
	if err != nil {
		t.Fatal(err)
	}
	again, err := store.Open("webui:tmp_000")
	if err != nil {
		t.Fatal(err)
	}
	if again != recent {
		t.Fatal("Open returned a different session for the same key")
	}
	if store.sessions["webui:tmp_000"] == nil {
		t.Fatal("the session being opened was evicted by its own Open")
	}
	if n := store.Count(); n > maxTransientSessions {
		t.Fatalf("Count()=%d, want at most %d", n, maxTransientSessions)
	}
}

func TestTransientStoreOpenRefreshesIdleTimer(t *testing.T) {
	store := NewTransientStore()
	s, err := store.Open("webui:tmp_active")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.updatedAt = time.Now().Add(-transientTTL + time.Minute)
	s.mu.Unlock()

	if _, err := store.Open("webui:tmp_active"); err != nil {
		t.Fatal(err)
	}
	if n := store.Count(); n != 1 {
		t.Fatalf("Count()=%d, want the touched session retained", n)
	}
}
