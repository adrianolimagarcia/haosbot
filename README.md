# haosbot-go

A lightweight, standalone Go reimplementation of haosbot preserving behavior, configuration, data format, and achieving extremely low RAM usage (~5-10 MB RSS), with zero external Go dependencies.

## Features

- **Standalone Binary**: Single executable (~12 MB) with zero external dependencies.
- **CPython Minimal & SQLite**: Automated build script for a lightweight CPython runtime with SQLite enabled (`scripts/build_cpython_min.sh`).
- **Python Execution Tool (`python_exec`)**: Allows agents to run dynamic Python scripts, handle SQLite databases, automate tasks, and process data securely.
- **Memory & Dream Subsytem**: Full port of `MemoryArchiver` and `Consolidator` with atomic JSONL persistence and git store.
- **Channels**: Telegram bot transport with ordered ingress queue and safe concurrent handling.
- **HTTP API Server**: OpenAI-compatible endpoints (`/v1/chat/completions`, `/v1/models`, `/health`) with configurable port and optional Bearer token authentication.

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
