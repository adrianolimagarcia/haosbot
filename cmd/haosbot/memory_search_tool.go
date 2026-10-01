package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/memoryfabric"
	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

type memorySearchTool struct {
	tools.ReadOnlyBase
	pool         *graphStorePool
	namespaces   map[string]memoryfabric.Namespace
	defaultScope string
}

func newScopedMemorySearchTool(pool *graphStorePool, namespaces []memoryfabric.Namespace, defaultScope string) *memorySearchTool {
	out := &memorySearchTool{pool: pool, namespaces: map[string]memoryfabric.Namespace{}}
	for _, namespace := range namespaces {
		ns, err := memoryfabric.NormalizeNamespace(namespace)
		if err != nil {
			continue
		}
		out.namespaces[ns.Scope] = ns
	}
	defaultScope = strings.ToLower(strings.TrimSpace(defaultScope))
	if _, ok := out.namespaces[defaultScope]; !ok {
		scopes := make([]string, 0, len(out.namespaces))
		for scope := range out.namespaces {
			scopes = append(scopes, scope)
		}
		sort.Strings(scopes)
		if len(scopes) > 0 {
			defaultScope = scopes[0]
		}
	}
	out.defaultScope = defaultScope
	return out
}

func (t *memorySearchTool) Name() string { return "memory_search" }

func (t *memorySearchTool) Description() string {
	if len(t.namespaces) <= 1 {
		return "Search HAOSBOT's physically isolated long-term GraphRAG namespace for deep recall. " +
			"Use when current history is insufficient or the delegated task needs older decisions."
	}
	return "Search HAOSBOT's physically isolated long-term GraphRAG memory. " +
		"Project memory is the default; global memory must be selected explicitly."
}

func (t *memorySearchTool) Parameters() json.RawMessage {
	properties := map[string]any{
		"query": map[string]any{"type": "string", "description": "What to recall from long-term memory"},
		"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 12, "description": "Maximum results (default 6)"},
	}
	if len(t.namespaces) > 1 {
		scopes := make([]string, 0, len(t.namespaces))
		for scope := range t.namespaces {
			scopes = append(scopes, scope)
		}
		sort.Strings(scopes)
		properties["scope"] = map[string]any{
			"type": "string", "enum": scopes,
			"description": "Physical memory scope to search. Defaults to " + t.defaultScope + ".",
		}
	}
	raw, _ := json.Marshal(map[string]any{
		"type": "object", "properties": properties,
		"required": []string{"query"}, "additionalProperties": false,
	})
	return raw
}

func retrieveScopedMemory(ctx context.Context, pool *graphStorePool, namespace memoryfabric.Namespace, query string, limit, maxChars int) (string, error) {
	query = strings.TrimSpace(query)
	if pool == nil || query == "" { return "", nil }
	if limit <= 0 { limit = 4 }
	if limit > 12 { limit = 12 }
	if maxChars <= 0 { maxChars = 5000 }
	key, err := graphStoreKey(namespace)
	if err != nil { return "", err }
	searchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	store, release, err := pool.Acquire(searchCtx, key)
	if err != nil { return "", err }
	defer release()
	opts := store.DefaultSearchOptions()
	opts.Limit = limit
	results, err := store.Search(searchCtx, query, opts)
	if err != nil { return "", err }
	var out strings.Builder
	for _, result := range results {
		content := strings.TrimSpace(result.Content)
		if content == "" { continue }
		if out.Len() > 0 { out.WriteString("\n\n---\n") }
		if len(content) > 1800 { content = content[:1800] + "... [truncated]" }
		fmt.Fprintf(&out, "score=%.4f\n%s", result.Score, content)
		if out.Len() >= maxChars { break }
	}
	text := out.String()
	if len(text) > maxChars { text = text[:maxChars] + "... [budget truncated]" }
	return text, nil
}

func (t *memorySearchTool) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
		Scope string `json:"scope"`
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
	scope := strings.ToLower(strings.TrimSpace(args.Scope))
	if scope == "" {
		scope = t.defaultScope
	}
	namespace, ok := t.namespaces[scope]
	if !ok {
		return tools.Errf("memory_search: scope %q is not available to this agent", scope), nil
	}
	key, err := graphStoreKey(namespace)
	if err != nil {
		return tools.Errf("memory_search: invalid namespace: %v", err), nil
	}

	searchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	store, release, err := t.pool.Acquire(searchCtx, key)
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
		return tools.OK("No matching long-term memory found in scope " + scope + "."), nil
	}

	var out strings.Builder
	fmt.Fprintf(&out, "Long-term memory matches (scope=%s, derived/untrusted data):\n", scope)
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
