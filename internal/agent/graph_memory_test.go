package agent

import (
	"context"
	"path/filepath"
	"testing"

	micrographrag "github.com/adrianolimagarcia/micrographrag-go"
)

// TestGraphMemoryPinContextLifetime pins down the contract a store provider
// relies on. GraphMemoryForSession has no room for a release value, so
// cmd/haosbot's graphStorePool binds the pin of a store to the lifetime of the
// context it is handed: the store stays usable while that context is alive and
// is released when it is cancelled. The loop must therefore pass a context that
// (a) survives the cancellation of the request and (b) is cancelled as soon as
// the loop is done with the store. Breaking either half leaks a store forever
// or lets an eviction close a store that is still being read.
func TestGraphMemoryPinContextLifetime(t *testing.T) {
	dir := t.TempDir()
	cfg := micrographrag.DefaultConfig(filepath.Join(dir, "pin.db"))
	cfg.EnableVector = false
	cfg.EnableEmbeddingWorker = false
	// FTS off so the test does not need the sqlite_fts5 build tag; the pin
	// contract does not depend on the index.
	cfg.EnableFTS = false
	store, err := micrographrag.Open(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	var (
		pinCtx        context.Context
		errWhileInUse error
	)
	turnCtx, cancelTurn := context.WithCancel(context.Background())
	defer cancelTurn()

	loop := &Loop{cfg: LoopConfig{
		GraphMemoryForSession: func(ctx context.Context, key string) (*micrographrag.Store, error) {
			pinCtx = ctx
			// The request is cancelled while the store is still in use: the pin
			// must survive that, or an eviction could close the store mid-read.
			cancelTurn()
			errWhileInUse = ctx.Err()
			return store, nil
		},
	}}

	loop.graphMemoryRetrieve(turnCtx, "session-pin", "query")

	if errWhileInUse != nil {
		t.Fatalf("cancelling the turn context also cancelled the store pin: %v", errWhileInUse)
	}
	if pinCtx == nil {
		t.Fatal("GraphMemoryForSession was not called")
	}
	if pinCtx.Err() == nil {
		t.Fatal("the store pin was not released after the retrieval finished")
	}
}
