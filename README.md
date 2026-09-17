# haosbot-go

A lightweight, standalone Go reimplementation of haosbot preserving behavior, configuration, and data format. The core runtime is a single binary; optional GraphRAG/vector features use Go modules and local model assets.

## Features

- **Standalone Binary**: Single executable with optional GraphRAG/vector capabilities and no required remote runtime service.
- **CPython Minimal & SQLite**: Automated build script for a lightweight CPython runtime with SQLite enabled (`scripts/build_cpython_min.sh`).
- **Python Execution Tool (`python_exec`)**: Allows agents to run dynamic Python scripts, handle SQLite databases, automate tasks, and process data securely.
- **Memory & Dream Subsytem**: Full port of `MemoryArchiver` and `Consolidator` with atomic JSONL persistence and git store.
- **Channels**: Telegram bot transport with ordered ingress queue and safe concurrent handling.
- **HTTP API Server**: OpenAI-compatible endpoints (`/v1/chat/completions`, `/v1/models`, `/health`) with configurable port and optional Bearer token authentication.
- **Memory Fabric**: One SQLite WAL transaction writes the canonical turn and durable GraphRAG/Obsidian projection jobs. Consumers have leases, retries, ACKs, deduplication and crash recovery.
- **Local Vector Retrieval**: Optional offline Potion/Model2Vec embeddings (64D INT8) with sqlite-vec. No model is downloaded in `auto` mode.
- **Operational Metrics**: `/metrics` exposes runtime memory, turns, provider timeouts, projection backlog/failures and vector status as JSON.

---

## Installation & Deployment

Extract the standalone archive:
```bash
tar -xzf haosbot-standalone.tar.gz
cd haosbot
```

Run the standalone installer (sets up `~/.local/bin/`, `~/.haosbot/` workspace templates, and systemd service):
```bash
./install.sh
```

### Systemd Service Management
To enable and start the gateway service:
```bash
systemctl --user daemon-reload
systemctl --user enable --now haosbot
```

To check service status and logs:
```bash
systemctl --user status haosbot
tail -f ~/.haosbot/logs/gateway.log
```

---

## Configuration & Bearer Token Authentication

Configuration and data live in `~/.haosbot/`.
To protect your API gateway endpoints (`/v1/chat/completions`) with a Bearer Token, set an `apiKey` in your `~/.haosbot/config.json`:

```json
{
  "agents": {
    "defaults": {
      "model": "gpt-4o",
      "temperature": 0.7
    }
  },
  "gateway": {
    "apiKey": "YOUR_SECRET_BEARER_TOKEN"
  }
}
```

When `apiKey` is configured, clients must include the header:
```http
Authorization: Bearer YOUR_SECRET_BEARER_TOKEN
```
If `apiKey` is empty or omitted, endpoints are open for local access.

### Low-resource profile

The runtime defaults to one worker per projection and small SQLite caches. Tune
only when measurements justify it:

```bash
export NANOBOT_PROJECTION_WORKERS=1
export NANOBOT_MEMORY_MAX_PENDING=512
export NANOBOT_MEMORY_CACHE_KB=512
```

Graph vector search is enabled only when a local model is present. Put the
64-dimensional model at `$GO_POTION_HOME/BASE2M/` (`model.safetensors` and
`tokenizer.json`) and leave `NANOBOT_GRAPH_EMBEDDER=auto`. Use
`NANOBOT_GRAPH_EMBEDDER=off` to force FTS + graph retrieval. The explicit
`potion` mode may resolve the model through the dependency and is not the
recommended mode for offline or resource-constrained deployments.

The canonical memory database is `~/.haosbot/memory-fabric.db`; projection
artifacts live under `~/.haosbot/obsidian-memory/` and the per-session
GraphRAG databases under `~/.haosbot/graph-sessions/`.
