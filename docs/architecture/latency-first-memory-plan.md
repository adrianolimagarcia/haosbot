# HAOSBOT Latency-First Memory Architecture

Status: Proposed implementation plan  
Target: make interactive latency approach nanobot while preserving HAOSBOT memory, GraphRAG, automation and agent capabilities.

## 1. Executive decision

HAOSBOT should stop treating GraphRAG retrieval as a mandatory pre-provider step.

The target architecture is:

1. **Markdown is the canonical long-term semantic memory** and remains human-readable/rebuildable.
2. **An in-memory snapshot of Markdown is the hot-path memory source** used to build the prompt.
3. **GraphRAG becomes a derived, eventually-consistent index** updated asynchronously from Markdown changes.
4. **Normal turns never wait for GraphRAG, embedding, graph traversal, projection acknowledgement or memory database maintenance before starting the provider stream.**
5. **Deep historical retrieval remains available through an explicit `memory_search` tool** and, later, an optional strictly budgeted adaptive retrieval path.
6. Session persistence is changed so that the pre-provider path does not rewrite an entire growing JSONL transcript.
7. Expensive summarization is moved out of the critical path and prepared proactively in background.

The architectural invariant is:

> If GraphRAG, the embedder, the memory projection worker or the derived memory database is slow, locked, rebuilding or unavailable, a normal chat turn must still start the model with the Markdown snapshot and session context.

GraphRAG is an accelerator and retrieval capability, not a gate.

---

## 2. Current latency findings

The current code makes several expensive operations part of time-to-first-token.

### 2.1 GraphRAG retrieval is synchronous before the provider

`internal/agent/loop.go` currently performs `graphMemoryRetrieve()` before `Runner.Run()`.

The hot path is effectively:

```text
HTTP/WebUI
  -> open session
  -> reconcile pending graph-memory marker
  -> build system prompt
  -> acquire/open per-session GraphRAG store
  -> hybrid GraphRAG search
  -> build retrieved-memory block
  -> load history
  -> estimate prompt tokens
  -> possible auto-summarization
  -> save user message/session
  -> Runner.Run()
  -> Provider.ChatStream()
  -> first token
```

This means local memory work is added directly to provider TTFT.

### 2.2 Memory retrieval is enabled by default

The balanced profile enables `MemoryRetrievalEnabled`, and the low-resource profile also leaves retrieval enabled unless the operator explicitly disables it.

Therefore GraphRAG is currently part of the normal interaction path rather than an exceptional/deep-retrieval path.

### 2.3 Per-session GraphRAG databases create cold-open cost

The graph pool currently opens a SQLite graph database per session key. On a cache miss, `Acquire()` performs the database open while the pool mutex is held.

That design provides isolation but adds cold-session cost, increases database churn and limits cache reuse across sessions even though useful long-term memory is frequently cross-session.

### 2.4 Session save rewrites the complete JSONL file

`internal/session/session.go` explicitly states that `Save()` writes the whole session to disk and atomically replaces the file.

The loop saves the user message before starting the runner. As session size increases, pre-provider persistence therefore scales with transcript size.

This is a second structural TTFT problem independent of GraphRAG.

### 2.5 Auto-summarization can block the real turn

Web interaction auto-summarization is currently triggered near 120k tokens and can perform a model request before the actual user turn starts.

The code already documents measured cases in which summarization takes tens of seconds. A maintenance operation must not consume the interactive latency budget.

### 2.6 Streaming itself is not the main problem

The WebUI uses `/api/agent/turn/stream`; the API flushes stream events and the runner forwards provider deltas.

The primary optimization target is therefore **pre-provider overhead**, not replacing SSE.

---

## 3. Performance objectives and SLOs

All performance work must be measured using the same provider, model, prompt, network path and warmed connection state.

### Interactive hot-path targets

For a warm session on local SSD:

- HAOS-only overhead before provider request:
  - p50 <= 5 ms
  - p95 <= 20 ms
  - p99 <= 50 ms
- Cold-session HAOS-only overhead:
  - p95 <= 75 ms
- GraphRAG work before provider start on a normal turn:
  - **0 ms by design**
- Markdown memory-cache hit rate after startup:
  - >= 99%
- Provider stream begins as soon as system prompt + bounded session context are ready.

These are engineering targets, not assumed guarantees. Benchmark results decide whether they are realistic on target hardware.

### Background-memory targets

- Markdown semantic memory becomes visible to the next turn immediately after its atomic update.
- GraphRAG projection lag:
  - p50 <= 1 s while idle
  - p95 <= 5 s under normal load
- Graph projection failure must never prevent chat.
- A graph database can be deleted and fully rebuilt from canonical Markdown without semantic data loss.

---

## 4. Target architecture

```text
                         INTERACTIVE HOT PATH
                         ====================

User/WebUI
   |
   v
Session hot cache
   |
   +----> tiny durability marker / append journal
   |
   v
PromptBuilder
   |
   +----> MemorySnapshotCache ----> canonical Markdown
   |             |
   |             +---- atomic in-memory snapshot read
   |
   v
Provider.ChatStream()
   |
   v
FIRST TOKEN
   |
   v
final answer
   |
   +---------------------------+
                               |
                               v
                     BACKGROUND MEMORY PLANE
                     =======================

                     Memory candidate/update
                               |
                               v
                     Atomic Markdown writer
                               |
                               +----> content hash/version
                               |
                               v
                     Projection queue/coalescer
                               |
                    +----------+----------+
                    |                     |
                    v                     v
                 GraphRAG             optional views
             FTS/vector/graph           / exports
                    |
                    v
             memory_search tool
```

The model always receives fast Markdown memory. GraphRAG remains available for recall that requires more depth than the compact Markdown state.

---

## 5. Memory source of truth

### 5.1 Canonical semantic memory

Use Markdown as the canonical semantic state.

Recommended layout:

```text
<workspace>/
  memory/
    MEMORY.md
    scopes/
      global.md
      project-<id>.md
      private.md
    .index-state.json
```

The first implementation can retain only `MEMORY.md` if scoped memory is not yet needed. The API should nevertheless be designed around a `MemoryDocumentID` so scope can be added without another rewrite.

### 5.2 GraphRAG is explicitly derived

GraphRAG stores:

- source document ID/path;
- source document version/hash;
- chunk hash;
- scope/session provenance;
- indexed timestamp.

It must never contain information that cannot be recreated from canonical memory documents or session archives.

### 5.3 Do not rely on a fragile dual-write transaction

Filesystem Markdown and SQLite cannot participate in one cheap portable atomic transaction.

Use recovery-by-reconciliation instead:

1. atomically replace the Markdown file;
2. publish a best-effort projection notification;
3. background reconciler periodically compares Markdown content hashes with the graph manifest;
4. missed notifications are repaired automatically;
5. duplicate projection requests are idempotent.

The notification is an accelerator. The source hash is the recovery mechanism.

---

## 6. L0: in-memory Markdown snapshot cache

Add a dedicated `MemorySnapshotCache`.

Properties:

- load canonical Markdown once at startup or first access;
- hold immutable snapshot data behind an atomic pointer / RW-safe value;
- prompt building reads the current string without filesystem I/O;
- refresh is triggered after HAOS writes memory;
- external file edits are detected using a cheap watcher or background mtime/hash scan;
- cache contains:
  - raw prompt-ready memory text;
  - document version/hash;
  - optional parsed heading offsets;
  - size/token estimate.

No normal turn should execute `os.ReadFile` for memory after the cache is warm.

### Prompt cache

Also cache static prompt components:

- identity;
- tool contract;
- static skills summary;
- static workspace instructions.

Only dynamic components should be assembled per turn:

- time/runtime fields that truly change;
- current memory snapshot;
- current summary;
- current message/history.

This reduces string building and repeated file parsing.

---

## 7. L1/L2 retrieval policy

### L0 — always available

Compact Markdown snapshot injected into the prompt.

Cost target: effectively memory-copy/string-append scale.

### L1 — optional bounded fast lexical lookup

Later optimization, disabled initially.

A small in-memory lexical index can answer obvious recall queries with a strict micro-budget. It must have a hard deadline; timeout means "continue without it".

### L2 — GraphRAG deep retrieval

Expose GraphRAG through a first-class tool such as:

```text
memory_search(
  query,
  scope,
  limit,
  mode = "hybrid"
)
```

Modes can include:

- `fts`
- `vector`
- `graph`
- `hybrid`

This preserves HAOSBOT's advanced memory while making its cost intentional.

### Adaptive retrieval, only after baseline success

A later version may automatically request deep retrieval when a deterministic classifier identifies strong memory intent, for example explicit references such as "what did I decide about X?".

Rules:

- must be feature-flagged;
- must have a strict latency budget;
- must never delay provider start beyond the configured budget;
- benchmark must prove net benefit before enabling by default.

The first latency-first release should prefer explicit `memory_search`.

---

## 8. GraphRAG projection pipeline

### 8.1 Coalesce updates

Do not index every tiny memory mutation independently.

Use a projector with:

- debounce: initial target 500-1000 ms;
- maximum wait: 5 s;
- batch size: initial target 16-32 documents/chunks;
- content-hash deduplication.

Repeated updates to the same Markdown document collapse to the newest version.

### 8.2 Incremental chunking

Chunk Markdown by stable semantic boundaries such as headings/sections.

For each chunk compute:

```text
chunk_id = hash(document_id + heading_path + normalized_content)
```

Only changed/new chunks are re-embedded and rewritten.

Removed chunks are tombstoned/deleted from the graph index.

### 8.3 FTS first, embeddings second

For fast background freshness:

1. update lexical/FTS representation first;
2. queue vector embedding for changed chunks;
3. update graph/vector representation;
4. mark the document version fully projected.

If the embedder is unavailable, FTS still becomes current.

### 8.4 Load-aware background priority

Projection must yield to interactive work.

When an active model turn exists or CPU/load exceeds a configured threshold:

- reduce embedding batch size;
- extend debounce/backoff;
- avoid database maintenance;
- continue only lightweight queue bookkeeping.

Chat latency wins over index freshness.

---

## 9. Replace per-session graph-store churn

The preferred target is a **workspace/project-scoped long-lived GraphRAG database**, not one database per chat session.

Store `session_key`, project/scope and source document as metadata.

Benefits:

- one warm SQLite page cache;
- no graph DB open in normal turn path;
- far fewer SQLite files;
- no global pool mutex around database open;
- cross-session long-term recall naturally works;
- easier reconciliation and rebuild.

### Migration safety

Do not delete old per-session graph files automatically.

Migration process:

1. open new workspace graph store;
2. rebuild it from canonical Markdown;
3. optionally import legacy graph/session memories when required;
4. verify counts/hashes;
5. switch reads to the workspace store;
6. retain legacy files until a later explicit cleanup.

If per-session stores must remain for compatibility, replace the global open lock with per-key singleflight so an unrelated cold session cannot serialize every other graph-store acquisition.

---

## 10. SQLite / graph database optimization

Some useful settings already exist: WAL and `synchronous=NORMAL`.

The new design should benchmark rather than blindly add pragmas.

Recommended baseline:

- WAL;
- `synchronous=NORMAL`;
- prepared statements for frequent projection operations;
- one serialized writer;
- separate long-lived read connection/path for deep retrieval when supported;
- batch changed chunks in one transaction;
- bounded page cache;
- `busy_timeout` appropriate for background writers;
- `PRAGMA optimize` only during idle maintenance;
- WAL checkpoint during idle periods, not during interactive turns;
- no `VACUUM` on the interactive path.

Evaluate behind benchmarks:

- `temp_store=MEMORY` for balanced profile;
- bounded `mmap_size`;
- larger cache for machines with RAM;
- FTS optimize cadence.

Low-resource mode may intentionally retain file-backed temp storage.

### Avoid write amplification

The projector should not perform one transaction per chunk.

Desired shape:

```text
begin
  update source manifest
  delete stale chunks
  upsert N FTS chunks
  upsert N vectors
  update graph edges
commit
```

One document/batch, one transaction.

---

## 11. Session persistence: remove full transcript rewrite from TTFT

This is as important as GraphRAG for long sessions.

Current `Session.Save()` rewrites the complete JSONL transcript, and the user message is saved before provider execution.

### Target

Pre-provider persistence must be O(new turn), not O(total history).

Introduce a small **turn journal / pending-turn sidecar**:

```text
session.jsonl             <- compact canonical transcript
session.turnlog           <- append-only/new-turn durability journal
session.checkpoint.json   <- runtime checkpoint if needed
```

Before provider start:

1. append or atomically write only the pending user-turn record;
2. update in-memory session state;
3. start provider immediately.

After the turn:

1. append assistant/tool records to the journal;
2. background compactor merges journal + canonical JSONL;
3. atomically replaces compact JSONL;
4. truncates/removes the journal.

Recovery loads canonical JSONL then replays the journal.

### Compatibility mode

Because HAOSBOT mirrors nanobot/Python session format, keep a compatibility flag initially:

- `legacy_full_save`
- `journal_fastpath`

The fast path becomes default only after differential tests prove equivalent visible history and crash recovery.

### Cache sizing

The current session cache is intentionally tiny. Add profile-driven limits and measure them.

Balanced systems can hold more active sessions and transcript bytes in memory; low-resource mode keeps conservative limits.

---

## 12. Background/proactive context compaction

Auto-summarization must not be a surprise synchronous request at the start of an interactive turn.

### New policy

After each completed turn, estimate context utilization.

When utilization crosses a proactive watermark, e.g. 70-80% of the configured threshold:

1. schedule summary generation in background;
2. persist the completed summary/checkpoint;
3. make the next turn consume the ready summary.

At interactive turn time:

- use ready summary if available;
- if a background summary is still running, do **not** wait for it;
- use the current bounded history/provider compaction strategy;
- only a user-explicit `/compact` command may intentionally block for compaction.

A failed background summary should apply cooldown/hysteresis and never make a chat session unusable.

---

## 13. Memory write path

Long-term memory writes generally happen after the answer, so correctness can be strong without impacting TTFT.

Add a `MarkdownMemoryStore` with:

- per-document lock;
- read-current-version;
- deterministic update;
- write temporary file in the same directory;
- optional fsync according to durability profile;
- atomic rename;
- increment/version hash;
- update `MemorySnapshotCache`;
- notify projector.

### Durability profiles

`balanced`:

- atomic rename;
- normal filesystem durability;
- background graph projection.

`strict`:

- fsync file;
- fsync parent directory where supported;
- slower but crash-resistant.

`low`:

- small cache;
- no vector embedder by default;
- same semantic invariants.

Graph projection failure never rolls back canonical Markdown.

---

## 14. Memory Fabric migration

The current Memory Fabric SQLite database is the canonical record + transactional outbox.

Do not remove it in one step.

### Phase A — compatibility

- introduce Markdown canonical store;
- continue emitting existing memory-fabric jobs for GraphRAG;
- compare Markdown hashes and memory-fabric records in tests;
- reads use Markdown cache, not GraphRAG.

### Phase B — projector source change

- graph projector consumes Markdown document versions/hashes;
- memory fabric becomes projection queue/state only, not semantic truth;
- add reconciliation scan.

### Phase C — simplify

Once recovery tests prove that GraphRAG is fully rebuildable from Markdown:

- remove duplicate canonical content storage from the projection DB;
- keep only job/receipt/manifest state if a durable queue is still useful;
- alternatively replace it with a compact projection-state DB.

No semantic information should exist only in the queue DB.

---

## 15. Observability required before optimization

Existing metrics separate graph search, provider TTFT, persistence and total turn time. Extend them to expose the missing critical-path boundaries.

Add:

- `request_to_provider_start_ms`
- `request_to_first_stream_delta_ms`
- `haos_pre_provider_overhead_ms`
- `session_open_ms`
- `session_pre_provider_persist_ms`
- `prompt_build_ms`
- `memory_snapshot_read_ms`
- `memory_snapshot_cache_hit_total`
- `graph_projection_lag_ms`
- `graph_projection_batch_size`
- `graph_projection_queue_depth`
- `graph_projection_failures_total`
- `memory_search_ms` by mode
- `background_compaction_ms`
- `background_compaction_ready_total`
- `background_compaction_missed_total`

Derived metric:

```text
haos_overhead =
  request_to_first_stream_delta
  - provider_ttft
```

Also emit structured per-turn trace timings in debug mode.

---

## 16. Benchmark harness

Performance claims must be reproducible.

Add benchmark scenarios for:

- empty/new session;
- warm session;
- 10, 100, 1,000 and large historical turns;
- 1, 8 and 32 concurrent sessions;
- no memory;
- 10 KB, 100 KB, 1 MB and large Markdown memory;
- GraphRAG caught up;
- GraphRAG 10,000 jobs behind;
- embedder enabled/disabled;
- graph database cold/warm;
- summary below/above proactive watermark.

Measure:

- request -> provider start;
- request -> first reasoning/content delta;
- provider TTFT;
- total turn;
- bytes written before provider start;
- syscalls/file opens where practical;
- CPU;
- RSS;
- graph lag.

Comparison matrix:

```text
nanobot reference
HAOSBOT current/legacy
HAOSBOT MD hot path
HAOSBOT MD hot path + background graph
HAOSBOT MD hot path + deep memory_search
```

Use the same provider/model and repeat enough runs to report p50/p95/p99, not one-off timings.

---

## 17. Failure-injection / harness matrix

The redesign is accepted only if it survives:

- process kill during Markdown temp write;
- process kill after Markdown rename but before projection notification;
- projector crash during batch;
- duplicate projection notification;
- stale graph manifest;
- corrupt graph DB;
- graph DB deleted;
- SQLite busy/locked;
- disk full;
- read-only memory directory;
- missing embedder;
- embedder timeout;
- huge memory document;
- malformed/external manual Markdown edit;
- simultaneous memory writes;
- 32 concurrent sessions;
- pending session journal at startup;
- truncated session journal tail;
- repeated crash/restart during session compaction.

Expected invariant:

> The worst GraphRAG failure mode is stale/degraded deep retrieval. It must not become a failed or slow normal chat turn.

---

## 18. Rollout phases

### P0 — instrument and freeze baseline

Files:
- `internal/observability/metrics.go`
- `internal/api/agent_turn_stream.go`
- `internal/agent/loop.go`
- `internal/agent/runner.go`
- benchmark scripts

Deliverables:
- provider-start timestamp;
- end-to-end first-delta timestamp;
- baseline p50/p95/p99.

Acceptance:
- measurements account for >95% of pre-provider wall time.

### P1 — remove GraphRAG from normal pre-provider path

Files:
- `internal/agent/loop.go`
- `cmd/haosbot/runtime.go`
- memory configuration

Change:
- stop automatic `graphMemoryRetrieve()` on every normal turn;
- prompt uses Markdown memory snapshot;
- legacy behavior remains behind a feature flag.

Acceptance:
- normal turn performs zero GraphRAG search before `ChatStream`;
- existing streaming tests pass;
- Markdown memory appears in prompt.

### P2 — MemorySnapshotCache + MarkdownMemoryStore

New package suggestion:
- `internal/memoryhot/`

Responsibilities:
- canonical Markdown read/write;
- immutable snapshots;
- atomic refresh;
- version/hash;
- external-change detection.

Acceptance:
- warm prompt memory read does not touch filesystem;
- concurrent read/write race tests pass;
- crash-write tests preserve old or new complete file, never partial content.

### P3 — asynchronous GraphRAG projector

Files:
- `cmd/haosbot/projection_manager.go`
- `internal/memoryfabric/*`
- MicroGraphRAG integration

Change:
- project Markdown versions/chunks;
- debounce/coalesce;
- batch transactions;
- manifest/reconciliation.

Acceptance:
- graph can lag without chat latency change;
- graph catches up after worker restart;
- delete graph DB -> complete rebuild succeeds.

### P4 — workspace-scoped long-lived graph store

Files:
- replace/simplify `cmd/haosbot/graph_pool.go`

Change:
- one workspace/project graph DB;
- provenance stored in rows/chunks;
- no per-session DB open on chat path.

Acceptance:
- no graph-store open occurs during a normal turn;
- cross-session recall test passes;
- migration from legacy per-session graph is safe.

### P5 — `memory_search` tool

Files:
- `internal/tools/builtin/`
- tool registry
- GraphRAG query adapter

Acceptance:
- FTS/vector/graph/hybrid modes;
- bounded result/context size;
- cancellation/timeouts;
- unavailable graph produces a tool-level degraded response, not an agent crash.

### P6 — session journal fast path

Files:
- `internal/session/store.go`
- `internal/session/session.go`
- recovery/differential tests

Change:
- O(new records) pre-provider persistence;
- background/full compaction;
- legacy compatibility mode.

Acceptance:
- pre-provider bytes written do not grow with transcript length;
- Python-format differential tests remain correct;
- kill/restart recovery reconstructs all acknowledged turns.

### P7 — proactive background compaction

Files:
- `internal/agent/loop.go`
- compaction scheduler/state

Acceptance:
- automatic summary provider call never blocks ordinary provider start;
- a ready summary is consumed on the next turn;
- failure cooldown prevents retry storms.

### P8 — tune SQLite/GraphRAG using evidence

Only after P0-P7 benchmark data exists.

Evaluate:
- temp store policy;
- cache sizes;
- mmap;
- transaction batch size;
- FTS optimize interval;
- graph worker count;
- embedding batch size.

Reject changes that improve microbenchmarks but worsen interactive p95/p99.

---

## 19. Feature flags / rollback

Every structural phase must be reversible.

Recommended config model:

```text
memory.hot_path = markdown
memory.deep_retrieval = tool
memory.graph_projection = async
memory.graph_debounce_ms = 750
memory.graph_batch_size = 32
session.persistence = journal_fastpath
compaction.mode = background
```

Keep compatibility aliases for existing environment variables during migration.

Emergency rollback modes:

- `memory.hot_path = legacy_graph`
- `session.persistence = legacy_full_save`
- `compaction.mode = legacy_sync`

A rollback must not require data migration.

---

## 20. Implementation rules

1. **Provider start is sacred.** No derived-index maintenance may precede it.
2. **No unbounded queues.**
3. **No new global locks around disk/network I/O.**
4. **Every background job is idempotent.**
5. **Every background operation is cancelable and bounded.**
6. **Canonical memory remains readable without GraphRAG.**
7. **GraphRAG remains rebuildable.**
8. **No silent memory loss on queue overflow.** Coalesce by latest document version instead.
9. **Do not add cache complexity without an invalidation rule.**
10. **Optimize p95/p99, not just average latency.**
11. **Profile before and after each phase.**
12. **Keep low-resource mode first-class.**

---

## 21. Expected result

After P1-P3, a normal turn becomes approximately:

```text
request
 -> cached session state
 -> cached Markdown memory snapshot
 -> build bounded prompt
 -> tiny pending-turn persistence
 -> provider stream
 -> first token
```

GraphRAG then runs underneath the conversation instead of in front of it.

After P4-P7, HAOSBOT should preserve:

- MicroGraphRAG hybrid retrieval;
- graph/vector/FTS memory;
- durable/recoverable memory;
- session history;
- tools;
- WebUI streaming;
- automation;
- background projections;
- deep cross-session recall;

while removing the largest structural causes of interactive latency:

- synchronous GraphRAG lookup;
- per-session graph cold-open;
- full transcript rewrite before the provider;
- synchronous automatic summarization.

The benchmark gate is simple:

> HAOSBOT is not considered latency-optimized until the measured HAOS-only pre-provider overhead is a small fraction of provider TTFT and remains nearly flat as memory and session history grow.
