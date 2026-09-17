package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	micrographrag "github.com/adrianolimagarcia/micrographrag-go"
)

// TestGraphMemoryContextRetrievesStoredMemory is the regression test for a
// SILENT no-op in the retrieved-memory path.
//
// graphMemoryContext used to call
//
//	store.Search(ctx, query, micrographrag.SearchOptions{Limit: 6})
//
// and micrographrag's normalizeSearchOptions (search.go:27-58) fills in Limit,
// FTSLimit, GraphDepth and the weights from the store defaults but NEVER copies
// EnableFTS, EnableVector or EnableGraph — it just returns the caller's struct.
// Search only consults an engine whose flag is set, so a non-zero options struct
// disabled every engine and returned zero results with a nil error. Measured
// against a store holding one matching memory:
//
//	SearchOptions{}                        -> 1 result
//	SearchOptions{Limit: 6}                -> 0 results, nil error
//	SearchOptions{Limit: 6, EnableFTS: true} -> 1 result
//
// The consequence was that the agent's retrieved-memory block was always empty
// and nothing ever reported a failure — the headline feature of this port did
// nothing at retrieval time.
//
// This test therefore asserts on the OUTPUT of the production function, not on
// the options it builds, so it keeps failing if the call is ever "simplified"
// back to a literal.
func TestGraphMemoryContextRetrievesStoredMemory(t *testing.T) {
	ctx := context.Background()
	store, err := micrographrag.Open(ctx, micrographrag.Config{
		DBPath:    filepath.Join(t.TempDir(), "graph.db"),
		EnableFTS: true,
	}, nil)
	if err != nil {
		// The FTS engine needs the sqlite_fts5 build tag. Skip rather than fail
		// in an untagged run; the CI job that builds production binaries passes
		// the tag.
		t.Skipf("micrographrag unavailable without -tags sqlite_fts5: %v", err)
	}
	defer store.Close()

	const needle = "alpha bravo charlie"
	if _, err := store.AddMemory(ctx, micrographrag.MemoryInput{
		Kind:    1,
		Source:  "haosbot/session/test",
		Title:   "Agent turn test",
		Content: "the " + needle + " secret token",
	}); err != nil {
		t.Fatalf("AddMemory: %v", err)
	}

	loop := &Loop{cfg: LoopConfig{GraphMemoryMaxChars: 6000}}
	got, err := loop.graphMemoryContext(ctx, store, needle)
	if err != nil {
		t.Fatalf("graphMemoryContext: %v", err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("retrieved-memory block is EMPTY: the search options disabled every engine " +
			"(normalizeSearchOptions does not copy EnableFTS/EnableVector/EnableGraph)")
	}
	if !strings.Contains(got, needle) {
		t.Fatalf("retrieved-memory block does not contain the stored memory; got %q", got)
	}
	if !strings.Contains(got, "chunk=") || !strings.Contains(got, "score=") {
		t.Fatalf("retrieved-memory block is not in the documented format; got %q", got)
	}
}

// TestGraphMemoryContextRespectsCharLimit pins the truncation contract that the
// same function owns, so a future change to the search call cannot quietly drop
// it.
func TestGraphMemoryContextRespectsCharLimit(t *testing.T) {
	ctx := context.Background()
	store, err := micrographrag.Open(ctx, micrographrag.Config{
		DBPath:    filepath.Join(t.TempDir(), "graph.db"),
		EnableFTS: true,
	}, nil)
	if err != nil {
		t.Skipf("micrographrag unavailable without -tags sqlite_fts5: %v", err)
	}
	defer store.Close()

	if _, err := store.AddMemory(ctx, micrographrag.MemoryInput{
		Kind:    1,
		Source:  "haosbot/session/test",
		Title:   "Agent turn test",
		Content: "delta echo foxtrot " + strings.Repeat("padding ", 500),
	}); err != nil {
		t.Fatalf("AddMemory: %v", err)
	}

	loop := &Loop{cfg: LoopConfig{GraphMemoryMaxChars: 120}}
	got, err := loop.graphMemoryContext(ctx, store, "delta echo foxtrot")
	if err != nil {
		t.Fatalf("graphMemoryContext: %v", err)
	}
	if got == "" {
		t.Fatal("retrieved-memory block is empty")
	}
	// The loop stops once the builder has reached the limit, so the result may
	// exceed it by at most the final entry's prefix line.
	if len(got) > 120+128 {
		t.Fatalf("retrieved-memory block is %d bytes, want it bounded near the 120-char limit", len(got))
	}
}
