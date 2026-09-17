#!/usr/bin/env python3
"""Dump channel-abstraction and pairing-store ground truth from frozen nanobot.

This is one half of the channels differential harness. It executes the REAL
Python implementation (upstream/nanobot @ 1bb712d3) and emits a single JSON
document on stdout; compat/channels_differential_test.go drives the Go port
through the same inputs and compares field by field.

Nothing here is transcribed from documentation or from reading the source: every
value is produced by running the reference. Where a value is nondeterministic
(the pairing code is drawn from secrets.choice), both sides normalise it with
the same rule so the comparison stays exact.

Run with the project venv:
    .tools/venv/bin/python compat/python/dump_channels.py
"""
from __future__ import annotations

import asyncio
import json
import os
import re
import shutil
import sys
import tempfile
from pathlib import Path
from types import SimpleNamespace

ROOT = Path(__file__).resolve().parents[2]

# Silence loguru: it writes diagnostics to stderr and this harness must emit
# exactly one JSON document on stdout.
try:
    from loguru import logger as _loguru_logger

    _loguru_logger.remove()
except Exception:
    pass

UPSTREAM = ROOT / "upstream" / "nanobot"
sys.path.insert(0, str(UPSTREAM))

import nanobot.channels.base as base_mod  # noqa: E402
import nanobot.pairing as pairing_pkg  # noqa: E402
import nanobot.pairing.store as store  # noqa: E402
from nanobot.bus.queue import MessageBus  # noqa: E402

# Pairing codes are drawn from secrets.choice, so the two runtimes never agree
# on the value. Every place a code can surface is normalised with this rule.
CODE_RE = re.compile(r"[A-Z0-9]{4}-[A-Z0-9]{4}")

# A fixed clock, in Unix seconds. The Go side installs the same value, which
# makes created_at/expires_at and every expiry comparison deterministic.
FROZEN_TIME = 1755000000.1234567


def norm(value):
    """Replace any pairing code in a string with the literal CODE."""
    if isinstance(value, str):
        return CODE_RE.sub("CODE", value)
    if isinstance(value, list):
        return [norm(v) for v in value]
    if isinstance(value, dict):
        return {k: norm(v) for k, v in value.items()}
    return value


# Hand-written store fixtures. They are emitted in the dump so the Go side
# writes byte-identical inputs instead of re-deriving them: key order inside
# "pending" decides the order entries come back in, so a re-derived fixture
# could differ in a way that looks like an implementation bug.
#
# "typed" exercises str() coercion of non-string approved entries, set
# deduplication, sorted output, a non-list approved value, Python's repr of
# nested containers (including its quote-switching rule), unknown top-level
# keys, and every branch of the pending garbage collector.
TYPED_FIXTURE = json.dumps({
    "version": 2,
    "approved": {
        "typed": ["a", 5, None, True, 5.0, [1, 2], {"k": "v"}, "a"],
        "repr": [["it's", 'a"b', "ctrl\x01", "del\x7f"], {"it's": 'a"b'}],
        "empty": [],
        "scalar": 7,
    },
    "pending": {
        "GOODCODE": {"channel": "typed", "sender_id": 5,
                     "created_at": 1.5, "expires_at": 9000000000.0,
                     "note": "kept"},
        "BADTYPE": "garbage",
        "NOCHANNEL": {"sender_id": "x", "expires_at": 9000000000.0},
        "EMPTYCHANNEL": {"channel": "", "sender_id": "x", "expires_at": 9000000000.0},
        "NOSENDER": {"channel": "typed", "expires_at": 9000000000.0},
        "BOOLEXPIRY": {"channel": "typed", "sender_id": "x", "expires_at": True},
        "STREXPIRY": {"channel": "typed", "sender_id": "x", "expires_at": "later"},
        "EXPIRED": {"channel": "typed", "sender_id": "x", "expires_at": 1.0},
        "NULLEXPIRY": {"channel": "typed", "sender_id": "x", "expires_at": None},
    },
}, indent=2, ensure_ascii=False)

# "typed2" exercises a save with NO garbage collection: revoke_channel writes
# the file directly, so malformed pending entries must survive it untouched.
TYPED2_FIXTURE = json.dumps({
    "approved": {"typed": ["a", "b"]},
    "pending": {
        "BADTYPE": "garbage",
        "NOCHANNEL": {"sender_id": "x"},
        "GOODCODE": {"channel": "typed", "sender_id": 5,
                     "created_at": 1.5, "expires_at": 9000000000.0},
    },
}, indent=2, ensure_ascii=False)


class FrozenTime:
    """Stand-in for the time module, exposing only what store.py uses."""

    def __init__(self, now: float):
        self.now = now

    def time(self) -> float:
        return self.now


# ---------------------------------------------------------------------------
# A minimal channel double: the canonical _DummyChannel of
# tests/channels/test_base_channel.py:10-25.
# ---------------------------------------------------------------------------


class DummyChannel(base_mod.BaseChannel):
    name = "dummy"

    def __init__(self, config, bus):
        super().__init__(config, bus)
        self._sent = []
        self._send_error = None

    async def start(self) -> None:
        return None

    async def stop(self) -> None:
        return None

    async def send(self, msg):
        if self._send_error is not None:
            raise self._send_error
        self._sent.append(msg)


class StreamingChannel(DummyChannel):
    """Overrides send_delta, which is what makes supports_streaming true."""

    async def send_delta(
        self, chat_id, delta, metadata=None, *, stream_id=None,
        stream_end=False, resuming=False, merge_next=False,
    ) -> None:
        return None


def outbound_to_json(msg) -> dict:
    return {
        "channel": msg.channel,
        "chat_id": msg.chat_id,
        "content": msg.content,
        "reply_to": msg.reply_to,
        "media": msg.media,
        "metadata": msg.metadata,
        "buttons": msg.buttons,
        "event": None if msg.event is None else type(msg.event).__name__,
    }


def inbound_to_json(msg) -> dict:
    return {
        "channel": msg.channel,
        "sender_id": msg.sender_id,
        "chat_id": msg.chat_id,
        "content": msg.content,
        "media": msg.media,
        "metadata": msg.metadata,
        "session_key_override": msg.session_key_override,
        "require_existing_session": msg.require_existing_session,
        "input_role": msg.input_role,
        "session_key": msg.session_key,
        "is_user_input": msg.is_user_input,
    }


# ---------------------------------------------------------------------------
# is_allowed
# ---------------------------------------------------------------------------

ALLOW_CASES = [
    ("empty-list", {"allow_from": []}, "alice"),
    ("empty-alias-only", {"allowFrom": []}, "alice"),
    ("star", {"allowFrom": ["*"]}, "anyone"),
    ("star-in-list", {"allow_from": ["alice", "*"]}, "bob"),
    ("exact", {"allow_from": ["alice"]}, "alice"),
    ("exact-miss", {"allow_from": ["alice"]}, "bob"),
    ("case-sensitive", {"allow_from": ["Alice"]}, "alice"),
    ("whitespace-entry", {"allow_from": [" alice "]}, "alice"),
    ("whitespace-sender", {"allow_from": ["alice"]}, " alice "),
    ("whitespace-entry-star", {"allow_from": [" * "]}, "alice"),
    ("numeric-entry", {"allow_from": [123]}, "123"),
    ("numeric-string-entry", {"allow_from": ["123"]}, "123"),
    ("float-entry", {"allow_from": [1.0]}, "1.0"),
    ("bool-entry", {"allow_from": [True]}, "True"),
    ("null-entry", {"allow_from": [None]}, "None"),
    ("unicode", {"allow_from": ["\u00fcser\U0001f389"]}, "\u00fcser\U0001f389"),
    ("duplicate-entries", {"allow_from": ["a", "a", "a"]}, "a"),
    ("none-snake", {"allow_from": None}, "alice"),
    ("none-alias", {"allowFrom": None}, "alice"),
    ("falsy-snake-falls-through", {"allow_from": [], "allowFrom": ["alice"]}, "alice"),
    ("truthy-snake-wins", {"allow_from": ["alice"], "allowFrom": []}, "alice"),
    ("truthy-snake-wins-2", {"allow_from": ["alice"], "allowFrom": ["bob"]}, "bob"),
    ("false-snake-falls-through", {"allow_from": False, "allowFrom": ["alice"]}, "alice"),
    ("string-wildcard", {"allow_from": "*"}, "anyone"),
    ("string-substring-hit", {"allow_from": "alice,bob"}, "alice"),
    ("string-substring-miss", {"allow_from": "alice,bob"}, "carol"),
    ("string-partial", {"allow_from": "alice,bob"}, "lic"),
    ("dict-keys", {"allow_from": {"*": 1}}, "anyone"),
    ("dict-keys-named", {"allow_from": {"alice": 1}}, "alice"),
    ("dict-keys-miss", {"allow_from": {"alice": 1}}, "bob"),
    ("empty-dict", {"allow_from": {}}, "alice"),
    ("empty-string", {"allow_from": ""}, "alice"),
    ("pipeline-injection", {"allow_from": ["allow@email.com"]}, "attacker|allow@email.com"),
    ("comma-sender", {"allow_from": ["alice,bob"]}, "alice"),
    ("newline-entry", {"allow_from": ["ali\nce"]}, "alice"),
    ("control-char-entry", {"allow_from": ["ali\x1cce"]}, "alice"),
    ("int-value-raises", {"allow_from": 123}, "alice"),
    ("bool-value-raises", {"allow_from": True}, "alice"),
]

ALLOW_OBJECT_CASES = [
    ("obj-exact", ["alice"], None, "alice"),
    ("obj-miss", ["alice"], None, "bob"),
    ("obj-none", None, None, "alice"),
    ("obj-empty", [], None, "alice"),
    ("obj-star", ["*"], None, "anyone"),
    ("obj-string", "alice,bob", None, "alice"),
    ("obj-whitespace", [" alice "], None, "alice"),
    ("obj-numeric", [123], None, "123"),
]


def dump_is_allowed() -> list[dict]:
    out = []
    for label, config, sender in ALLOW_CASES:
        channel = DummyChannel(config, MessageBus())
        try:
            result = channel.is_allowed(sender)
            error = None
        except Exception as exc:  # noqa: BLE001 - the failure mode is the datum
            result = None
            error = type(exc).__name__
        out.append({
            "label": label,
            "config": config,
            "sender": sender,
            "result": result,
            "error": error,
        })
    return out


def dump_is_allowed_object() -> list[dict]:
    out = []
    for label, allow_from, streaming, sender in ALLOW_OBJECT_CASES:
        channel = DummyChannel(SimpleNamespace(allow_from=allow_from), MessageBus())
        try:
            result = channel.is_allowed(sender)
            error = None
        except Exception as exc:  # noqa: BLE001
            result = None
            error = type(exc).__name__
        out.append({
            "label": label,
            "allow_from": allow_from,
            "streaming": streaming,
            "sender": sender,
            "result": result,
            "error": error,
        })
    return out


def dump_pairing_fallback() -> list[dict]:
    """is_allowed's pairing-store branch, with is_approved stubbed."""
    original = base_mod.is_approved
    out = []
    try:
        base_mod.is_approved = lambda _ch, sid: sid == "paired"
        for label, config, sender in [
            ("pairing-hit", {"allowFrom": []}, "paired"),
            ("pairing-miss", {"allowFrom": []}, "unknown"),
            ("allowlist-wins-over-pairing", {"allowFrom": ["alice"]}, "paired"),
        ]:
            channel = DummyChannel(config, MessageBus())
            out.append({"label": label, "sender": sender,
                        "result": channel.is_allowed(sender)})
    finally:
        base_mod.is_approved = original
    return out


# ---------------------------------------------------------------------------
# supports_streaming
# ---------------------------------------------------------------------------

STREAMING_CASES = [
    ("dict-false-plain", {"streaming": False}, DummyChannel),
    ("dict-true-plain", {"streaming": True}, DummyChannel),
    ("dict-true-streaming", {"streaming": True}, StreamingChannel),
    ("dict-absent-streaming", {}, StreamingChannel),
    ("dict-truthy-string", {"streaming": "yes"}, StreamingChannel),
    ("dict-falsy-string", {"streaming": ""}, StreamingChannel),
    ("dict-zero", {"streaming": 0}, StreamingChannel),
    ("dict-empty-list", {"streaming": []}, StreamingChannel),
    ("dict-nonempty-list", {"streaming": [0]}, StreamingChannel),
    ("dict-camel-alias-ignored", {"streamingEnabled": True}, StreamingChannel),
]


def dump_supports_streaming() -> list[dict]:
    """supports_streaming for both channel classes.

    The "class" field records which channel the reference instantiated, because
    the result depends on BOTH the config and whether the class overrides
    send_delta — "dict-true-plain" is false while "dict-true-streaming" is true
    for the identical config.
    """
    out = []
    for label, config, cls in STREAMING_CASES:
        channel = cls(config, MessageBus())
        out.append({
            "label": label,
            "config": config,
            "class": "streaming" if cls is StreamingChannel else "plain",
            "result": channel.supports_streaming,
        })
    for label, streaming in [("obj-true", True), ("obj-false", False), ("obj-none", None)]:
        channel = StreamingChannel(SimpleNamespace(streaming=streaming), MessageBus())
        out.append({"label": label, "config": {"streaming": streaming}, "class": "streaming",
                    "result": channel.supports_streaming})
    return out


# ---------------------------------------------------------------------------
# _handle_message
# ---------------------------------------------------------------------------

HANDLE_CASES = [
    ("allowed", {"allow_from": ["alice"]}, DummyChannel,
     {"sender_id": "alice", "chat_id": "c1", "content": "hello"}),
    ("allowed-full", {"allow_from": ["alice"]}, DummyChannel,
     {"sender_id": "alice", "chat_id": "c1", "content": "hi", "media": ["u1", "u2"],
      "metadata": {"k": "v"}, "session_key": "sess-1", "require_existing_session": True}),
    ("allowed-empty-media", {"allow_from": ["alice"]}, DummyChannel,
     {"sender_id": "alice", "chat_id": "c", "content": "x", "media": []}),
    ("allowed-none-metadata", {"allow_from": ["alice"]}, DummyChannel,
     {"sender_id": "alice", "chat_id": "c", "content": "x", "metadata": None}),
    ("allowed-empty-metadata", {"allow_from": ["alice"]}, DummyChannel,
     {"sender_id": "alice", "chat_id": "c", "content": "x", "metadata": {}}),
    ("allowed-star", {"allowFrom": ["*"]}, DummyChannel,
     {"sender_id": "anyone", "chat_id": "c", "content": "x"}),
    ("denied-dm", {"allow_from": ["alice"]}, DummyChannel,
     {"sender_id": "stranger", "chat_id": "c1", "content": "hello", "is_dm": True}),
    ("denied-group", {"allow_from": ["alice"]}, DummyChannel,
     {"sender_id": "stranger", "chat_id": "c1", "content": "hello"}),
    ("authorization-id-ok", {"allow_from": ["group@g.us"]}, DummyChannel,
     {"sender_id": "member-lid", "authorization_id": "group@g.us",
      "chat_id": "group@g.us", "content": "hello"}),
    ("authorization-id-denied", {"allow_from": ["member-lid"]}, DummyChannel,
     {"sender_id": "member-lid", "authorization_id": "other@g.us",
      "chat_id": "other@g.us", "content": "hello"}),
    ("authorization-id-denied-dm", {"allow_from": ["member-lid"]}, DummyChannel,
     {"sender_id": "member-lid", "authorization_id": "other@g.us",
      "chat_id": "other@g.us", "content": "hello", "is_dm": True}),
    ("authorization-id-empty", {"allow_from": ["alice"]}, DummyChannel,
     {"sender_id": "alice", "authorization_id": "", "chat_id": "c", "content": "x"}),
    ("streaming-on", {"allow_from": ["alice"], "streaming": True}, StreamingChannel,
     {"sender_id": "alice", "chat_id": "c", "content": "x"}),
    ("streaming-on-with-metadata", {"allow_from": ["alice"], "streaming": True}, StreamingChannel,
     {"sender_id": "alice", "chat_id": "c", "content": "x", "metadata": {"k": "v"}}),
    ("streaming-on-nonstream-class", {"allow_from": ["alice"], "streaming": True}, DummyChannel,
     {"sender_id": "alice", "chat_id": "c", "content": "x"}),
    ("unicode-content", {"allow_from": ["\u00fcser"]}, DummyChannel,
     {"sender_id": "\u00fcser", "chat_id": "c\U0001f389", "content": "ol\u00e1 \U0001f389"}),
]


async def dump_handle_message(work: Path) -> list[dict]:
    out = []
    for label, config, cls, kwargs in HANDLE_CASES:
        bus = MessageBus()
        channel = cls(config, bus)
        error = None
        try:
            await channel._handle_message(**kwargs)
        except Exception as exc:  # noqa: BLE001
            error = f"{type(exc).__name__}"
        inbound_size = bus.inbound_size
        published = None
        if inbound_size:
            published = norm(inbound_to_json(await bus.consume_inbound()))
        out.append({
            "label": label,
            "config": config,
            "kwargs": kwargs,
            "error": error,
            "published": published,
            "inbound_size": inbound_size,
            "sent": [norm(outbound_to_json(m)) for m in channel._sent],
        })

    # Metadata aliasing: `meta = metadata or {}` shares a non-empty dict with the
    # caller but replaces an empty one, and streaming always copies.
    for label, config, cls in [
        ("aliasing-nonstream-nonempty", {"allow_from": ["alice"]}, DummyChannel),
        ("aliasing-nonstream-empty", {"allow_from": ["alice"]}, DummyChannel),
        ("aliasing-streaming-nonempty", {"allow_from": ["alice"], "streaming": True}, StreamingChannel),
    ]:
        bus = MessageBus()
        channel = cls(config, bus)
        metadata = {} if label.endswith("-empty") else {"k": "v"}
        await channel._handle_message(sender_id="alice", chat_id="c", content="x",
                                      metadata=metadata)
        metadata["late"] = "added-after"
        published = inbound_to_json(await bus.consume_inbound())
        out.append({
            "label": label,
            "config": config,
            "kwargs": {},
            "error": None,
            "published": published,
            "inbound_size": 0,
            "sent": [],
        })

    # Media aliasing: a non-empty list is shared with the caller. The mutation
    # is an element assignment rather than an append, because that is the only
    # form Go can reproduce: a slice header is copied into the message, so an
    # append (which may reallocate and always changes only the caller's length)
    # is invisible there, while element assignment writes through the shared
    # backing array.
    bus = MessageBus()
    channel = DummyChannel({"allow_from": ["alice"]}, bus)
    media = ["u1"]
    await channel._handle_message(sender_id="alice", chat_id="c", content="x", media=media)
    media[0] = "u9"
    published = inbound_to_json(await bus.consume_inbound())
    out.append({
        "label": "aliasing-media",
        "config": {"allow_from": ["alice"]},
        "kwargs": {},
        "error": None,
        "published": published,
        "inbound_size": 0,
        "sent": [],
    })

    # Unavailable pairing store: a DM must be dropped without replying and
    # without erasing approvals.
    store_path = work / "unavailable.json"
    store_path.mkdir()
    bus = MessageBus()
    channel = DummyChannel({"allowFrom": []}, bus)
    error = None
    try:
        await channel._handle_message(sender_id="stranger", chat_id="c",
                                      content="hello", is_dm=True)
    except Exception as exc:  # noqa: BLE001
        error = f"{type(exc).__name__}"
    out.append({
        "label": "store-unavailable-dm",
        "config": {"allowFrom": []},
        "kwargs": {"sender_id": "stranger", "chat_id": "c", "content": "hello", "is_dm": True},
        "error": error,
        "published": None,
        "inbound_size": bus.inbound_size,
        "sent": [],
    })
    store_path.rmdir()
    return out


# ---------------------------------------------------------------------------
# Pairing store
# ---------------------------------------------------------------------------


def dump_store(work: Path) -> dict:
    path = work / "pairing.json"
    original_path = store._store_path
    original_time = store.time
    store._store_path = lambda: path
    clock = FrozenTime(FROZEN_TIME)
    store.time = clock

    steps: list[dict] = []
    results: dict = {}

    def snapshot(label: str) -> None:
        steps.append({
            "label": label,
            "exists": path.exists(),
            "text": norm(path.read_text(encoding="utf-8")) if path.exists() else None,
        })

    try:
        snapshot("initial")
        code_a = store.generate_code("telegram", "alice")
        results["code_a_shape_ok"] = bool(re.fullmatch(r"[A-Z0-9]{4}-[A-Z0-9]{4}", code_a))
        results["code_a_length"] = len(code_a)
        snapshot("generate-alice")

        code_a_again = store.generate_code("telegram", "alice")
        results["generate_is_idempotent"] = code_a == code_a_again

        code_b = store.generate_code("discord", "bob")
        results["codes_differ"] = code_a != code_b
        snapshot("generate-bob")

        results["is_approved_before"] = store.is_approved("telegram", "alice")
        results["is_approved_unknown_channel"] = store.is_approved("slack", "alice")

        clock.now = FROZEN_TIME + 0.5
        results["list_pending"] = norm(store.list_pending())
        results["format_expiry_future"] = store.format_expiry(clock.now + 120)
        results["format_expiry_now"] = store.format_expiry(clock.now)
        results["format_expiry_past"] = store.format_expiry(clock.now - 1)
        results["format_expiry_fractional"] = store.format_expiry(clock.now + 0.5)

        results["approve_a"] = store.approve_code(code_a)
        results["approve_a_again"] = store.approve_code(code_a)
        results["approve_unknown"] = store.approve_code("ZZZZZZZZ")
        snapshot("approve-a")

        results["is_approved_after"] = store.is_approved("telegram", "alice")
        results["get_approved_telegram"] = store.get_approved("telegram")
        results["get_approved_discord"] = store.get_approved("discord")
        results["get_approved_unknown"] = store.get_approved("nope")

        # A second approval on the same channel exercises sorted() output.
        code_c = store.generate_code("telegram", "carol")
        store.approve_code(code_c)
        results["get_approved_telegram_sorted"] = store.get_approved("telegram")
        snapshot("approve-carol")

        results["cmd_empty"] = norm(store.handle_pairing_command("telegram", ""))
        results["cmd_list"] = norm(store.handle_pairing_command("telegram", "list"))
        results["cmd_list_whitespace"] = norm(store.handle_pairing_command("telegram", "  list  "))
        results["cmd_list_control_ws"] = norm(store.handle_pairing_command("telegram", "\x1clist\x1c"))
        results["cmd_approve_no_arg"] = store.handle_pairing_command("telegram", "approve")
        results["cmd_approve_bad"] = store.handle_pairing_command("telegram", "approve NOPE-NOPE")
        results["cmd_deny_no_arg"] = store.handle_pairing_command("telegram", "deny")
        results["cmd_deny_bad"] = store.handle_pairing_command("telegram", "deny NOPE-NOPE")
        results["cmd_revoke_one"] = store.handle_pairing_command("telegram", "revoke alice")
        results["cmd_revoke_one_missing"] = store.handle_pairing_command("telegram", "revoke nobody")
        results["cmd_revoke_two"] = store.handle_pairing_command("telegram", "revoke discord bob")
        results["cmd_revoke_usage"] = store.handle_pairing_command("telegram", "revoke")
        results["cmd_unknown"] = store.handle_pairing_command("telegram", "frobnicate")
        snapshot("after-commands")

        results["deny_unknown"] = store.deny_code("NOPECODE")
        results["deny_known"] = store.deny_code(code_b)
        results["deny_known_again"] = store.deny_code(code_b)
        snapshot("after-deny")

        results["revoke_present"] = store.revoke("telegram", "carol")
        results["revoke_absent"] = store.revoke("telegram", "nobody")
        snapshot("after-revoke")

        results["revoke_channel_known"] = store.revoke_channel("telegram")
        results["revoke_channel_absent"] = store.revoke_channel("telegram")
        results["revoke_channel_unknown"] = store.revoke_channel("nope")
        snapshot("after-revoke-channel")

        # clear_channel: pending code present, no approved senders.
        code_d = store.generate_code("discord", "dave")
        results["clear_channel"] = store.clear_channel("discord")
        results["clear_channel_empty"] = store.clear_channel("discord")
        results["deny_d"] = store.deny_code(code_d)
        snapshot("after-clear")

        # Expiry: _gc_pending removes the entry from the view but does not save.
        code_e = store.generate_code("x", "y", ttl=10)
        clock.now = FROZEN_TIME + 100.0
        results["list_pending_expired"] = norm(store.list_pending())
        results["approve_expired"] = store.approve_code(code_e)
        snapshot("after-expiry-no-save")

        # Corrupted store: reset in memory, file untouched.
        path.write_text("not json", encoding="utf-8")
        results["is_approved_corrupted"] = store.is_approved("telegram", "alice")
        results["list_pending_corrupted"] = store.list_pending()
        snapshot("corrupted-untouched")

        # Non-dict top level resets too.
        path.write_text("[1, 2, 3]", encoding="utf-8")
        results["get_approved_nondict"] = store.get_approved("telegram")
        snapshot("nondict-untouched")

        # Hand-written file with typed and malformed entries: str() coercion,
        # set dedup, sorted output, gc of malformed pending entries, and the
        # preservation of pending entries by revoke (which does not gc).
        path.write_text(TYPED_FIXTURE, encoding="utf-8")
        results["list_pending_typed"] = norm(store.list_pending())
        # list_pending garbage-collects in memory only: the file must still hold
        # every malformed entry.
        snapshot("typed-after-list-pending")
        results["get_approved_typed"] = store.get_approved("typed")
        results["get_approved_repr"] = store.get_approved("repr")
        results["get_approved_empty"] = store.get_approved("empty")
        results["get_approved_scalar"] = store.get_approved("scalar")
        results["is_approved_typed_numeric"] = store.is_approved("typed", "5")
        results["approve_typed_good"] = norm(store.approve_code("GOODCODE"))
        snapshot("typed-after-approve")

        # revoke does NOT gc, so malformed pending entries must survive a save.
        results["revoke_typed"] = store.revoke("typed", "5")
        snapshot("typed-after-revoke")

        # generate_code does gc, so the malformed entries disappear here.
        code_f = store.generate_code("typed", "zoe")
        results["generate_after_gc"] = bool(re.fullmatch(r"[A-Z0-9]{4}-[A-Z0-9]{4}", code_f))
        snapshot("typed-after-generate")

        # A save with no gc at all: revoke_channel persists the approved map and
        # must carry the malformed pending entries through untouched.
        path.write_text(TYPED2_FIXTURE, encoding="utf-8")
        results["revoke_channel_typed"] = store.revoke_channel("typed")
        snapshot("typed2-after-revoke-channel")
        results["list_pending_typed2"] = norm(store.list_pending())
    finally:
        store._store_path = original_path
        store.time = original_time

    return {
        "frozen_time": FROZEN_TIME,
        "fixtures": {"typed": TYPED_FIXTURE, "typed2": TYPED2_FIXTURE},
        "steps": steps,
        "results": results,
    }


def dump_store_constants() -> dict:
    return {
        "alphabet": store._ALPHABET,
        "code_length": store._CODE_LENGTH,
        "ttl_default_s": store._TTL_DEFAULT_S,
        "default_path": str(store._store_path()),
        "meta_code_key": pairing_pkg.PAIRING_CODE_META_KEY,
        "meta_command_key": pairing_pkg.PAIRING_COMMAND_META_KEY,
    }


def dump_pairing_reply() -> list[dict]:
    return [
        {"code": code, "text": store.format_pairing_reply(code)}
        for code in ["ABCD-EFGH", "ZZZZZZZZ", "", "code`with`ticks", "\u00e9\u00e8"]
    ]


def dump_float_repr() -> list[dict]:
    values = [
        0.0, -0.0, 1.0, -1.0, 0.5, 1.5, 100.0, 1e15, 1e16, 1e17, 1e21,
        1e-4, 1e-5, 1e-6, 0.0001, 123456789.123456789, 1755000000.1234567,
        9000000000.0, 1755000600.1234567, 2.2250738585072014e-308,
        1.7976931348623157e308, 0.1, 1/3, 1234567890123456.0, 12345678901234567.0,
    ]
    return [{"value": v, "repr": repr(v)} for v in values]


def dump_py_str() -> list[dict]:
    values = [
        5, -7, 5.0, 1.5, 9000000000.0, True, False, None, "alice", "",
        [1, 2], {"a": 1}, ["x", {"y": [1]}], [], {},
        "it's", 'a"b', 'both\'"q', "tab\there", "nl\nhere",
        "ctrl\x01", "del\x7f", "\u00e9\u00e8", "\U0001f389",
    ]
    return [{"value": v, "str": str(v), "repr": repr(v)} for v in values]


async def main() -> dict:
    work = Path(tempfile.mkdtemp(prefix="channels-diff-", dir=ROOT / ".tools" / "tmp"))
    try:
        doc = {
            "upstream_commit": "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9",
            "store_constants": dump_store_constants(),
            "is_allowed": dump_is_allowed(),
            "is_allowed_object": dump_is_allowed_object(),
            "pairing_fallback": dump_pairing_fallback(),
            "supports_streaming": dump_supports_streaming(),
            "format_pairing_reply": dump_pairing_reply(),
            "float_repr": dump_float_repr(),
            "py_str": dump_py_str(),
            "handle_message": await dump_handle_message(work),
            "store": dump_store(work),
        }
    finally:
        shutil.rmtree(work, ignore_errors=True)
    return doc


if __name__ == "__main__":
    print(json.dumps(asyncio.run(main()), ensure_ascii=False, sort_keys=True, indent=None))
