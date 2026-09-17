#!/usr/bin/env python3
"""Session interop harness — the Python half.

Proves that nanobot-go and the Python nanobot can read each other's session
files in the same ~/.nanobot/ layout. This is the concrete test of the
"compatible with ~/.nanobot/" requirement.

Usage:
    interop_session.py write <workspace> <sessions_root>   # write a rich session
    interop_session.py read  <workspace> <sessions_root> <key>

Both modes print a JSON document on stdout so the Go side can compare parsed
values rather than raw bytes (key order is not significant to either
implementation, because both parse JSON into a mapping).
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "upstream" / "nanobot"))


def make_store(workspace: str, sessions_root: str):
    from nanobot.session.manager import JsonlSessionStore

    ws = Path(workspace).expanduser().resolve(strict=False)
    rt = Path(sessions_root).expanduser().resolve(strict=False)
    ws.mkdir(parents=True, exist_ok=True)
    rt.mkdir(parents=True, exist_ok=True)
    return JsonlSessionStore(ws, sessions_root=rt)


def cmd_write(workspace: str, sessions_root: str) -> int:
    store = make_store(workspace, sessions_root)
    from nanobot.session.manager import Session

    key = "cli:interop"
    s = Session(key=key)
    s.add_message("user", "hello world")
    s.add_message("assistant", "hi there")
    # Unicode must survive: the reference writes raw UTF-8, not \\u escapes.
    s.add_message("user", "acentuação, 日本語, emoji 🎉")
    # An assistant turn carrying a tool call.
    s.add_message(
        "assistant",
        "",
        tool_calls=[{
            "id": "call_1",
            "type": "function",
            "function": {"name": "read_file", "arguments": '{"path":"a.txt"}'},
        }],
    )
    # The matching tool result.
    s.add_message("tool", "file contents", tool_call_id="call_1", name="read_file")
    # A message with HTML-ish characters, to catch Go's default JSON escaping.
    s.add_message("user", "a < b & c > d")

    store.save(s)
    path = store.get_session_path(key)

    print(json.dumps({
        "key": key,
        "path": str(path),
        "workspace_id": path.parent.name,
        "message_count": len(s.messages),
    }, indent=2))
    return 0


def cmd_read(workspace: str, sessions_root: str, key: str) -> int:
    store = make_store(workspace, sessions_root)
    session = store.load(key)
    if session is None:
        print(json.dumps({"error": "session not found", "key": key}))
        return 1

    msgs = []
    for m in session.messages:
        entry = {"role": m.get("role"), "content": m.get("content")}
        for opt in ("tool_calls", "tool_call_id", "name", "reasoning_content"):
            if opt in m:
                entry[opt] = m[opt]
        msgs.append(entry)

    print(json.dumps({
        "key": key,
        "message_count": len(msgs),
        "messages": msgs,
    }, indent=2, ensure_ascii=False))
    return 0


def main(argv: list[str]) -> int:
    if len(argv) < 4:
        print(__doc__, file=sys.stderr)
        return 2
    mode, workspace, sessions_root = argv[1], argv[2], argv[3]
    if mode == "write":
        return cmd_write(workspace, sessions_root)
    if mode == "read":
        if len(argv) < 5:
            print("read requires a session key", file=sys.stderr)
            return 2
        return cmd_read(workspace, sessions_root, argv[4])
    print(f"unknown mode {mode!r}", file=sys.stderr)
    return 2


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
