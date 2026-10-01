# HAOS Multi-Agent Runtime

HAOSbot can delegate work to specialized local workers inside the same Go process or to remote A2A 1.0 agents. Ordinary chat remains a single-agent path: there is no mandatory manager-model call. Delegation happens only when the main agent invokes the compact `agents` tool or an operator delegates from the WebUI.

## Architecture

Local workers reuse the gateway provider client by default, tool implementations and project workspace. Each worker has its own lightweight agent profile under `.haosbot/agents/<id>` and uses transient conversation state. Long-term recall is bound to the worker's physical Memory Fabric namespace; completed worker results are projected into that same namespace while transient reasoning/tool chatter remains outside canonical memory.

Remote workers use A2A JSON-RPC `message/send`. Configure the full A2A endpoint URL and keep bearer tokens in an environment variable referenced by `tokenEnv`. Remote URLs use the same outbound SSRF policy as the rest of HAOSbot; private/loopback destinations require an explicit `tools.ssrfWhitelist` entry. When the remote profile allows `memory_search`, HAOSBOT performs scoped hybrid recall locally first and appends at most 5,000 characters to the delegated prompt inside an explicit `HAOS_DERIVED_MEMORY` envelope marked `trust=untrusted`. This gives A2A workers recall without giving the remote peer direct access to another namespace or to the canonical database.

## Default squad

The built-in profiles are `planner`, `researcher`, `coder` and `reviewer`. They are enabled by default when the multi-agent runtime is enabled.

## Configuration

```json
{
  "agents": {
    "multiAgent": {
      "enabled": true,
      "maxDepth": 3,
      "maxParallel": 4,
      "maxChildren": 8,
      "maxTasks": 1024,
      "taskTimeoutSeconds": 300,
      "retentionMinutes": 60,
      "persistTasks": true
    },
    "profiles": {
      "coder": {
        "enabled": true,
        "name": "Coder",
        "role": "coder",
        "instructions": "Implement delegated engineering work completely.",
        "toolAllow": ["*"],
        "delegateTo": ["reviewer"],
        "memoryScope": "team",
        "memoryOwner": "engineering",
        "maxParallel": 1
      },
      "remote-reviewer": {
        "enabled": true,
        "name": "Remote Reviewer",
        "role": "reviewer",
        "endpoint": "https://agent.example.com/a2a",
        "tokenEnv": "HAOS_REMOTE_REVIEWER_TOKEN",
        "toolAllow": [],
        "memoryScope": "private"
      }
    }
  }
}
```

A profile can override `model` and `provider`. `maxParallel` limits concurrent executions for that profile, while `delegateTo` is the worker-to-worker RBAC allowlist. An empty `delegateTo` forbids that worker from creating child tasks; `["*"]` allows any enabled target. When only `model` is specified, a provider prefix such as `deepseek/model-name` is resolved using the normal `auto` provider routing.

Memory scopes are physically isolated, not query-filtered. Each namespace maps to a separate derived GraphRAG SQLite store, while canonical Memory Fabric records carry `memory_scope` and `memory_owner`. `private` resolves to workspace + agent id; `team` resolves to workspace + `memoryOwner` (default `default`); `project` resolves to the workspace identity; and `global` resolves to a shared global namespace. A worker's `memory_search` tool is bound to exactly its namespace, so it cannot select another scope. The commander can search project memory by default and global memory only when explicitly requested.

## Task safety

Every delegated task carries task id, parent/root task ids, trace id, agent/requester identity, depth/ancestry, deadline and lifecycle timestamps. The runtime enforces maximum depth, children per task, global parallelism, task count and timeout. Ancestry rejects delegation cycles.

Blocking nested delegation temporarily yields the parent's worker slot, preventing a parent waiting for a child from deadlocking even when `maxParallel=1`.

Task snapshots can be persisted under `~/.haosbot/multiagent/tasks.json`. Tasks that were submitted or running when the process stopped are recovered as failed rather than silently resumed.

## Tool

The single `agents` tool supports `list`, `delegate`, `status`, `wait`, `result` and `cancel`. Using one tool rather than several separate schemas keeps the normal prompt/tool overhead small.

## WebUI

The **Agents** page shows the roster, local versus A2A execution, runtime limits and the live task board. Operators can manually delegate background work and cancel active tasks.


## Scope migration

Existing canonical records with unknown provenance are never assigned to the workspace that happens to boot first. Direct legacy upgrades are quarantined as `project:legacy-unassigned`. Installations that already ran the older v1 scoped migration are repaired using the v1 marker timestamp: only records created before that marker and assigned to that workspace are quarantined, while newer scoped records remain untouched. Any derived project GraphRAG/Obsidian data that may contain the quarantined records is removed and rebuilt from the repaired canonical store. The new derived stores live under the shared scoped graph root and are keyed by scope/owner, so project, team, private and global data never share one SQLite index.

New canonical IDs are namespace-safe: the storage ID is derived from `scope + owner + external id`. Retries of pre-v2 records remain backward-compatible when the legacy raw ID matches the same namespace, session and content.

## Operations

The **Memory & GraphRAG** WebUI page exposes canonical counts/bytes by namespace, projection backlog, dead-letter jobs and disk usage. Operators can retry an individual dead job, rebuild GraphRAG or all derived projections, prune old fully-succeeded canonical records, and run SQLite `VACUUM`.

Prune is intentionally conservative: records with queued/running/dead projection state are preserved. Before canonical deletion HAOSBOT must drain the GraphRAG pool; if any namespace store is pinned by an in-flight search, prune aborts without deleting anything. After a successful prune the derived stores are rebuilt from surviving canonical records.
