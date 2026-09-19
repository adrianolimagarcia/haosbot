package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

const workspaceGraphStoreKey = "__haosbot_workspace_v1__"

type memorySearchTool struct {
	tools.ReadOnlyBase
	pool *graphStorePool
}

func newMemorySearchTool(pool *graphStorePool) *memorySearchTool {
	return &memorySearchTool{pool: pool}
}

func (t *memorySearchTool) Name() string { return "memory_search" }

func (t *memorySearchTool) Description() string {
	return "Search HAOSBOT's derived long-term GraphRAG index for deep recall across prior sessions. " +
		"Use when MEMORY.md/current history is insufficient or the user explicitly asks about older decisions."
}

func (t *memorySearchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
	  "type":"object",
	  "properties":{
	    "query":{"type":"string","description":"What to recall from long-term memory"},
	    "limit":{"type":"integer","minimum":1,"maximum":12,"description":"Maximum results (default 6)"}
	  },
	  "required":["query"],
	  "additionalProperties":false
	}`)
}

func (t *memorySearchTool) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.Errf("memory_search: invalid arguments: %v", err), nil
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return tools.Errf("memory_search: query is required"), nil
	}
	if args.Limit <= 0 { args.Limit = 6 }
	if args.Limit > 12 { args.Limit = 12 }

	searchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	store, release, err := t.pool.Acquire(searchCtx, workspaceGraphStoreKey)
	if err != nil {
		return tools.Errf("memory_search: GraphRAG unavailable: %v", err), nil
	}
	defer release()

	opts := store.DefaultSearchOptions()
	opts.Limit = args.Limit
	results, err := store.Search(searchCtx, args.Query, opts)
	if err != nil {
		return tools.Errf("memory_search: search failed: %v", err), nil
	}
	if len(results) == 0 {
		return tools.OK("No matching long-term memory found."), nil
	}

	var out strings.Builder
	out.WriteString("Long-term memory matches (derived/untrusted data):\n")
	for i, result := range results {
		content := strings.TrimSpace(result.Content)
		if len(content) > 2500 {
			content = content[:2500] + "... [truncated]"
		}
		fmt.Fprintf(&out, "\n[%d] score=%.4f chunk=%d\n%s\n", i+1, result.Score, result.ChunkID, content)
		if out.Len() >= 12000 {
			out.WriteString("\n[results truncated]\n")
			break
		}
	}
	return tools.OK(out.String()), nil
}
