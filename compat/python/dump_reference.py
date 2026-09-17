#!/usr/bin/env python3
"""Dump reference values from the frozen Python nanobot.

This is one half of the differential test harness. It emits a stable JSON
document of values that nanobot-go must reproduce. The Go half emits the same
shape, and scripts/compat.sh diffs them.

Run with the project venv:
    .tools/venv/bin/python compat/python/dump_reference.py

Every value here is read from the frozen upstream at
HKUDS/nanobot@1bb712d3488915ca4ed9ccc1a93067ff722f5ab9. Nothing is hardcoded
from documentation: if the upstream source changes, this output changes.
"""
from __future__ import annotations

import base64
import json
import os
import re
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

# Silence loguru: it writes diagnostics to stderr, and this harness must emit
# exactly one JSON document on stdout so the Go side can parse it.
try:
    from loguru import logger as _loguru_logger

    _loguru_logger.remove()
except Exception:
    pass
UPSTREAM = ROOT / "upstream" / "nanobot"
sys.path.insert(0, str(UPSTREAM))


def storage_key(key: str) -> str:
    """Reproduce JsonlSessionStore.storage_key (session/manager.py:1006)."""
    return base64.urlsafe_b64encode(key.encode()).decode().rstrip("=")


def dump_agent_defaults() -> dict:
    from nanobot.config.schema import AgentDefaults

    d = AgentDefaults()
    return {
        "workspace": d.workspace,
        "model": d.model,
        "provider": d.provider,
        "max_tokens": d.max_tokens,
        "context_window_tokens": d.context_window_tokens,
        "temperature": d.temperature,
        "max_tool_iterations": d.max_tool_iterations,
        "max_concurrent_subagents": d.max_concurrent_subagents,
        "max_tool_result_chars": d.max_tool_result_chars,
        "provider_retry_mode": d.provider_retry_mode,
        "tool_hint_max_length": d.tool_hint_max_length,
        "timezone": d.timezone,
        "timezone_mode": d.timezone_mode,
        "bot_name": d.bot_name,
        "bot_icon": d.bot_icon,
        "unified_session": d.unified_session,
        "session_ttl_minutes": d.session_ttl_minutes,
        "idle_compact_check_interval_seconds": d.idle_compact_check_interval_seconds,
    }


def dump_serialization() -> dict:
    """How the reference SERIALIZES a config, including alias choices."""
    from nanobot.config.schema import AgentDefaults

    d = AgentDefaults()
    # by_alias=True is what the loader writes back to disk.
    return json.loads(d.model_dump_json(by_alias=True))


def dump_alias_acceptance() -> dict:
    """Which input spellings the reference accepts for aliased fields.

    session_ttl_minutes is the interesting case: it accepts both
    idleCompactAfterMinutes and sessionTtlMinutes on input, but serializes as
    idleCompactAfterMinutes.
    """
    from nanobot.config.schema import AgentDefaults

    out = {}
    for spelling in ("sessionTtlMinutes", "session_ttl_minutes", "idleCompactAfterMinutes"):
        try:
            d = AgentDefaults.model_validate({spelling: 42})
            out[spelling] = d.session_ttl_minutes
        except Exception as exc:  # noqa: BLE001 - we want the failure recorded
            out[spelling] = f"REJECTED: {type(exc).__name__}"
    return out


def dump_storage_keys() -> dict:
    """Session key -> on-disk filename stem, for keys that exercise edge cases."""
    keys = [
        "cli:1",
        "telegram:42",
        "discord:guild:channel:thread",
        "slack:C123:1699999999.000100",
        "unicode:ção-日本-🎉",
        "a",
        "a:b:c:d:e",
    ]
    return {k: storage_key(k) for k in keys}


def dump_paths() -> dict:
    """Resolved runtime paths (config/paths.py)."""
    from nanobot.config import paths

    return {
        "workspace": str(paths.get_workspace_path()),
        "legacy_sessions_dir": str(paths.get_legacy_sessions_dir()),
        "cli_history_path": str(paths.get_cli_history_path()),
        "cron_dir": str(paths.get_cron_dir()),
        "logs_dir": str(paths.get_logs_dir()),
        "media_dir": str(paths.get_media_dir()),
        "webui_dir": str(paths.get_webui_dir()),
    }


def dump_retry_classification() -> list[dict]:
    """The reference's transient/retry verdict for a matrix of error shapes.

    The 429 cases are the interesting ones: the reference does NOT treat 429 as
    uniformly retryable, so a port that does would hammer a dead account.
    """
    from nanobot.providers.base import LLMProvider, LLMResponse

    cases = [
        {"name": "success", "finish_reason": "stop"},
        {"name": "500", "status": 500},
        {"name": "502", "status": 502},
        {"name": "503", "status": 503},
        {"name": "504", "status": 504},
        {"name": "408", "status": 408},
        {"name": "409", "status": 409},
        {"name": "400", "status": 400},
        {"name": "401", "status": 401},
        {"name": "403", "status": 403},
        {"name": "402", "status": 402},
        {"name": "404", "status": 404},
        {"name": "429-ratelimit-type", "status": 429, "error_type": "rate_limit_exceeded"},
        {"name": "429-ratelimit-code", "status": 429, "error_code": "rate_limit_error"},
        {"name": "429-quota-type", "status": 429, "error_type": "insufficient_quota"},
        {"name": "429-quota-text", "status": 429, "content": "insufficient_quota"},
        {"name": "429-billing-text", "status": 429, "content": "billing hard limit reached"},
        {"name": "429-unknown", "status": 429},
        {"name": "429-retryafter-text", "status": 429, "content": "rate limit, retry after 5s"},
        {"name": "kind-timeout", "error_kind": "timeout"},
        {"name": "kind-connection", "error_kind": "connection"},
        {"name": "text-503", "content": "upstream returned 503 server error"},
        {"name": "text-overloaded", "content": "provider overloaded"},
        {"name": "text-timedout", "content": "request timed out"},
        {"name": "text-nonsense", "content": "totally unknown failure"},
        {"name": "explicit-true", "status": 400, "error_should_retry": True},
        {"name": "explicit-false", "status": 503, "error_should_retry": False},
    ]

    out = []
    for case in cases:
        case = dict(case)
        name = case.pop("name")
        resp = LLMResponse(
            content=case.pop("content", None),
            finish_reason=case.pop("finish_reason", "error"),
            error_status_code=case.pop("status", None),
            error_kind=case.pop("error_kind", None),
            error_type=case.pop("error_type", None),
            error_code=case.pop("error_code", None),
            error_should_retry=case.pop("error_should_retry", None),
        )
        out.append({"name": name, "transient": bool(LLMProvider.is_transient_response(resp))})
    return out


def dump_governance() -> dict:
    """Run the reference history-repair functions over structural fixtures.

    Each fixture is a raw transcript containing a defect that makes upstream
    APIs reject a whole request. The expected output is whatever the reference
    produces, so the Go port can be compared case by case.
    """
    from nanobot.agent.context_governance import ContextGovernor

    def asst(content="", calls=None):
        m = {"role": "assistant", "content": content}
        if calls is not None:
            m["tool_calls"] = calls
        return m

    def call(cid, name):
        return {"id": cid, "type": "function",
                "function": {"name": name, "arguments": "{}"}}

    def tool(cid, content="ok", name="read_file"):
        return {"role": "tool", "tool_call_id": cid, "name": name, "content": content}

    user = lambda t: {"role": "user", "content": t}

    fixtures = {
        # A tool result whose declaring assistant turn is absent.
        "orphan_no_declaration": [user("hi"), tool("ghost")],
        # The same id fulfilled twice.
        "duplicate_tool_result": [
            user("hi"), asst(calls=[call("c1", "read_file")]),
            tool("c1"), tool("c1", "second"),
        ],
        # An assistant call that never received a result.
        "missing_tool_result": [
            user("hi"), asst(calls=[call("c1", "read_file")]),
        ],
        # A call with an empty function name.
        "malformed_empty_name": [
            user("hi"), asst(calls=[call("c1", "")]), tool("c1"),
        ],
        # A malformed call alongside a valid one.
        "malformed_and_valid": [
            user("hi"), asst(calls=[call("bad", ""), call("good", "read_file")]),
            tool("good"),
        ],
        # A malformed call that was the assistant's only content.
        "malformed_only_empty_turn": [
            user("hi"), asst("", calls=[call("bad", "")]),
        ],
        # A compaction placeholder replayed as a real turn.
        "placeholder_assistant": [
            user("hi"), asst("[Previous assistant message omitted.]"), user("again"),
        ],
        # A placeholder that still carries real tool calls must be kept.
        "placeholder_with_calls": [
            user("hi"), asst("[Previous assistant message omitted.]", calls=[call("c1", "read_file")]),
            tool("c1"),
        ],
        # A tool result appearing before its declaring assistant turn.
        "result_before_declaration": [
            user("hi"), tool("c1"), asst(calls=[call("c1", "read_file")]),
        ],
        # A tool message with no tool_call_id at all.
        "tool_without_id": [
            user("hi"), asst(calls=[call("c1", "read_file")]),
            {"role": "tool", "name": "read_file", "content": "x"},
        ],
        # A healthy transcript must be returned unchanged.
        "clean_history": [
            user("hi"), asst(calls=[call("c1", "read_file")]), tool("c1"),
            asst("done"),
        ],
        # Two consecutive user messages.
        "consecutive_users": [user("one"), user("two")],
    }

    out = {}
    for name, messages in fixtures.items():
        # prepare_for_model needs a config only for apply_tool_result_budget,
        # which is a no-op for these fixtures; call the repair steps directly
        # so the fixture matrix stays independent of workspace configuration.
        updated = ContextGovernor.strip_placeholder_assistant_messages(messages)
        updated = ContextGovernor.strip_malformed_tool_calls(updated)
        updated = ContextGovernor.drop_orphan_tool_results(updated)
        updated = ContextGovernor.backfill_missing_tool_results(updated)
        out[name] = {
            "input": messages,
            "output": updated,
            "changed": updated is not messages,
        }
    return out


def dump_budget() -> list[dict]:
    """InputBudget for a matrix of window/output combinations."""
    from nanobot.agent.context_governance import ContextGovernor, ContextGovernanceConfig

    cases = [
        {"window": 200000, "max_tokens": 8192},
        {"window": 200000, "max_tokens": 0},
        {"window": 8192, "max_tokens": 8192},
        {"window": 4096, "max_tokens": 8192},
        {"window": 0, "max_tokens": 8192},
        {"window": 16384, "max_tokens": 4096},
        {"window": 1024, "max_tokens": 4096},
        {"window": 12000, "max_tokens": 0},
    ]
    out = []
    for c in cases:
        cfg = ContextGovernanceConfig(
            provider=None, model="m", tools=None, workspace=None,
            session_key="s", max_tool_result_chars=16000,
            context_window_tokens=c["window"], max_tokens=c["max_tokens"],
        )
        out.append({**c, "budget": ContextGovernor.input_budget(cfg)})
    return out


def dump_tool_results() -> dict:
    """Reference behaviour for empty and oversized tool results."""
    from nanobot.agent.context_governance import ContextGovernor, ContextGovernanceConfig
    from nanobot.utils.helpers import _render_tool_result_reference, safe_filename
    from nanobot.utils.runtime import ensure_nonempty_tool_result

    # Empty-result markers, per tool name.
    empty = {}
    for name in ("read_file", "exec", "list_dir", ""):
        empty[name] = ensure_nonempty_tool_result(name, "")

    # Which inputs count as empty.
    emptiness = []
    for label, value in [
        ("none", None),
        ("empty_string", ""),
        ("whitespace", "   \n\t "),
        ("text", "real output"),
        ("empty_list", []),
        ("blank_blocks", [{"type": "text", "text": "  "}]),
        ("text_blocks", [{"type": "text", "text": "hello"}]),
        ("image_block", [{"type": "image_url", "image_url": {"url": "x"}}]),
    ]:
        got = ensure_nonempty_tool_result("exec", value)
        emptiness.append({"label": label, "result": got})

    # safe_filename over the characters the reference replaces.
    filenames = {name: safe_filename(name) for name in [
        "cli:1", "telegram:42", "a/b\\c", 'q<>:"|?*', "  spaced  ", "normal",
    ]}

    # The rendered reference text, including the max_chars fallback.
    rendered = []
    for label, kwargs in [
        ("plain", dict(reference_path="/w/a.txt", original_size=5000,
                       preview="abc", truncated_preview=True, max_chars=16000)),
        ("untruncated_preview", dict(reference_path="/w/a.txt", original_size=10,
                                     preview="abc", truncated_preview=False, max_chars=16000)),
        ("max_chars_overflow", dict(reference_path="/w/a.txt", original_size=5000,
                                    preview="abc", truncated_preview=True, max_chars=40)),
    ]:
        rendered.append({"label": label, "result": _render_tool_result_reference(**kwargs)})

    # normalize_tool_result end to end, with and without a workspace.
    configs = []
    for label, workspace in [("with_workspace", "/nonexistent-ws"), ("no_workspace", None)]:
        cfg = ContextGovernanceConfig(
            provider=None, model="m", tools=None, workspace=workspace,
            session_key="cli:1", max_tool_result_chars=50,
        )
        configs.append({
            "label": label,
            "empty": ContextGovernor.normalize_tool_result(cfg, "c1", "exec", ""),
            "read_file_exempt": ContextGovernor.normalize_tool_result(
                cfg, "c2", "read_file", "x" * 500),
        })

    return {
        "empty_markers": empty,
        "emptiness": emptiness,
        "safe_filenames": filenames,
        "rendered": rendered,
        "normalize": configs,
    }


def dump_dream() -> dict:
    """Dream memory-consolidation helpers (agent/memory.py:496-741) plus the
    two helpers raw_archive depends on.

    Everything here is pure: no session manager, no provider, no clock beyond
    dream_session_key's own shape. The corpus is adversarial on purpose — the
    reference's `_format_messages` stringifies with str(), so an int timestamp,
    a bool role and a multimodal content list all reach it, and the
    runtime-context marker's version test is a Python VALUE comparison where
    True == 1.

    The rendered Dream prompt is dumped alongside the template source and the
    substituted path so the Go side can prove both that its embedded copy is
    byte-identical to the upstream file and that its rstrip/substitution
    semantics match Jinja2's `render_template(..., strip=True)`.
    """
    import re as _re
    import shutil
    from types import SimpleNamespace

    from nanobot.agent.memory import MemoryStore, _RAW_ARCHIVE_MAX_CHARS
    from nanobot.agent.skills import BUILTIN_SKILLS_DIR
    from nanobot.runtime_context import public_history_message
    from nanobot.utils.helpers import content_with_media_breadcrumbs, image_placeholder_text

    out: dict = {}

    # -- build_dream_commit_message -----------------------------------------
    commit_cases = [
        ("dream: manual run", "SOUL.md: +1 -0"),
        ("dream: manual run", ""),
        ("dream: manual run", None),
        ("dream: manual run", "   "),
        ("dream: manual run", "\n\t "),
        ("dream: manual run", "  SOUL.md: +1 -0  "),
        ("dream: manual run", "line1\nline2"),
        ("dream: auto", "\u65e5\u672c\u8a9e: +2 -1"),
        ("", "body"),
        ("", ""),
    ]
    out["commit_messages"] = [
        {"prefix": prefix, "diff_body": body,
         "out": MemoryStore.build_dream_commit_message(prefix, body)}
        for prefix, body in commit_cases
    ]

    # -- dream_session_key ---------------------------------------------------
    key = MemoryStore.dream_session_key()
    head, _, tail = key.partition(":")
    out["session_key"] = {
        "prefix": head,
        "timestamp": tail,
        "timestamp_len": len(tail),
        "matches_format": bool(_re.fullmatch(r"\d{8}-\d{6}", tail)),
    }

    # -- default_dream_prompt ------------------------------------------------
    template_path = UPSTREAM / "nanobot" / "templates" / "agent" / "dream.md"
    out["prompt"] = {
        "template_source": template_path.read_text(encoding="utf-8"),
        "builtin_skills_dir": str(BUILTIN_SKILLS_DIR),
        "skill_creator_path": str(BUILTIN_SKILLS_DIR / "skill-creator" / "SKILL.md"),
        "rendered": MemoryStore.default_dream_prompt(),
    }

    # -- content_with_media_breadcrumbs --------------------------------------
    breadcrumb_cases = [
        ("user", "hello", ["/a.png"]),
        ("user", "hello", []),
        ("user", "", ["/a.png"]),
        ("user", "", []),
        ("user", "", [""]),
        ("user", "  ", ["/a.png"]),
        ("assistant", "hello", ["/a.png"]),
        ("tool", "hello", ["/a.png"]),
        ("", "hello", ["/a.png"]),
        ("user", ["block"], ["/a.png"]),
        ("user", None, ["/a.png"]),
        ("user", [], ["/a.png"]),
        ("user", 0, ["/a.png"]),
        ("user", "hello", "notalist"),
        ("user", "hello", None),
        ("user", "hello", [""]),
        ("user", "hello", [None]),
        ("user", "hello", [1]),
        ("user", "hello", ["a", "", "b"]),
        ("user", "hello", ["a", 1, "b"]),
    ]
    out["media_breadcrumbs"] = [
        {"role": role, "content": content, "media": media,
         "out": content_with_media_breadcrumbs(role, content, media)}
        for role, content, media in breadcrumb_cases
    ]
    out["image_placeholders"] = [
        {"path": path, "out": image_placeholder_text(path)}
        for path in [None, "", "/a.png", "0", " "]
    ]

    # -- _format_messages ----------------------------------------------------
    runtime_message = {
        "role": "user",
        "content": "what did I ask?\n\n[Runtime Context]",
        "timestamp": "2026-07-28T12:00:00",
        "_runtime_context": {"version": 1, "sources": ["goal_state"],
                             "suffix": "[Runtime Context]"},
    }
    format_corpus = [
        ("plain", [{"role": "user", "content": "hi", "timestamp": "2026-07-27T12:00:00"}]),
        ("missing_timestamp", [{"role": "user", "content": "hi"}]),
        ("null_timestamp", [{"role": "user", "content": "hi", "timestamp": None}]),
        ("int_timestamp", [{"role": "user", "content": "hi", "timestamp": 1720000000}]),
        ("bool_timestamp", [{"role": "user", "content": "hi", "timestamp": True}]),
        ("float_timestamp", [{"role": "user", "content": "hi", "timestamp": 1.5}]),
        ("list_timestamp", [{"role": "user", "content": "hi", "timestamp": [1, 2]}]),
        ("dict_timestamp", [{"role": "user", "content": "hi", "timestamp": {"a": 1}}]),
        ("empty_timestamp", [{"role": "user", "content": "hi", "timestamp": ""}]),
        ("long_timestamp", [{"role": "user", "content": "hi",
                             "timestamp": "2026-07-28T12:00:00.123456"}]),
        ("cjk_timestamp", [{"role": "user", "content": "hi",
                            "timestamp": "\u65e5\u672c\u8a9e" * 8}]),
        ("missing_role", [{"content": "no role", "timestamp": "2026-07-28T12:00:00"}]),
        ("empty_role", [{"role": "", "content": "x", "timestamp": "t"}]),
        ("null_role", [{"role": None, "content": "x", "timestamp": "t"}]),
        ("int_role", [{"role": 5, "content": "x", "timestamp": "t"}]),
        ("bool_role", [{"role": True, "content": "x", "timestamp": "t"}]),
        ("zero_role", [{"role": 0, "content": "x", "timestamp": "t"}]),
        ("tools_used", [{"role": "assistant", "content": "hi",
                         "tools_used": ["read_file", "exec"],
                         "timestamp": "2026-07-28T12:00:00"}]),
        ("tools_used_empty", [{"role": "assistant", "content": "hi",
                               "tools_used": [], "timestamp": "t"}]),
        ("tools_used_string", [{"role": "assistant", "content": "hi",
                                "tools_used": "ab", "timestamp": "t"}]),
        ("tools_used_one", [{"role": "assistant", "content": "hi",
                             "tools_used": ["exec"], "timestamp": "t"}]),
        ("media_only", [{"role": "user", "content": "", "media": ["/m/a.png"],
                         "timestamp": "2026-07-27"}]),
        ("media_and_content", [{"role": "user", "content": "hi", "media": ["/m/a.png"],
                                "timestamp": "2026-07-27"}]),
        ("media_non_list", [{"role": "user", "content": "hi", "media": "/m/a.png",
                             "timestamp": "t"}]),
        ("media_empty_entries", [{"role": "user", "content": "hi", "media": ["", "/m/a.png"],
                                  "timestamp": "t"}]),
        ("media_non_string", [{"role": "user", "content": "hi", "media": [None, 1, "/m/a.png"],
                               "timestamp": "t"}]),
        ("media_non_user", [{"role": "assistant", "content": "hi", "media": ["/m/a.png"],
                             "timestamp": "t"}]),
        ("runtime_context", [runtime_message]),
        ("empty_content", [{"role": "user", "content": "", "timestamp": "t"}]),
        ("null_content", [{"role": "user", "content": None, "timestamp": "t"}]),
        ("empty_list_content", [{"role": "user", "content": [], "timestamp": "t"}]),
        ("zero_content", [{"role": "user", "content": 0, "timestamp": "t"}]),
        ("multimodal_single_key", [{"role": "user", "content": [{"type": "text"}],
                                    "timestamp": "t"}]),
        ("multimodal_scalars", [{"role": "user", "content": ["a", "b"], "timestamp": "t"}]),
        ("multimodal_escapes", [{"role": "user", "content": [{"a'b": "x\ny\tz\\w"}],
                                 "timestamp": "t"}]),
        ("multimodal_nested", [{"role": "user",
                                "content": [{"type": "text", "n": 1, "f": 1.5,
                                             "b": True, "nul": None}],
                                "timestamp": "t"}]),
        ("newlines", [{"role": "user", "content": "hi\nsecond", "timestamp": "t"}]),
        ("skipped_and_kept", [
            {"role": "user", "content": "", "timestamp": "t"},
            {"role": "tool", "content": "result", "timestamp": "t2"},
            {"role": "assistant", "content": "done", "timestamp": "t3"},
        ]),
        ("empty_batch", []),
    ]
    out["format_messages"] = [
        {"label": label, "messages": messages, "out": MemoryStore._format_messages(messages)}
        for label, messages in format_corpus
    ]

    # -- _build_raw_checkpoint ----------------------------------------------
    # PID-suffixed like GITSTORE_TMP: two compat runs in parallel would
    # otherwise rmtree each other's scratch workspace mid-section.
    ws = ROOT / ".tools" / "tmp" / f"dream-ref-ws-{os.getpid()}"
    if ws.exists():
        shutil.rmtree(ws)
    ws.mkdir(parents=True)
    store = MemoryStore(ws)

    big = [{"role": "user", "content": "x" * 20000, "timestamp": "2026-07-28T12:00:00"}]
    at_cap = [{"role": "user", "content": "y" * (16000 - len("[RAW] 1 messages\n[2026-07-28T12:00] USER: ")),
               "timestamp": "2026-07-28T12:00:00"}]
    thinky = [{"role": "assistant", "content": "<think>hidden</think>visible", "timestamp": "t"}]
    checkpoint_cases = [
        ("empty", [], None),
        ("plain", format_corpus[0][1], None),
        ("skipped_only", [{"role": "user", "content": "", "timestamp": "t"}], None),
        ("over_cap", big, None),
        ("at_cap", at_cap, None),
        ("custom_cap", format_corpus[0][1], 40),
        ("custom_cap_zero", big, 0),
        ("custom_cap_over_hard", big, 100000),
        ("runtime_context_excluded", [runtime_message], None),
        ("think_stripped", thinky, None),
        ("media", format_corpus[22][1], None),
    ]
    out["raw_checkpoints"] = [
        {"label": label, "messages": messages, "max_chars": max_chars,
         "out": store._build_raw_checkpoint(messages, max_chars=max_chars)}
        for label, messages, max_chars in checkpoint_cases
    ]
    out["raw_archive_max_chars"] = _RAW_ARCHIVE_MAX_CHARS

    # -- build_dream_prompt --------------------------------------------------
    # The journal is written directly with fixed timestamps so the two sides
    # compare the assembled prompt without racing the clock.
    prompt_ws = ROOT / ".tools" / "tmp" / f"dream-prompt-ws-{os.getpid()}"
    if prompt_ws.exists():
        shutil.rmtree(prompt_ws)
    (prompt_ws / "memory").mkdir(parents=True)
    history_lines = [
        {"cursor": 1, "timestamp": "2026-01-01 10:00", "content": "first entry"},
        {"cursor": 2, "timestamp": "2026-01-01 10:01",
         "content": "<think>hidden</think>visible entry"},
        {"cursor": 3, "timestamp": "2026-01-01 10:02", "content": "x" * 2000},
    ]
    history_doc = "".join(json.dumps(line, ensure_ascii=False) + "\n" for line in history_lines)
    (prompt_ws / "memory" / "history.jsonl").write_text(history_doc, encoding="utf-8")

    prompt_store = MemoryStore(prompt_ws)
    prompt_cases = []
    for label, max_entries, last_cursor in [
        ("default", 20, 0),
        ("limited", 2, 0),
        ("limited_to_one", 1, 0),
        ("negative_drops_tail", -1, 0),
        ("nothing_unprocessed", 20, 3),
        ("partially_consumed", 20, 1),
        ("zero_entries", 0, 0),
        ("negative_past_start", -5, 0),
    ]:
        prompt_store.set_last_dream_cursor(last_cursor)
        record = {"label": label, "max_entries": max_entries, "last_cursor": last_cursor,
                  "out": None, "cursor": None, "error": None}
        try:
            result = prompt_store.build_dream_prompt(max_entries=max_entries)
        except Exception as exc:  # noqa: BLE001 - the failure itself is the expectation
            record["error"] = type(exc).__name__
        else:
            if result is not None:
                record["out"] = result[0]
                record["cursor"] = result[1]
        prompt_cases.append(record)
    out["build_dream_prompt"] = prompt_cases
    out["prompt_history_jsonl"] = history_doc

    # -- public_history_message ---------------------------------------------
    suffix = "[Runtime Context]"
    blocks = [{"type": "text", "text": "injected"}]
    public_cases = [
        ("no_marker", {"role": "user", "content": "hi"}),
        ("marker_not_a_dict", {"role": "user", "content": "hi", "_runtime_context": "x"}),
        ("marker_list", {"role": "user", "content": "hi", "_runtime_context": [1]}),
        ("marker_no_version", {"role": "user", "content": "hi",
                               "_runtime_context": {"suffix": suffix}}),
        ("version_2", {"role": "user", "content": "hi\n\n" + suffix,
                       "_runtime_context": {"version": 2, "suffix": suffix}}),
        ("version_true", {"role": "user", "content": "hi\n\n" + suffix,
                          "_runtime_context": {"version": True, "suffix": suffix}}),
        ("version_float", {"role": "user", "content": "hi\n\n" + suffix,
                           "_runtime_context": {"version": 1.0, "suffix": suffix}}),
        ("version_string", {"role": "user", "content": "hi\n\n" + suffix,
                            "_runtime_context": {"version": "1", "suffix": suffix}}),
        ("suffix_exact", {"role": "user", "content": suffix,
                          "_runtime_context": {"version": 1, "suffix": suffix}}),
        ("suffix_tail", {"role": "user", "content": "hi\n\n" + suffix,
                         "_runtime_context": {"version": 1, "suffix": suffix}}),
        ("suffix_absent_from_content", {"role": "user", "content": "hi",
                                        "_runtime_context": {"version": 1, "suffix": suffix}}),
        ("suffix_empty", {"role": "user", "content": "hi\n\n" + suffix,
                          "_runtime_context": {"version": 1, "suffix": ""}}),
        ("suffix_non_string", {"role": "user", "content": "hi\n\n" + suffix,
                               "_runtime_context": {"version": 1, "suffix": 5}}),
        ("suffix_only_single_newline", {"role": "user", "content": "hi\n" + suffix,
                                        "_runtime_context": {"version": 1, "suffix": suffix}}),
        ("blocks_tail", {"role": "user", "content": [{"type": "text", "text": "real"}] + blocks,
                         "_runtime_context": {"version": 1, "blocks": blocks}}),
        ("blocks_exact", {"role": "user", "content": list(blocks),
                          "_runtime_context": {"version": 1, "blocks": blocks}}),
        ("blocks_not_at_tail", {"role": "user", "content": list(blocks) + [{"type": "text", "text": "real"}],
                                "_runtime_context": {"version": 1, "blocks": blocks}}),
        ("blocks_empty_expected", {"role": "user", "content": [{"type": "text", "text": "real"}],
                                   "_runtime_context": {"version": 1, "blocks": []}}),
        ("blocks_longer_than_content", {"role": "user", "content": [{"type": "text", "text": "injected"}],
                                        "_runtime_context": {"version": 1,
                                                             "blocks": blocks + [{"type": "text", "text": "more"}]}}),
        ("blocks_two", {"role": "user", "content": [{"type": "text", "text": "real"}] + blocks,
                        "_runtime_context": {"version": 1, "blocks": blocks}}),
        ("blocks_number_equality", {"role": "user", "content": [{"type": "text", "n": 1}],
                                    "_runtime_context": {"version": 1,
                                                         "blocks": [{"type": "text", "n": 1.0}]}}),
        ("content_absent", {"role": "user", "_runtime_context": {"version": 1, "suffix": suffix}}),
        ("content_null", {"role": "user", "content": None,
                          "_runtime_context": {"version": 1, "suffix": suffix}}),
        ("marker_null", {"role": "user", "content": "hi", "_runtime_context": None}),
        ("other_keys_kept", {"role": "user", "content": "hi", "timestamp": "t",
                             "tools_used": ["a"], "_runtime_context": {"version": 1, "suffix": suffix}}),
    ]
    out["public_history"] = [
        {"label": label, "message": message, "out": public_history_message(message)}
        for label, message in public_cases
    ]

    # -- dream_run_completed / dream_incompletion_reason ---------------------
    run_cases = []
    for label, kind, metadata in [
        ("completed", "metadata", {"_stop_reason": "completed"}),
        ("max_iterations", "metadata", {"_stop_reason": "max_iterations"}),
        ("null_stop_reason", "metadata", {"_stop_reason": None}),
        ("int_stop_reason", "metadata", {"_stop_reason": 123}),
        ("bool_stop_reason", "metadata", {"_stop_reason": True}),
        ("empty_metadata", "metadata", {}),
        ("other_keys", "metadata", {"other": 1}),
        ("metadata_none", "metadata_none", None),
        ("metadata_list", "metadata_list", [1, 2]),
        ("no_metadata_attribute", "no_metadata", None),
        ("no_response", "no_response", None),
    ]:
        if kind == "no_response":
            resp = None
        elif kind == "no_metadata":
            resp = SimpleNamespace()
        else:
            resp = SimpleNamespace(metadata=metadata)
        run_cases.append({
            "label": label,
            "kind": kind,
            "metadata": metadata,
            "completed": MemoryStore.dream_run_completed(resp),
            "reason": MemoryStore.dream_incompletion_reason(resp),
        })
    out["run_status"] = run_cases

    return out


def dump_pyjson() -> dict:
    """How Python's json.dumps renders floats and strings.

    These are the two places where Go's encoder disagrees by default, so the
    exact strings are captured rather than described. Inputs are emitted as
    real JSON values so the Go side does not have to reverse-engineer a repr().
    """
    import json as _json

    def encoded(value):
        return _json.dumps({"v": value}, ensure_ascii=False)[6:-1]

    floats = []
    for value in [1.0, 0.1, 2.5, 100.0, 1e15, 1e16, 1e17, 1e20, 1e-4, 1e-5,
                  1e-7, 0.0, -0.0, -1.5, 3.141592653589793, 1.5e300,
                  float("inf"), float("-inf"), float("nan")]:
        # repr() is used as the key because json.dumps emits bare
        # Infinity/NaN, which is not valid JSON and would make the
        # reference file unparseable.
        floats.append({"repr": repr(value), "encoded": encoded(value)})

    strings = []
    for value in ["a < b & c > d", "line\u2028sep", "line\u2029sep",
                  'quote"and\\slash', "unicode \u2713 \u65e5\u672c",
                  "tab\there", "literal \\u2028 text"]:
        strings.append({"value": value, "encoded": encoded(value)})

    return {"floats": floats, "strings": strings}


def dump_length_recovery() -> dict:
    """_restore_outer_whitespace and build_length_recovery_message.

    The whitespace function is surprising enough to be worth pinning: passing
    content == original doubles the boundary whitespace, because the base hook's
    finalize_content is the identity and nothing was ever stripped.
    """
    from nanobot.agent.runner import _restore_outer_whitespace
    from nanobot.utils.runtime import build_length_recovery_message

    cases = [
        ("The history of ", "The history of "),
        ("b", "  b  "),
        ("b", "b"),
        (" b ", " b "),
        ("b", ""),
        ("x", "\n x \n"),
        ("", "  pad  "),
    ]
    restore = [
        {"content": c, "original": o, "result": _restore_outer_whitespace(c, o)}
        for c, o in cases
    ]

    messages = [
        {"content": c, "message": build_length_recovery_message(c)}
        for c in ["short", "x" * 200, "\u65e5" * 100]
    ]

    return {"restore": restore, "messages": messages}


def dump_strip_think() -> list:
    """strip_think over an adversarial corpus.

    The reference uses a backreference and a negative lookahead, neither of
    which Go's RE2 supports, so both were rewritten. The corpus exists to prove
    the rewrites are equivalent, including the cases where the alternation
    order and the ASCII-only character class decide the outcome.
    """
    from nanobot.utils.helpers import strip_think

    corpus = [
        "plain text",
        "  padded  ",
        "<think>hidden</think>visible",
        "before<think>hidden</think>after",
        "<thinking>a</thinking>b",
        "<thought>x</thought>y",
        "<think>unclosed and never ends",
        "<thinking>streaming prefix",
        "<thinking/>marker",
        "text<thinking/>",
        "a<thinking/>b",
        "<think\u5e7f\u573a leaked",
        "<thinking\u5e7f\u573a leaked",
        "<thought\u5e7f\u573a leaked",
        "<think>closed</think>",
        "<think>x</thought>mismatched",
        "</think>orphan at start",
        "orphan at end</think>",
        "mid </think> text",
        "</thinking>a",
        "a</thinking>",
        "<|channel|>text",
        "<channel|>text",
        "<|channel|>",
        "a <|channel|> b",
        "<thi", "<thin", "<tho", "<think", "<think>", "<", "<|",
        "text <",
        "\u65e5\u672c\u8a9e<think>x</think>\u6f22\u5b57",
        "<think>a</think><think>b</think>",
        "<think>nested<think>inner</think></think>tail",
        "<THINK>uppercase</THINK>",
        "<Think>mixed</Think>",
        "<think >space</think >",
        "<think\n>newline</think\n>",
        "", " ",
        "<think></think>",
        "<thinking></thinking>",
        "<thought>multi\nline</thought>tail",
        "keep <this> intact",
        "<thinking>x</thinking>",
        "a<thinking>b",
        "<thought>unclosed",
        "a<thought",
        "<thinker>not a tag</thinker>",
        "<thinking_x>underscore</thinking_x>",
        "<think-a>dash</think-a>",
        "<think:a>colon</think:a>",
        "<think/>selfclosed",
        "x<think/>y",
        "<|channel>text",
        "<channel>text",
        "text</thought>",
        "<thi>partial",
        "<t>partial",
    ]
    return [{"in": c, "out": strip_think(c)} for c in corpus]


def dump_truncate_text() -> list:
    """truncate_text over ASCII, CJK, emoji and boundary lengths.

    The unit is characters, not bytes, and the result is longer than the limit
    by the length of the suffix. Both facts are easy to get wrong and neither is
    obvious from the signature.
    """
    from nanobot.utils.helpers import truncate_text

    cases = []
    for text, limit in [
        ("Z" * 5000, 200),
        ("\u65e5" * 100, 50),
        ("\u65e5" * 100, 100),
        ("\U0001f600" * 50, 10),
        ("abc", 10),          # under the limit: unchanged
        ("abc", 3),           # exactly at the limit: unchanged
        ("abcd", 3),          # one over
        ("", 10),
        ("abc", 0),           # non-positive limit: unchanged
        ("abc", -1),
        ("\u65e5", 1),         # single multibyte char at the limit
        ("\u65e5\u672c", 1),    # cut between two multibyte chars
        ("a\u65e5b", 2),
    ]:
        cases.append({"text": text, "limit": limit, "out": truncate_text(text, limit)})
    return cases


def dump_pystr() -> dict:
    """Python's str.strip()/rstrip()/lstrip() whitespace definition.

    Go's strings.TrimSpace and unicode.IsSpace are close to Python's
    str.isspace() but NOT identical: Python also accepts U+001C..U+001F
    (FILE/GROUP/RECORD/UNIT SEPARATOR), which Unicode's White_Space property
    excludes. Any port that reaches for strings.TrimSpace diverges there, and
    the divergence is invisible until a separator character shows up in pasted
    terminal output.

    The full code point set is pinned by enumeration rather than transcribed,
    so the port cannot drift from the interpreter that actually defines it.
    """
    ws = [chr(cp) for cp in range(0x110000) if chr(cp).isspace()]
    non_ws = ["\u200b", "\ufeff", "\u180e", "\u00b7", "a"]

    probes: list[str] = []
    for ch in ws:
        probes.append("a" + ch)
        probes.append(ch + "a")
        probes.append("a" + ch + "a")
    probes.append("".join(ws))
    probes.append("".join(ws) + "x" + "".join(ws))
    for ch in non_ws:
        probes.append("a" + ch)
        probes.append(ch + "a")
    probes.extend([
        "",
        "   ",
        "\t\n mixed \r\n",
        "\u65e5\u672c\u8a9e \u30c6\u30b9\u30c8 ",
        " \u65e5\u672c\u8a9e ",
        "a\u00a0\u3000",
        "\u3000\u3000",
        "no-whitespace-here",
    ])

    return {
        "whitespace_codepoints": [ord(c) for c in ws],
        "cases": [
            {
                "in": p,
                "strip": p.strip(),
                "rstrip": p.rstrip(),
                "lstrip": p.lstrip(),
            }
            for p in probes
        ],
    }


# ---------------------------------------------------------------------------
# GitStore
# ---------------------------------------------------------------------------

# The scenario below is mirrored exactly by compat/gitstore_differential_test.go.
# Both sides use the same fixed workspace paths so that the Python workspace is
# still on disk when the Go test runs, and the Go test can compare the two
# repositories with the git CLI as an independent oracle.
# Every gitstore scenario path lives under one directory, which is per-process by
# default: the compat suite can be run by more than one process at a time in this
# workspace, and a shared scenario directory makes two runs delete each other's
# repositories mid-scenario. GITSTORE_SCRATCH_DIR overrides it, which lets
# compat/gitstore_differential_test.go point its own dumper run at a directory it
# can find again (instead of guessing this process's pid). The recorded values
# never contain these paths (they are normalised away by _norm), so the directory
# name does not affect the comparison.
#
# WHERE THE DEFAULT LIVES MATTERS. The reference's GitStore.init() calls
# _is_inside_git_repo(), which walks UP from the workspace and declines to
# initialize when any ancestor holds a .git entry. A scratch directory inside the
# project checkout — which is what ROOT/.tools/tmp is — therefore makes EVERY
# scenario workspace "already inside a repository": init() writes no .git at all
# and the dump dies reading .git/info/exclude. The default must be outside any
# repository, and the "workspace inside another repository" case below still
# exercises the guard on purpose with its own hand-made .git.
def _gitstore_scratch_dir() -> Path:
    override = os.environ.get("GITSTORE_SCRATCH_DIR")
    if override:
        return Path(override)
    return Path(tempfile.gettempdir()) / f"haosbot-gitstore-ref-{os.getpid()}"


GITSTORE_TMP = _gitstore_scratch_dir()
GITSTORE_WS = GITSTORE_TMP / "ws"
GITSTORE_UNBORN_WS = GITSTORE_TMP / "unborn"
GITSTORE_OUTSIDE = GITSTORE_TMP / "outside.txt"
GITSTORE_FILE = GITSTORE_TMP / "file"
GITSTORE_TRACKED = ["SOUL.md", "USER.md", "memory/MEMORY.md", "memory/.dream_cursor"]


def _norm(text, *extra):
    """Replace the volatile parts of a value.

    Commit SHAs depend on the commit timestamp, which the reference reads from
    the clock (dulwich's porcelain.commit calls time.time()); the two sides of a
    differential run never execute in the same second, so commit ids cannot be
    compared literally. Everything else in the recorded values is deterministic:
    blob ids, tree contents (compared through the git oracle), summaries, patches
    and error texts. The workspace path is replaced because the two sides use
    different directories.
    """
    out = str(text)
    for value, token in [(str(GITSTORE_WS), "<WS>"), (str(GITSTORE_UNBORN_WS), "<UNBORN>"),
                         (str(GITSTORE_OUTSIDE), "<OUT>"), (str(GITSTORE_FILE), "<FILE>")] + list(extra):
        out = out.replace(value, token)
    # Commit timestamps come from the clock, and the two sides of a differential
    # run are seconds apart: a run that crosses a minute boundary would otherwise
    # report a spurious mismatch. The shape of the timestamp is pinned separately
    # (timestamp_len / timestamp_shape).
    return re.sub(r"\d{4}-\d{2}-\d{2} \d{2}:\d{2}", "<TS>", out)


def _read(path: Path) -> str:
    try:
        return path.read_text(encoding="utf-8", errors="replace")
    except OSError as exc:
        return f"<{type(exc).__name__}>"


def _git_entries(root: Path) -> list:
    """Sorted .git paths, with loose object directories collapsed."""
    out = []
    for p in sorted(root.rglob("*")):
        rel = p.relative_to(root).as_posix()
        parts = rel.split("/")
        # Loose objects are named by their content hash, so they are recorded by
        # count instead of by name.
        if parts[0] == "objects" and len(parts) >= 2 and len(parts[1]) == 2:
            continue
        out.append(rel + ("/" if p.is_dir() else ""))
    return out


def _call(fn, *args, **kwargs):
    """Call fn and return either its value or its exception, as JSON."""
    try:
        return {"ok": True, "value": fn(*args, **kwargs)}
    except Exception as exc:  # noqa: BLE001 - the error text is the observation
        return {"ok": False, "type": type(exc).__name__, "message": _norm(exc)}


def dump_gitstore() -> dict:
    """GitStore over a real repository, exercising every public method.

    Three properties are being pinned here, none of which is obvious from the
    signature:

    - the reference does not shell out to git (it uses dulwich), so every value
      below is produced with PATH emptied;
    - summarize_working_tree's exact text, which memory.dream_content_diff
      compares and the Dream prompt embeds;
    - the degradation paths: uninitialized, unborn HEAD, corrupt index, missing
      object, unreadable path.
    """
    import shutil

    from nanobot.utils.gitstore import GitStore

    for path in (GITSTORE_WS, GITSTORE_UNBORN_WS):
        if path.exists():
            shutil.rmtree(path)
        path.mkdir(parents=True)
    GITSTORE_OUTSIDE.write_text("outside line\n", encoding="utf-8")

    # Prove the reference never needs the git binary: everything below runs with
    # an empty PATH, and dulwich is pure Python.
    saved_path = os.environ.get("PATH", "")
    os.environ["PATH"] = ""
    try:
        return _dump_gitstore_body(GitStore, GITSTORE_WS, GITSTORE_UNBORN_WS)
    finally:
        os.environ["PATH"] = saved_path


def _dump_gitstore_body(GitStore, ws, unborn_ws) -> dict:
    import shutil

    out = {}

    # -- 1. init ------------------------------------------------------------
    gs = GitStore(ws, GITSTORE_TRACKED)
    out["is_initialized_before"] = gs.is_initialized()
    out["init_created"] = _call(gs.init)
    out["init_again"] = _call(gs.init)
    out["gitignore"] = _read(ws / ".gitignore")
    out["head_file"] = _read(ws / ".git" / "HEAD")
    out["config_file"] = _read(ws / ".git" / "config")
    out["description_file"] = _read(ws / ".git" / "description")
    out["exclude_file_len"] = len((ws / ".git" / "info" / "exclude").read_bytes())
    out["git_entries"] = _git_entries(ws / ".git")
    out["is_initialized_after"] = gs.is_initialized()
    out["gitignore_other_tracked"] = GitStore(ws, ["SOUL.md"])._build_gitignore()
    out["gitignore_flat"] = GitStore(ws, ["a.md", "b.md"])._build_gitignore()

    init_log = gs.log()
    out["init_log"] = [
        {
            "subject": c.subject(),
            "message": c.message,
            "sha_len": len(c.sha),
            "sha_is_hex": all(ch in "0123456789abcdef" for ch in c.sha),
            "timestamp_len": len(c.timestamp),
            "timestamp_shape": _timestamp_shape(c.timestamp),
            "format": _norm(c.format(""), (c.sha, "<SHA>")),
        }
        for c in init_log
    ]
    out["summary_clean"] = gs.summarize_working_tree(
        ["SOUL.md", "USER.md", "memory/MEMORY.md"]
    )

    # -- 2. working-tree summary formats ------------------------------------
    (ws / "SOUL.md").write_text("hello\nworld\n", encoding="utf-8")
    (ws / "USER.md").write_text("user line\n", encoding="utf-8")
    summary_paths = ["SOUL.md", "USER.md", "memory/MEMORY.md"]
    out["summary_added"] = gs.summarize_working_tree(summary_paths)
    out["summary_duplicate"] = gs.summarize_working_tree(["SOUL.md", "SOUL.md"])
    out["summary_dot_prefix"] = gs.summarize_working_tree(["./SOUL.md"])
    out["summary_missing_tracked"] = gs.summarize_working_tree(["memory/MEMORY.md"])
    out["summary_absolute"] = _norm(gs.summarize_working_tree([str(GITSTORE_OUTSIDE)]))
    out["summary_empty_list"] = gs.summarize_working_tree([])

    # -- 3. commit, log, diff ------------------------------------------------
    out["commit_second"] = _sha_shape(gs.auto_commit("dream: second"))
    out["summary_after_commit"] = gs.summarize_working_tree(summary_paths)
    out["commit_clean"] = _sha_shape(gs.auto_commit("dream: second"))

    log = gs.log()
    out["log_subjects"] = [c.subject() for c in log]
    out["log_zero"] = gs.log(max_entries=0)
    out["log_negative"] = gs.log(max_entries=-1)
    out["log_one"] = [c.subject() for c in gs.log(max_entries=1)]
    out["log_prefix_match"] = [c.subject() for c in gs.log(message_prefix="dream:")]
    out["log_prefix_none_match"] = [c.subject() for c in gs.log(message_prefix="zzz")]
    out["log_prefix_empty"] = [c.subject() for c in gs.log(message_prefix="")]

    init_sha, second_sha = log[-1].sha, log[0].sha
    out["diff_init_second"] = _norm(gs.diff_commits(init_sha, second_sha))
    out["diff_same"] = gs.diff_commits(second_sha, second_sha)
    out["diff_unknown"] = gs.diff_commits("deadbeef", second_sha)
    out["diff_reversed"] = _norm(gs.diff_commits(second_sha, init_sha))

    shown = _call(gs.show_commit_diff, second_sha[:8], 20, None)
    if shown["ok"] and shown["value"] is not None:
        info, diff = shown["value"]
        shown = {"ok": True, "value": {"subject": info.subject(), "sha_len": len(info.sha),
                                       "diff": _norm(diff)}}
    out["show_commit_diff"] = shown
    out["show_commit_diff_no_match"] = _call(gs.show_commit_diff, "ffffffff", 20, None)
    out["show_commit_diff_prefix_filter"] = _call(
        gs.show_commit_diff, second_sha[:8], 20, "zzz"
    )

    # -- 4. content shapes ---------------------------------------------------
    # The same text with CRLF line endings is NOT reported: the comparison
    # normalises CRLF to LF before deciding (gitstore.py:364-366).
    (ws / "SOUL.md").write_text("hello\r\nworld\r\n", encoding="utf-8")
    out["summary_crlf_equal"] = gs.summarize_working_tree(["SOUL.md"])
    # A lone CR is a line break for str.splitlines(), so the split lines match
    # the committed ones and the change is counted with an empty diff block.
    (ws / "SOUL.md").write_text("hello\rworld\r", encoding="utf-8")
    out["summary_cr_only"] = gs.summarize_working_tree(["SOUL.md"])
    (ws / "SOUL.md").write_text("hello\rworld\r\n", encoding="utf-8")
    out["summary_crlf_partial"] = gs.summarize_working_tree(["SOUL.md"])
    (ws / "SOUL.md").write_bytes(b"hello\nworld")
    out["summary_no_trailing_newline"] = gs.summarize_working_tree(["SOUL.md"])
    (ws / "SOUL.md").write_text("hello\nworld\n", encoding="utf-8")

    (ws / "USER.md").write_bytes(b"\xff\xfe\x00binary\n")
    out["summary_binary"] = gs.summarize_working_tree(summary_paths)
    (ws / "USER.md").write_text("user line\n", encoding="utf-8")

    # str.splitlines() breaks on more than \n and \r: \v \f \x1c \x1d \x1e \x85
    # U+2028 U+2029 all end a line. The diff below is only correct if the Go
    # port splits identically.
    (ws / "SOUL.md").write_text(
        "A\x0bB\x0cC\x1cD\x1dE\x1eF\x85G\u2028H\u2029I\n", encoding="utf-8"
    )
    out["summary_exotic_separators"] = gs.summarize_working_tree(["SOUL.md"])
    (ws / "SOUL.md").write_text("hello\nworld\n", encoding="utf-8")

    # A committed blob that is not valid UTF-8: the HEAD side is decoded with
    # errors="replace", which collapses a truncated multi-byte sequence into ONE
    # replacement character (the Unicode "maximal subpart" rule) — not one per
    # byte, which is what a naive byte-wise decoder produces.
    (ws / "USER.md").write_bytes(b"\xf0\x9f\x98ok\n")
    gs.auto_commit("dream: invalid utf8 blob")
    (ws / "USER.md").write_text("plain\n", encoding="utf-8")
    out["summary_invalid_utf8_head"] = gs.summarize_working_tree(["USER.md"])
    (ws / "USER.md").write_text("user line\n", encoding="utf-8")

    # Two hunks separated by a long unchanged run: exercises the diff grouping
    # (three lines of context) rather than a single hunk.
    body_head = [f"line {i}\n" for i in range(60)]
    body_new = list(body_head)
    body_new[3] = "changed early\n"
    body_new[56] = "changed late\n"
    (ws / "SOUL.md").write_text("".join(body_head), encoding="utf-8")
    gs.auto_commit("dream: grouping base")
    (ws / "SOUL.md").write_text("".join(body_new), encoding="utf-8")
    out["summary_two_hunks"] = gs.summarize_working_tree(["SOUL.md"])

    # 300 lines with one value repeated far more often than the autojunk
    # heuristic allows (n >= 200, popular = count > n // 100 + 1): difflib drops
    # those elements from its matching, which changes the diff.
    popular = ["x\n"] * 250 + [f"u{i}\n" for i in range(50)]
    popular_new = ["x\n"] * 250 + [f"v{i}\n" for i in range(50)]
    (ws / "SOUL.md").write_text("".join(popular), encoding="utf-8")
    gs.auto_commit("dream: autojunk base")
    (ws / "SOUL.md").write_text("".join(popular_new), encoding="utf-8")
    out["summary_autojunk"] = gs.summarize_working_tree(["SOUL.md"])
    (ws / "SOUL.md").write_text("hello\nworld\n", encoding="utf-8")

    (ws / "SOUL.md").write_text("".join(f"line {i}\n" for i in range(1000)), encoding="utf-8")
    truncated = gs.summarize_working_tree(["SOUL.md"])
    out["summary_truncated_len"] = len(truncated)
    out["summary_truncated_tail"] = truncated[-60:]
    out["summary_truncated_head"] = truncated[:120]
    (ws / "SOUL.md").write_text("hello\nworld\n", encoding="utf-8")

    # -- 5. revert -----------------------------------------------------------
    revert_sha = gs.revert(second_sha)
    out["revert_created"] = _sha_shape(revert_sha)
    out["revert_soul"] = _read(ws / "SOUL.md")
    out["revert_user"] = _read(ws / "USER.md")
    out["revert_summary"] = gs.summarize_working_tree(summary_paths)
    out["revert_message"] = _norm(gs.log()[0].message, (second_sha, "<SHA>"))
    out["revert_root"] = gs.revert(init_sha)
    out["revert_prefix_no_match"] = gs.revert(second_sha, message_prefix="zzz")
    out["revert_unknown"] = gs.revert("ffffffff")

    # -- 5b. patch shapes ----------------------------------------------------
    # One commit that adds a file with no trailing newline, flips its mode and
    # deletes another file: the three header forms plus git's
    # "\ No newline at end of file" marker.
    (ws / "SOUL.md").write_text("hello\nworld", encoding="utf-8")
    os.chmod(ws / "SOUL.md", 0o755)
    (ws / "USER.md").unlink()
    out["commit_shapes"] = _sha_shape(gs.auto_commit("dream: shapes"))
    shapes_log = gs.log()
    out["diff_shapes"] = _norm(gs.diff_commits(shapes_log[1].sha, shapes_log[0].sha))
    out["summary_shapes"] = gs.summarize_working_tree(summary_paths)

    # -- 6. store that was never initialized --------------------------------
    blank = GITSTORE_TMP / "blank"
    if blank.exists():
        shutil.rmtree(blank)
    blank.mkdir(parents=True)
    blank_gs = GitStore(blank, GITSTORE_TRACKED)
    out["uninitialized"] = {
        "is_initialized": blank_gs.is_initialized(),
        "auto_commit": blank_gs.auto_commit("x"),
        "log": blank_gs.log(),
        "diff": blank_gs.diff_commits("a", "b"),
        "summary": blank_gs.summarize_working_tree(["SOUL.md"]),
        "show": blank_gs.show_commit_diff("a"),
        "revert": blank_gs.revert("a"),
        # Nothing was written: every method above is a no-op.
        "entries": sorted(p.name for p in blank.iterdir()),
    }
    out["init_after_noop"] = _call(blank_gs.init)
    out["init_after_noop_entries"] = sorted(p.name for p in blank.iterdir())

    # -- 7. unborn HEAD in a hand-made repository ---------------------------
    (unborn_ws / ".git" / "objects" / "info").mkdir(parents=True)
    (unborn_ws / ".git" / "objects" / "pack").mkdir(parents=True)
    (unborn_ws / ".git" / "refs" / "heads").mkdir(parents=True)
    (unborn_ws / ".git" / "HEAD").write_text("ref: refs/heads/master\n", encoding="utf-8")
    unborn = GitStore(unborn_ws, GITSTORE_TRACKED)
    (unborn_ws / "SOUL.md").write_text("x\n", encoding="utf-8")
    out["unborn"] = {
        "is_initialized": unborn.is_initialized(),
        "log": unborn.log(),
        "diff": unborn.diff_commits("deadbeef", "deadbeef"),
        "summary": unborn.summarize_working_tree(["SOUL.md"]),
        "commit": _sha_shape(unborn.auto_commit("dream: unborn")),
        "summary_after": unborn.summarize_working_tree(["SOUL.md"]),
        "subjects": [c.subject() for c in unborn.log()],
        "entries": _git_entries(unborn_ws / ".git"),
    }

    # -- 8. ignore sources ---------------------------------------------------
    out["ignores"] = _dump_gitstore_ignores(GitStore)

    # -- 9. failure paths ----------------------------------------------------
    out["failures"] = _dump_gitstore_failures(GitStore, ws, summary_paths)
    return out


def _dump_gitstore_failures(GitStore, ws, summary_paths) -> dict:
    """Every degradation path the reference reports as a GitStoreError."""
    import shutil

    out = {}

    # A workspace that is a regular file.
    as_file = GITSTORE_FILE
    as_file.write_text("not a directory\n", encoding="utf-8")
    out["init_on_file"] = _call(GitStore(as_file, ["SOUL.md"]).init)

    # A workspace inside another repository: init() declines, writes nothing.
    nested_parent = GITSTORE_TMP / "nested"
    if nested_parent.exists():
        shutil.rmtree(nested_parent)
    (nested_parent / ".git").mkdir(parents=True)
    nested = nested_parent / "inner"
    nested.mkdir()
    nested_gs = GitStore(nested, GITSTORE_TRACKED)
    out["nested_init"] = _call(nested_gs.init)
    out["nested_entries"] = sorted(p.name for p in nested.iterdir())

    # A tracked path that is a directory.
    dir_ws = GITSTORE_TMP / "dir"
    if dir_ws.exists():
        shutil.rmtree(dir_ws)
    dir_ws.mkdir(parents=True)
    dir_gs = GitStore(dir_ws, GITSTORE_TRACKED)
    dir_gs.init()
    (dir_ws / "SOUL.md").unlink()
    (dir_ws / "SOUL.md").mkdir()
    out["summary_directory"] = _call(dir_gs.summarize_working_tree, ["SOUL.md"])

    # A corrupt index: auto_commit fails, log and summary still work.
    corrupt = GITSTORE_TMP / "corrupt"
    if corrupt.exists():
        shutil.rmtree(corrupt)
    corrupt.mkdir(parents=True)
    corrupt_gs = GitStore(corrupt, GITSTORE_TRACKED)
    corrupt_gs.init()
    (corrupt / "SOUL.md").write_text("changed\n", encoding="utf-8")
    index = corrupt / ".git" / "index"
    good_index = index.read_bytes()
    index.write_bytes(b"GARBAGE" + good_index[7:])
    failure = _call(corrupt_gs.auto_commit, "dream: corrupt")
    if not failure["ok"]:
        # The reference embeds dulwich's exception text, which this port cannot
        # reproduce verbatim; only the wrapper's own prefix is part of the
        # contract.
        failure["prefix"] = failure["message"].startswith("Git auto-commit failed: ")
    out["auto_commit_corrupt_index"] = failure
    out["summary_corrupt_index"] = corrupt_gs.summarize_working_tree(["SOUL.md"])
    index.write_bytes(good_index)

    # A garbage HEAD: log fails, auto_commit and summary report a failure.
    head = corrupt / ".git" / "HEAD"
    good_head = head.read_bytes()
    head.write_bytes(b"garbage-not-a-ref\n")
    out["log_garbage_head"] = _call(corrupt_gs.log)
    out["auto_commit_garbage_head"] = _call(corrupt_gs.auto_commit, "dream: bad head")
    out["show_garbage_head"] = _call(corrupt_gs.show_commit_diff, "aaaa")
    head.write_bytes(good_head)

    # A missing blob object: the summary fails, log still works.
    missing = GITSTORE_TMP / "missing"
    if missing.exists():
        shutil.rmtree(missing)
    missing.mkdir(parents=True)
    missing_gs = GitStore(missing, GITSTORE_TRACKED)
    missing_gs.init()
    (missing / "SOUL.md").write_text("hello\n", encoding="utf-8")
    missing_gs.auto_commit("dream: present")
    (missing / "SOUL.md").write_text("hello again\n", encoding="utf-8")
    import subprocess

    # PATH is empty for the whole scenario (to prove the reference needs no git
    # binary), so the oracle below is located explicitly.
    git_bin = shutil.which("git", path=os.defpath)
    blob = subprocess.run(
        [git_bin, "-C", str(missing), "rev-parse", "HEAD:SOUL.md"],
        capture_output=True, text=True, check=True,
    ).stdout.strip()
    object_path = missing / ".git" / "objects" / blob[:2] / blob[2:]
    object_path.unlink()
    out["summary_missing_object"] = _call(missing_gs.summarize_working_tree, ["SOUL.md"])
    out["log_missing_object"] = _call(lambda: [c.subject() for c in missing_gs.log()])

    return out


def _dump_gitstore_ignores(GitStore) -> dict:
    """Which ignore sources decide whether a tracked file is staged.

    porcelain.add skips ignored paths, so an ignore rule that matches a tracked
    file silently stops it from ever being committed. dulwich reads more sources
    than .gitignore — the user's global ignore file (core.excludesFile, else
    ~/.config/git/ignore) and .git/info/exclude — and its precedence is the
    reverse of git's: the last match wins, and the filters are consulted from the
    deepest .gitignore outwards to the global ones.
    """
    import shutil

    out = {}
    cases = [
        {"name": "control", "global": "*.tmp\n", "target": "SOUL.md"},
        {"name": "global_md", "global": "*.md\n", "target": "SOUL.md"},
        {"name": "info_exclude", "global": "*.tmp\n",
         "extra": ("info/exclude", "SOUL.md\n"), "target": "SOUL.md"},
        {"name": "nested_ignored", "global": "*.tmp\n",
         "extra": ("memory/.gitignore", "MEMORY.md\n"), "target": "memory/MEMORY.md"},
        {"name": "nested_negated", "global": "*.tmp\n",
         "extra": ("memory/.gitignore", "!MEMORY.md\n"), "target": "memory/MEMORY.md"},
        {"name": "nested_new_file", "global": "*.tmp\n",
         "extra": ("memory/.gitignore", "*.md\n"), "target": "SOUL.md"},
        # core.ignorecase reaches the .gitignore filters only, and dulwich parses
        # it with get_boolean, which accepts nothing but true/false.
        {"name": "ignorecase_true_gitignore", "global": "*.tmp\n", "config": "ignorecase = true",
         "extra": (".gitignore", "soul.md\n"), "target": "SOUL.md"},
        {"name": "ignorecase_false_gitignore", "global": "*.tmp\n", "config": "ignorecase = false",
         "extra": (".gitignore", "soul.md\n"), "target": "SOUL.md"},
        {"name": "ignorecase_true_exclude", "global": "*.tmp\n", "config": "ignorecase = true",
         "extra": ("info/exclude", "soul.md\n"), "target": "SOUL.md"},
        {"name": "ignorecase_bad", "global": "*.tmp\n", "config": "ignorecase = 1",
         "target": "SOUL.md"},
    ]
    tmp = GITSTORE_TMP
    for case in cases:
        name = case["name"]
        target = case["target"]
        ws = tmp / f"gitstore-ref-ign-{name}"
        if ws.exists():
            shutil.rmtree(ws)
        ws.mkdir(parents=True)
        user_ignore = tmp / f"gitstore-ref-ign-{name}.ignore"
        user_ignore.write_text(case["global"], encoding="utf-8")
        gs = GitStore(ws, GITSTORE_TRACKED)
        gs.init()
        # Point the repository at this scenario's user ignore file.
        with (ws / ".git" / "config").open("a", encoding="utf-8") as f:
            f.write(f"[core]\n\texcludesFile = {user_ignore.resolve()}\n")
            if "config" in case:
                f.write(f"\t{case['config']}\n")
        if "extra" in case:
            rel, content = case["extra"]
            path = ws / ".git" / rel if rel.startswith("info/") else ws / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            # Appended, not written: init() has already created .gitignore.
            with path.open("a", encoding="utf-8") as f:
                f.write(content)
        (ws / target).write_text("changed\n", encoding="utf-8")
        try:
            sha = gs.auto_commit("dream: probe")
        except Exception as exc:  # noqa: BLE001 - the error is the observation
            out[name] = {"error": _norm(exc), "error_type": type(exc).__name__}
            continue
        out[name] = {
            "committed": sha is not None,
            "summary_after": gs.summarize_working_tree([target]),
            "subjects": [c.subject() for c in gs.log()],
        }
    return out


def _sha_shape(value):
    """Describe a returned commit id without pinning its value."""
    if value is None:
        return None
    return {"len": len(value), "hex": all(ch in "0123456789abcdef" for ch in value)}


def _timestamp_shape(value: str) -> bool:
    """True when the timestamp is "%Y-%m-%d %H:%M" for a plausible date."""
    import re

    return bool(re.fullmatch(r"\d{4}-\d{2}-\d{2} \d{2}:\d{2}", value))


def main() -> int:
    doc = {
        "upstream_commit": "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9",
        "agent_defaults": dump_agent_defaults(),
        "serialized_defaults": dump_serialization(),
        "alias_acceptance": dump_alias_acceptance(),
        "storage_keys": dump_storage_keys(),
        "paths": dump_paths(),
        "retry_classification": dump_retry_classification(),
        "governance": dump_governance(),
        "budget": dump_budget(),
        "tool_results": dump_tool_results(),
        "pyjson": dump_pyjson(),
        "length_recovery": dump_length_recovery(),
        "strip_think": dump_strip_think(),
        "gitstore": dump_gitstore(),
        "truncate_text": dump_truncate_text(),
        "pystr": dump_pystr(),
        "dream": dump_dream(),
    }
    json.dump(doc, sys.stdout, indent=2, sort_keys=True, ensure_ascii=False)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
