#!/usr/bin/env python3
"""Dump reference values for the session summary / checkpoint machinery.

This is a fourth, independent dumper alongside dump_reference.py,
dump_outbound_events.py and dump_legacy_history.py. It exists for the same
reason the others do: the shared dumpers are edited concurrently by several
agents, and a read-modify-write race there would destroy work. Keeping this
section in its own file and its own Go test file removes the race entirely.

Run with the project venv:
    .tools/venv/bin/python compat/python/dump_session_summary.py

Covers, at HKUDS/nanobot@1bb712d3488915ca4ed9ccc1a93067ff722f5ab9:
    nanobot/session/summary.py          (the whole file)
    nanobot/session/history_visibility.py
    nanobot/session/manager.py:323-483  (commit_summary_checkpoint, get_history, clear)
    nanobot/utils/helpers.py:452-476    (recent_message_start_index)
    nanobot/runtime_context.py:14,215   (RUNTIME_CONTEXT_HISTORY_META, public_history_message)
    datetime.fromisoformat               (the accept/reject decision)

Every value in the output comes from EXECUTING the frozen reference. Nothing is
transcribed from documentation or from the Go implementation.

Sessions are seeded as session FILES and loaded through the real
JsonlSessionStore, rather than by constructing Session objects in memory. That
keeps created_at/updated_at deterministic (they come from the file) and
exercises the same load path the Go side uses.

Only one thing in the output is non-deterministic: the timestamp
commit_summary_checkpoint stamps on the continuation marker, which comes from
datetime.now(). Both sides normalise exactly that field to "<MARKER_TS>" and
assert its shape separately.
"""
from __future__ import annotations

import json
import re
import shutil
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "upstream" / "nanobot"))

# loguru writes diagnostics to stderr; this harness must emit exactly one JSON
# document on stdout so the Go side can parse it.
try:
    from loguru import logger as _loguru_logger

    _loguru_logger.remove()
except Exception:  # pragma: no cover - loguru is present in the project venv
    pass

# Scratch directories live under .tools/ and carry the PID, because several
# agents run `go test ./compat/` concurrently and a fixed name would let one run
# delete another's scratch tree.
SCRATCH = ROOT / ".tools" / f"session-summary-dump-{__import__('os').getpid()}"

KEY = "telegram:summary-differential"
CREATED_AT = "2026-01-02T03:04:05.123456"
UPDATED_AT = "2026-01-02T03:04:09.654321"
FALLBACK_LAST_ACTIVE = "2026-03-04T05:06:07.000001"

MARKER_TS_RE = re.compile(
    r'("content": "Continue the active task from the working-memory checkpoint'
    r' above\.", "_hidden_history": true, "timestamp": ")[^"]*(")'
)


def normalize_persisted(text: str) -> str:
    """Replace the one non-deterministic field of the marker record."""
    return MARKER_TS_RE.sub(r"\1<MARKER_TS>\2", text)


def make_store(workspace: Path, sessions_root: Path):
    from nanobot.session.manager import JsonlSessionStore

    workspace.mkdir(parents=True, exist_ok=True)
    sessions_root.mkdir(parents=True, exist_ok=True)
    return JsonlSessionStore(workspace, sessions_root=sessions_root)


def render_lines(metadata, last_archived, messages) -> list[str]:
    """Render the session file the way _save_unlocked would."""
    lines = [
        json.dumps(
            {
                "_type": "metadata",
                "key": KEY,
                "created_at": CREATED_AT,
                "updated_at": UPDATED_AT,
                "metadata": metadata,
                "last_archived": last_archived,
                "last_consolidated": last_archived,
            },
            ensure_ascii=False,
        )
    ]
    for message in messages:
        lines.append(json.dumps(message, ensure_ascii=False))
    return lines


def seed(store, lines: list[str]):
    path = store.get_session_path(KEY)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return store.load(KEY)


# ---------------------------------------------------------------------------
# 1. datetime.fromisoformat accept/reject
# ---------------------------------------------------------------------------


def fromisoformat_cases() -> list[str]:
    """An adversarial corpus for the accept/reject decision.

    session_summary_from_metadata keeps the stored last_active verbatim when
    datetime.fromisoformat accepts it and substitutes the fallback otherwise, so
    the decision itself is the observable behaviour.
    """
    cases: list[str] = []

    def add(*values: str) -> None:
        cases.extend(values)

    # Documented shapes.
    add(
        "2026-01-02", "2026-01-02T03:04:05", "2026-01-02T03:04:05.123456",
        "2026-01-02T03:04:05+00:00", "2026-01-02T03:04:05Z", "garbage", "",
        "2026-01-02 03:04:05", "2026-01-02T03:04",
    )
    # Date-only and compact forms.
    add(
        "2026", "2026-01", "2026-1", "2026-01-2", "202601", "20260102",
        "2026-002", "2026-W01", "2026-W1", "2026W01", "2026-W01-1", "2026W011",
        "2026-W01-", "2026-01-02-", "20260102T", "2026-W53-1", "2026-W53-7",
        "2020-W53-1", "2015-W53-7", "2026-W00-1", "2026-W01-0", "2026-W01-8",
        "2026-W54-1", "0001-W01-1", "9999-W52-5", "9999-W52-7",
    )
    # Separator handling: any single character is accepted.
    add(
        "2026-01-02T03:04:05", "2026-01-02t03:04:05", "2026-01-02 03:04:05",
        "2026-01-0203:04:05", "2026-01-02X03:04:05", "2026-01-02_03:04:05",
        "2026-01-02\t03:04:05", "2026-01-02é03:04:05", "2026-01-02🎉03:04:05",
        "2026-W01-1X03:04:05", "2026W011T03:04:05", "20260102 03:04:05",
    )
    # Time shapes.
    add(
        "2026-01-02T03", "2026-01-02T0304", "2026-01-02T030405",
        "2026-01-02T3:04:05", "2026-01-02T03:4:05", "2026-01-02T03:04:5",
        "2026-01-02T03:04:05:", "2026-01-02T03::05", "2026-01-02T03:04:05.5",
    )
    # Fractional seconds.
    add(
        "2026-01-02T03:04:05.", "2026-01-02T03:04:05.0",
        "2026-01-02T03:04:05.000000", "2026-01-02T03:04:05.0000001",
        "2026-01-02T03:04:05.12345678901234567890", "2026-01-02T03:04:05,123",
        "2026-01-02T03:04.5", "2026-01-02T03:04:05.1234567",
    )
    # Timezone shapes.
    add(
        "2026-01-02T03:04:05Z", "2026-01-02T03:04:05z", "2026-01-02T03:04:05+01",
        "2026-01-02T03:04:05+0100", "2026-01-02T03:04:05+01:00",
        "2026-01-02T03:04:05-01:00", "2026-01-02T03:04:05+01:00:30",
        "2026-01-02T03:04:05+1:00", "2026-01-02T03:04:05+24:00",
        "2026-01-02T03:04:05-24:00", "2026-01-02T03:04:05+99:99",
        "2026-01-02T03:04:05 Z", "2026-01-02T03:04:05+23:59:59",
        "2026-01-02T03:04:05-23:59:59", "2026-01-02T03:04:05+00:00:00",
        "2026-01-02T03:04:05.5+01:00", "2026-01-02T03:04:05.5Z",
        "2026-01-02T03:04:05+", "2026-01-02T03:04:05-",
        "2026-01-02T03:04:05+00:00:00.5", "2026-01-02T03:04:05-00:00:01",
    )
    # Range checks.
    add(
        "2026-00-01", "2026-01-00", "2026-02-29", "2024-02-29", "0000-01-01",
        "9999-12-31", "10000-01-01", "-001-01-01", "2026-01-02T24:00:00",
        "2026-01-02T23:60:00", "2026-01-02T23:59:60", "2026-01-02T23:59:61",
        "2026-01-02T24:00:01", "2026-12-31T24:00:00", "2026-01-00T24:00:00",
        "2026-13-01T24:00:00", "2026-01-31T24:00:00", "2024-02-28T24:00:00",
        "2026-01-02T25:00:00",
    )
    # Whitespace, lengths and trailing junk.
    add(
        " 2026-01-02", "2026-01-02 ", "2026-01-02T03:04:05\n", "\t2026-01-02",
        "2026-01-0", "2026-01-", "2026-", "2026", "123456", "1234567",
        "12345678", "123456789", "abcdefg", "2026-01-02T03:04:05.123456+00:00",
        "2026-01-02T03:04:05.123456Z", "2026-01-02T03:04:05extra",
    )
    # Non-ASCII and control characters.
    add(
        "2026-01-02\u00e9", "2026-01-02T03:04:05\u00e9", "２０２６-０１-０２",
        "2026-01-02T03:04:05\x1c", "2026\u00a0-01-02", "\u20282026-01-02",
    )
    return cases


def dump_fromisoformat() -> dict:
    from datetime import datetime

    accepted = 0
    out = []
    for value in fromisoformat_cases():
        try:
            datetime.fromisoformat(value)
            ok = True
        except ValueError:
            ok = False
        except Exception as exc:  # pragma: no cover - must not happen
            ok = f"unexpected:{type(exc).__name__}"
        if ok is True:
            accepted += 1
        out.append({"s": value, "ok": ok})
    return {"accepted": accepted, "cases": out}


# ---------------------------------------------------------------------------
# 2. session_summary_from_metadata
# ---------------------------------------------------------------------------


def summary_metadata_cases() -> list[tuple[str, object]]:
    from datetime import datetime

    from nanobot.session.summary import session_summary_from_metadata

    cases: list[tuple[str, object]] = [
        ("none_metadata", None),
        ("empty_metadata", {}),
        ("null_summary", {"_last_summary": None}),
        ("int_summary", {"_last_summary": 5}),
        ("str_summary", {"_last_summary": "text"}),
        ("list_summary", {"_last_summary": [{"text": "x"}]}),
        ("empty_object", {"_last_summary": {}}),
        ("text_missing", {"_last_summary": {"last_active": "2026-01-02"}}),
        ("text_null", {"_last_summary": {"text": None}}),
        ("text_int", {"_last_summary": {"text": 5}}),
        ("text_float", {"_last_summary": {"text": 5.0}}),
        ("text_bool", {"_last_summary": {"text": True}}),
        ("text_empty", {"_last_summary": {"text": ""}}),
        ("text_list", {"_last_summary": {"text": ["a"]}}),
        ("text_ok_no_last_active", {"_last_summary": {"text": "keep me"}}),
        ("text_ok_null_last_active", {"_last_summary": {"text": "keep me", "last_active": None}}),
        ("text_ok_int_last_active", {"_last_summary": {"text": "keep me", "last_active": 5}}),
        ("text_ok_list_last_active", {"_last_summary": {"text": "keep me", "last_active": []}}),
        ("text_ok_object_last_active", {"_last_summary": {"text": "keep me", "last_active": {}}}),
        ("text_ok_bool_last_active", {"_last_summary": {"text": "keep me", "last_active": False}}),
        ("text_ok_valid_last_active",
         {"_last_summary": {"text": "keep me", "last_active": "2026-01-02T03:04:05"}}),
        ("text_ok_invalid_last_active",
         {"_last_summary": {"text": "keep me", "last_active": "not a date"}}),
        ("text_ok_empty_last_active",
         {"_last_summary": {"text": "keep me", "last_active": ""}}),
        ("extra_keys_ignored",
         {"_last_summary": {"text": "keep me", "last_active": "2026-01-02T03:04:05", "x": 1},
          "other": {"a": 1}}),
        ("unicode_text", {"_last_summary": {"text": "acentuação 日本語 🎉"}}),
        ("newline_text", {"_last_summary": {"text": "line1\nline2"}}),
        ("nested_oddity", {"_last_summary": {"text": "keep me", "last_active": {"a": [1, 2]}}}),
        ("summary_is_bool", {"_last_summary": True}),
        ("summary_is_float", {"_last_summary": 1.0}),
    ]

    # Every fromisoformat corpus value, as a stored last_active.
    for index, value in enumerate(fromisoformat_cases()):
        cases.append((
            f"last_active_corpus_{index}",
            {"_last_summary": {"text": "keep me", "last_active": value}},
        ))

    fallback = datetime.fromisoformat(FALLBACK_LAST_ACTIVE)
    out = []
    for name, metadata in cases:
        try:
            result = session_summary_from_metadata(metadata, fallback_last_active=fallback)
        except Exception as exc:  # pragma: no cover - must not happen
            result = f"error:{type(exc).__name__}"
        out.append({"id": name, "metadata": metadata, "result": result})
    return out


# ---------------------------------------------------------------------------
# 3. is_summary_checkpoint
# ---------------------------------------------------------------------------

CONTINUATION = "Continue the active task from the working-memory checkpoint above."


def summary_checkpoint_cases() -> list[tuple[str, dict]]:
    cases: list[tuple[str, dict]] = []

    def add(name: str, message: dict) -> None:
        cases.append((name, message))

    for label, marker in [
        ("absent", "ABSENT"),
        ("true", True),
        ("false", False),
        ("one", 1),
        ("zero", 0),
        ("one_float", 1.0),
        ("empty_object", {}),
        ("object", {"a": 1}),
        ("empty_list", []),
        ("list", [1]),
        ("empty_string", ""),
        ("string", "yes"),
        ("null", None),
    ]:
        message = {"role": "user", "content": CONTINUATION}
        if marker != "ABSENT":
            message["_hidden_history"] = marker
        add(f"hidden_{label}", message)

    for label, marker in [
        ("absent", "ABSENT"), ("true", True), ("false", False),
        ("empty_object", {}), ("object", {"kind": "cron"}), ("null", None),
        ("string", "x"),
    ]:
        message = {"role": "user", "content": CONTINUATION}
        if marker != "ABSENT":
            message["_automation_turn"] = marker
        add(f"automation_{label}", message)

    for label, marker in [("true", True), ("false", False), ("null", None), ("one", 1)]:
        add(f"cron_{label}", {"role": "user", "content": CONTINUATION, "_cron_turn": marker})

    for label, content in [
        ("exact", CONTINUATION),
        ("trailing_space", CONTINUATION + " "),
        ("leading_space", " " + CONTINUATION),
        ("different", "Continue the active task from the working-memory checkpoint above"),
        ("empty", ""),
        ("null", None),
        ("list", [{"type": "text", "text": CONTINUATION}]),
        ("number", 5),
        ("bool", True),
        ("object", {"text": CONTINUATION}),
    ]:
        add(f"content_{label}", {"role": "user", "content": content, "_hidden_history": True})

    for label, role in [("user", "user"), ("assistant", "assistant"), ("tool", "tool"),
                        ("system", "system"), ("empty", "")]:
        add(f"role_{label}", {"role": role, "content": CONTINUATION, "_hidden_history": True})

    add("both_markers", {"role": "user", "content": CONTINUATION,
                         "_hidden_history": True, "_automation_turn": True})
    add("hidden_plus_extra", {"role": "user", "content": CONTINUATION,
                              "_hidden_history": {"note": "n"}, "timestamp": UPDATED_AT})
    return cases


def dump_is_summary_checkpoint() -> list[dict]:
    from nanobot.session.summary import is_summary_checkpoint

    out = []
    for name, message in summary_checkpoint_cases():
        out.append({"id": name, "message": message, "result": bool(is_summary_checkpoint(message))})
    return out


# ---------------------------------------------------------------------------
# 4. commit_summary_checkpoint
# ---------------------------------------------------------------------------

BASE_MESSAGES = [
    {"role": "user", "content": "first", "timestamp": "2026-01-02T03:04:05.100001"},
    {"role": "assistant", "content": "second", "timestamp": "2026-01-02T03:04:06.100001"},
    {"role": "user", "content": "third", "timestamp": "2026-01-02T03:04:07.100001"},
    {"role": "assistant", "content": "fourth", "timestamp": "2026-01-02T03:04:08.100001"},
]

COMMIT_CASES = [
    ("append", None, None, "a summary"),
    ("insert_zero", 0, None, "a summary"),
    ("insert_mid", 2, None, "a summary"),
    ("insert_end", 4, None, "a summary"),
    ("insert_beyond", 99, None, "a summary"),
    ("insert_negative_one", -1, None, "a summary"),
    ("insert_negative_beyond", -99, None, "a summary"),
    ("explicit_last_active", 1, "2020-05-06T07:08:09.101112", "another summary"),
    ("empty_summary", 2, None, ""),
    ("unicode_summary", 2, None, "acentuação 日本語 🎉 \"quoted\" \\ slash"),
    ("control_summary", 2, None, "a\u001cb\u2028c\nd"),
    ("summary_with_metadata", 2, None, "meta"),
]


def dump_commit() -> list[dict]:
    out = []
    for name, insert_at, last_active, summary in COMMIT_CASES:
        case_dir = SCRATCH / f"commit-{name}"
        store = make_store(case_dir / "ws", case_dir / "sessions")
        metadata = {"title": "T", "_last_summary": {"text": "old", "last_active": "2026-01-01"}}
        session = seed(store, render_lines(metadata, 1, BASE_MESSAGES))

        kwargs = {}
        if insert_at is not None:
            kwargs["insert_at"] = insert_at
        if last_active is not None:
            from datetime import datetime

            kwargs["last_active"] = datetime.fromisoformat(last_active)
        session.commit_summary_checkpoint(summary, **kwargs)
        store.save(session)

        path = store.get_session_path(KEY)
        persisted = normalize_persisted(path.read_text(encoding="utf-8"))
        out.append({
            "id": name,
            "insert_at": insert_at,
            "last_active": last_active,
            "summary": summary,
            "seed_lines": render_lines(metadata, 1, BASE_MESSAGES),
            "messages": session.messages,
            "metadata": session.metadata,
            "last_archived": session.last_archived,
            "persisted": persisted,
        })
    return out


# ---------------------------------------------------------------------------
# 5. get_history
# ---------------------------------------------------------------------------


def transcripts() -> list[tuple[str, dict, list[dict]]]:
    """(name, metadata, messages) triples covering the replay code paths."""
    marker = {"role": "user", "content": CONTINUATION, "_hidden_history": True,
              "timestamp": "2026-01-02T03:04:09.000000"}

    basic = [
        {"role": "user", "content": "one", "timestamp": "2026-01-02T03:04:05.000001"},
        {"role": "assistant", "content": "two", "timestamp": "2026-01-02T03:04:06.000001"},
        {"role": "user", "content": "three", "timestamp": "2026-01-02T03:04:07.000001"},
        {"role": "assistant", "content": "four", "timestamp": "2026-01-02T03:04:08.000001"},
    ]

    commands = [
        {"role": "user", "content": "/new", "_command": True, "timestamp": "2026-01-02T03:04:05.000001"},
        {"role": "user", "content": "/help", "_command": 1},
        {"role": "user", "content": "kept", "_command": False},
        {"role": "user", "content": "kept2", "_command": 0},
        {"role": "user", "content": "kept3", "_command": ""},
        {"role": "user", "content": "kept4", "_command": []},
        {"role": "user", "content": "kept5", "_command": {}},
        {"role": "assistant", "content": "answer"},
    ]

    deliveries = [
        {"role": "assistant", "content": "proactive", "_channel_delivery": True},
        {"role": "user", "content": "reply to proactive"},
        {"role": "assistant", "content": "proactive2", "_channel_delivery": 1},
        {"role": "user", "content": "reply 2"},
        {"role": "assistant", "content": "proactive3", "_channel_delivery": "yes"},
        {"role": "user", "content": "reply 3"},
        {"role": "assistant", "content": "proactive4", "_channel_delivery": 0},
        {"role": "user", "content": "reply 4"},
        {"role": "assistant", "content": "proactive5", "_channel_delivery": []},
        {"role": "user", "content": "reply 5"},
        {"role": "assistant", "content": "proactive6", "_channel_delivery": {}},
        {"role": "user", "content": "reply 6"},
    ]

    orphans = [
        {"role": "tool", "tool_call_id": "missing", "name": "t", "content": "orphan"},
        {"role": "tool", "tool_call_id": "also-missing", "content": "orphan2"},
        {"role": "assistant", "content": "", "tool_calls": [
            {"id": "call_1", "type": "function",
             "function": {"name": "read_file", "arguments": "{}"}}]},
        {"role": "tool", "tool_call_id": "call_1", "name": "read_file", "content": "data"},
        {"role": "user", "content": "after tools"},
        {"role": "tool", "tool_call_id": "call_1", "name": "read_file", "content": "again"},
    ]

    sanitize = [
        {"role": "assistant", "content": "[Message Time: 2026-01-02 03:04]hello",
         "timestamp": "2026-01-02T03:04:05.000001"},
        {"role": "assistant", "content": "[Message Time: 2026-01-02 03:04]\nhello"},
        {"role": "assistant", "content": "a\n[image: /tmp/x.png]\nb"},
        {"role": "assistant", "content": "a\n[image: ~/x.png]  \nb"},
        {"role": "assistant", "content": "a\n[image: relative/x.png]\nb"},
        {"role": "assistant", "content": "a\n[image: /x.png] extra\nb"},
        {"role": "assistant", "content": "a\ngenerate_image(prompt)\nb"},
        {"role": "assistant", "content": "a\n  message(text)  \nb"},
        {"role": "assistant", "content": "a\ngenerate_image(b)\nmessage(c)\nb"},
        {"role": "assistant", "content": "  \n\t\n[image: /x.png]\n"},
        {"role": "assistant", "content": "a\u001cb\u2028c\rd"},
        {"role": "assistant", "content": "[Message Time: ]only prefix"},
        {"role": "assistant", "content": "[Message Time: no close"},
        {"role": "assistant", "content": "keep [image: /x.png] inline"},
        {"role": "assistant", "content": "a\n[image: /x.png]\u00a0\nb"},
        {"role": "assistant", "content": "a\nmessage()\nb"},
        {"role": "assistant", "content": "a\nmessagex(b)\nb"},
        {"role": "user", "content": "[Message Time: 2026-01-02 03:04]user side"},
    ]

    media = [
        {"role": "user", "content": "", "media": ["/a.png"]},
        {"role": "user", "content": "text", "media": ["/b.png", "~/c.png"]},
        {"role": "user", "content": "text", "media": []},
        {"role": "user", "content": "text", "media": "not-a-list"},
        {"role": "user", "content": "text", "media": [5, "", "/d.png"]},
        {"role": "assistant", "content": "text", "media": ["/e.png"]},
        {"role": "user", "content": [{"type": "text", "text": "blocks"}], "media": ["/f.png"]},
        {"role": "user", "content": "text", "media": None},
    ]

    cli_apps = [
        {"role": "user", "content": "run it", "cli_apps": [
            {"name": "Foo", "entry_point": "main.py"},
            {"name": "BAR", "entry_point": ""},
            {"name": "", "entry_point": "x"},
            {"name": "  Spaced  ", "entry_point": "  ep  "},
            {"name": 5, "entry_point": 7},
            {"name": True, "entry_point": False},
            {"entry_point": "noname"},
            "not-a-dict",
            {"name": "Nine"},
        ]},
        {"role": "user", "content": "", "cli_apps": [{"name": "empty-content"}]},
        {"role": "assistant", "content": "a", "cli_apps": [{"name": "assistant-side"}]},
        {"role": "user", "content": "b", "cli_apps": []},
        {"role": "user", "content": "c", "cli_apps": "not-a-list"},
        {"role": "user", "content": "d", "cli_apps": [{"name": "x"}],
         "_runtime_context": {"version": 1, "suffix": "RC"}},
    ]

    runtime_context = [
        {"role": "user", "content": "hi\n\n[Runtime Context]",
         "_runtime_context": {"version": 1, "sources": ["goal_state"], "suffix": "[Runtime Context]"}},
        {"role": "user", "content": "hi", "_runtime_context": {"version": 1, "suffix": "nope"}},
        {"role": "user", "content": "hi", "_runtime_context": {"version": 2, "suffix": "hi"}},
        {"role": "user", "content": "hi", "_runtime_context": "not-a-dict"},
        {"role": "user", "content": [{"type": "text", "text": "keep"},
                                     {"type": "text", "text": "inj"}],
         "_runtime_context": {"version": 1, "blocks": [{"type": "text", "text": "inj"}]}},
        {"role": "assistant", "content": "plain", "_runtime_context": {"version": 1, "suffix": "plain"}},
    ]

    empty_assistant = [
        {"role": "assistant", "content": ""},
        {"role": "assistant", "content": "   "},
        {"role": "assistant", "content": "", "tool_calls": []},
        {"role": "assistant", "content": "", "reasoning_content": ""},
        {"role": "assistant", "content": "", "thinking_blocks": []},
        {"role": "assistant", "content": "", "reasoning_content": "thinking"},
        {"role": "assistant", "content": "[image: /x.png]"},
        {"role": "user", "content": "keep"},
    ]

    multimodal = [
        {"role": "user", "content": [{"type": "text", "text": "describe"},
                                     {"type": "image_url", "image_url": {"url": "data:x"}}]},
        {"role": "assistant", "content": "it is an image"},
        {"role": "user", "content": []},
        {"role": "user", "content": None},
        {"role": "assistant", "content": None},
    ]

    summary_transcript = [
        {"role": "user", "content": "old one"},
        {"role": "assistant", "content": "old two"},
        marker,
        {"role": "user", "content": "after checkpoint"},
        {"role": "assistant", "content": "after checkpoint 2"},
    ]

    unicode_transcript = [
        {"role": "user", "content": "acentuação 日本語 🎉"},
        {"role": "assistant", "content": "a\u001cb\u001dc\u2028d\u2029e\u0085f\u000bg"},
        {"role": "user", "content": "trailing\u00a0"},
        {"role": "assistant", "content": "\u00a0leading"},
        {"role": "user", "content": "tab\there"},
    ]

    return [
        ("basic", {}, basic),
        ("commands", {}, commands),
        ("deliveries", {}, deliveries),
        ("orphans", {}, orphans),
        ("sanitize", {}, sanitize),
        ("media", {}, media),
        ("cli_apps", {}, cli_apps),
        ("runtime_context", {}, runtime_context),
        ("empty_assistant", {}, empty_assistant),
        ("multimodal", {}, multimodal),
        ("summary", {"_last_summary": {"text": "old summary", "last_active": "2026-01-01"}},
         summary_transcript),
        ("unicode", {}, unicode_transcript),
    ]


def dump_get_history() -> list[dict]:
    out = []
    for name, metadata, messages in transcripts():
        for last_archived in ("zero", "mid", "end"):
            if last_archived == "zero":
                offset = 0
            elif last_archived == "mid":
                offset = len(messages) // 2
            else:
                offset = len(messages)
            for max_messages in sorted({0, 1, len(messages) - 1, len(messages), len(messages) + 1}):
                for extend_to_user in (False, True):
                    for include_runtime_context in (False, True):
                        case = f"{name}|{last_archived}|{max_messages}|{extend_to_user}|{include_runtime_context}"
                        case_dir = SCRATCH / "gh" / case.replace("|", "_")
                        store = make_store(case_dir / "ws", case_dir / "sessions")
                        try:
                            session = seed(store, render_lines(metadata, offset, messages))
                            result = session.get_history(
                                max_messages,
                                max_tokens=0,
                                extend_to_user=extend_to_user,
                                include_runtime_context=include_runtime_context,
                            )
                        except Exception as exc:
                            result = f"error:{type(exc).__name__}:{exc}"
                        out.append({
                            "id": case,
                            "seed_lines": render_lines(metadata, offset, messages),
                            "max_messages": max_messages,
                            "extend_to_user": extend_to_user,
                            "include_runtime_context": include_runtime_context,
                            "result": result,
                        })
    return out


def main() -> int:
    if SCRATCH.exists():
        shutil.rmtree(SCRATCH, ignore_errors=True)
    SCRATCH.mkdir(parents=True, exist_ok=True)
    try:
        doc = {
            "upstream_commit": "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9",
            "continuation_text": CONTINUATION,
            "key": KEY,
            "created_at": CREATED_AT,
            "updated_at": UPDATED_AT,
            "fallback_last_active": FALLBACK_LAST_ACTIVE,
            "fromisoformat": dump_fromisoformat(),
            "summary_metadata": summary_metadata_cases(),
            "is_summary_checkpoint": dump_is_summary_checkpoint(),
            "commit": dump_commit(),
            "get_history": dump_get_history(),
        }
        json.dump(doc, sys.stdout, indent=1, sort_keys=True, ensure_ascii=False)
        sys.stdout.write("\n")
    finally:
        shutil.rmtree(SCRATCH, ignore_errors=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
