#!/usr/bin/env python3
"""End-to-end test: drive the real nanobot binary against a mock model server.

This is the only test that exercises the whole stack — config loading, session
storage, provider HTTP, tool dispatch and the agent loop — through the actual
compiled binary rather than by calling library functions directly.

The mock server speaks the OpenAI chat-completions protocol. Two scenarios are
covered:

  1. a plain answer, which must come back verbatim;
  2. a tool call, where the model asks for `read_file`, the runtime executes it
     and feeds the result back, and the model then answers.

Run via scripts/e2e.sh, or directly:
    .tools/venv/bin/python compat/python/e2e_test.py
"""
from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
BIN = ROOT / ".tools" / "bench" / "nanobot"

# Requests the mock server received, for assertions about the wire protocol.
RECEIVED: list[dict] = []
# Scenario switch: "plain" or "tool".
SCENARIO = "plain"


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):  # silence
        pass

    def do_POST(self):  # noqa: N802
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length) or b"{}")
        RECEIVED.append({"path": self.path, "body": body})

        messages = body.get("messages", [])
        already_used_tool = any(m.get("role") == "tool" for m in messages)

        # The reference runner always streams (runner.py:984 uses
        # chat_stream_with_retry), so the mock must speak SSE, not plain JSON.
        if SCENARIO == "tool" and not already_used_tool:
            deltas = [
                {"role": "assistant",
                 "tool_calls": [{
                     "index": 0,
                     "id": "call_1",
                     "type": "function",
                     "function": {"name": "read_file",
                                  "arguments": json.dumps({"path": "hello.txt"})},
                 }]},
            ]
            finish = "tool_calls"
        else:
            text = "TOOL-RESULT-OK" if SCENARIO == "tool" else "E2E-OK"
            deltas = [{"role": "assistant", "content": text}]
            finish = "stop"

        chunks = []
        for d in deltas:
            chunks.append({
                "id": "chatcmpl-1", "object": "chat.completion.chunk",
                "created": 1, "model": body.get("model", "mock"),
                "choices": [{"index": 0, "delta": d, "finish_reason": None}],
            })
        chunks.append({
            "id": "chatcmpl-1", "object": "chat.completion.chunk",
            "created": 1, "model": body.get("model", "mock"),
            "choices": [{"index": 0, "delta": {}, "finish_reason": finish}],
            "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
        })

        payload = b""
        for c in chunks:
            payload += b"data: " + json.dumps(c).encode() + b"\n\n"
        payload += b"data: [DONE]\n\n"

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def start_server() -> tuple[HTTPServer, int]:
    srv = HTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv, srv.server_address[1]


def write_config(home: Path, base_url: str) -> None:
    cfg = {
        "agents": {
            "defaults": {
                "workspace": str(home / "workspace"),
                "model": "openai/mock-model",
                "provider": "openai",
                "timezoneMode": "manual",
                "timezone": "UTC",
            }
        },
        "providers": {"openai": {"apiKey": "test-key", "apiBase": base_url}},
    }
    path = home / ".nanobot" / "config.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(cfg, indent=2))


def run_binary(home: Path, *args: str) -> subprocess.CompletedProcess:
    env = dict(os.environ)
    env["HOME"] = str(home)
    # Never let a build or the runtime touch the RAM-backed /tmp.
    env["TMPDIR"] = str(ROOT / ".tools" / "tmp")
    return subprocess.run(
        [str(BIN), *args],
        env=env,
        capture_output=True,
        text=True,
        timeout=60,
    )


def check(label: str, condition: bool, detail: str = "") -> bool:
    status = "PASS" if condition else "FAIL"
    print(f"  [{status}] {label}" + (f" — {detail}" if detail and not condition else ""))
    return condition


def main() -> int:
    global SCENARIO

    if not BIN.exists():
        print(f"binary not found at {BIN}; run scripts/bench.sh first", file=sys.stderr)
        return 2

    srv, port = start_server()
    base_url = f"http://127.0.0.1:{port}/v1"
    home = ROOT / ".tools" / "e2e-home"
    if home.exists():
        shutil.rmtree(home)
    write_config(home, base_url)

    # A file for the tool scenario to read.
    ws = home / "workspace"
    ws.mkdir(parents=True, exist_ok=True)
    (ws / "hello.txt").write_text("hello from the workspace\n")

    ok = True
    try:
        print("scenario 1: plain answer")
        SCENARIO = "plain"
        RECEIVED.clear()
        r = run_binary(home, "run", "say hi")
        ok &= check("exit code 0", r.returncode == 0, r.stderr[-400:])
        ok &= check("reply is E2E-OK", "E2E-OK" in r.stdout, repr(r.stdout[-200:]))
        ok &= check("server received a request", len(RECEIVED) == 1)
        if RECEIVED:
            body = RECEIVED[0]["body"]
            ok &= check("path is /v1/chat/completions",
                        RECEIVED[0]["path"].endswith("/chat/completions"),
                        RECEIVED[0]["path"])
            ok &= check("model sent", body.get("model") == "mock-model", str(body.get("model")))
            ok &= check("has system + user message",
                        len(body.get("messages", [])) >= 2,
                        str(len(body.get("messages", []))))
            ok &= check("tools advertised", len(body.get("tools", [])) >= 5,
                        str(len(body.get("tools", []))))
            ok &= check("no leaked timestamp key",
                        all("timestamp" not in m for m in body.get("messages", [])))

        print("scenario 2: tool call round trip")
        SCENARIO = "tool"
        RECEIVED.clear()
        r = run_binary(home, "run", "read hello.txt")
        ok &= check("exit code 0", r.returncode == 0, r.stderr[-400:])
        ok &= check("final answer reached model",
                    "TOOL-RESULT-OK" in r.stdout, repr(r.stdout[-200:]))
        ok &= check("two model calls made", len(RECEIVED) == 2, str(len(RECEIVED)))
        if len(RECEIVED) == 2:
            second = RECEIVED[1]["body"]["messages"]
            tool_msgs = [m for m in second if m.get("role") == "tool"]
            ok &= check("tool result fed back", len(tool_msgs) == 1, str(len(tool_msgs)))
            if tool_msgs:
                tm = tool_msgs[0]
                ok &= check("tool message has name",
                            tm.get("name") == "read_file", str(tm.get("name")))
                ok &= check("tool message has tool_call_id",
                            bool(tm.get("tool_call_id")), str(tm.get("tool_call_id")))
                ok &= check("tool actually read the file",
                            "hello from the workspace" in str(tm.get("content")),
                            str(tm.get("content"))[:200])

        print("scenario 3: session persistence on disk")
        sessions = home / ".nanobot" / "sessions"
        files = list(sessions.rglob("*.jsonl")) if sessions.exists() else []
        ok &= check("session file written", len(files) >= 1, str(sessions))
        if files:
            lines = [l for l in files[0].read_text().splitlines() if l.strip()]
            ok &= check("session has records", len(lines) >= 2, str(len(lines)))
            first = json.loads(lines[0])
            ok &= check("first record is metadata",
                        "created_at" in first or "createdAt" in first, str(first)[:200])
            # Timestamps must be naive local ISO, never RFC3339 with Z/offset.
            bad = [l for l in lines if '"timestamp"' in l and ("Z\"" in l or "+00:00" in l)]
            ok &= check("timestamps are naive (no Z/offset)", not bad, str(bad[:1])[:200])
    finally:
        srv.shutdown()

    print()
    print("E2E RESULT:", "ALL PASS" if ok else "FAILURES PRESENT")
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
