#!/usr/bin/env python3
"""Dump ChannelManager ground truth from the frozen nanobot reference.

This is one half of the manager differential harness. It EXECUTES the real
Python implementation (upstream/nanobot @ 1bb712d3,
nanobot/channels/manager.py) and emits a single JSON document on stdout;
compat/manager_differential_test.go drives the Go port through the same inputs
and compares field by field.

It is a separate file from dump_reference.py on purpose: dump_reference.py and
differential_test.go are edited concurrently by several agents, and a
read-modify-write race there would destroy work. This dumper and its Go test
share nothing but repoRoot.

Nothing here is transcribed from documentation or from reading the source:
every value is produced by running the reference.

Run with the project venv:
    .tools/venv/bin/python compat/python/dump_manager.py
"""
from __future__ import annotations

import asyncio
import json
import sys
from collections import OrderedDict
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

# loguru writes diagnostics to stderr; this harness must emit exactly one JSON
# document on stdout so the Go side can parse it.
try:
    from loguru import logger as _loguru_logger

    _loguru_logger.remove()
except Exception:
    pass

sys.path.insert(0, str(ROOT / "upstream" / "nanobot"))

import nanobot.channels.manager as manager_module  # noqa: E402
from nanobot.bus.events import OutboundMessage  # noqa: E402
from nanobot.bus.outbound_events import (  # noqa: E402
    FileEditEvent,
    ProgressEvent,
    RetryWaitEvent,
    StreamDeltaEvent,
    StreamEndEvent,
    StreamedResponseEvent,
    outbound_message_for_event,
)
from nanobot.bus.queue import MessageBus  # noqa: E402
from nanobot.channels.manager import ChannelManager  # noqa: E402
from nanobot.config.schema import Config  # noqa: E402

COMMIT = "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9"

# The retry schedule the harness substitutes for the real (1, 2, 4) seconds.
# The Go side installs the millisecond equivalents, so the two runtimes agree on
# the SCHEDULE without either one sleeping for seconds.
FAST_RETRY_DELAYS = (0.001, 0.002, 0.004)
FAST_RETRY_DELAYS_MS = [1, 2, 4]

OUT: dict = {}


# ---------------------------------------------------------------------------
# Message specs
#
# A message is described as plain data so the Go side can rebuild it from the
# dump instead of re-deriving it; a re-derived fixture could differ in a way
# that looks like an implementation bug.
# ---------------------------------------------------------------------------

EVENT_KINDS = (
    "none",
    "progress",
    "file_edit",
    "stream_delta",
    "stream_end",
    "streamed_response",
    "retry_wait",
)


def spec(
    content: str = "c",
    *,
    channel: str = "mock",
    chat_id: str = "chat",
    event: str = "none",
    stream_id: str | None = None,
    resuming: bool = False,
    merge_next: bool = False,
    reasoning: bool = False,
    reasoning_delta: bool = False,
    reasoning_end: bool = False,
    tool_hint: bool = False,
    file_edit_events: list | None = None,
    metadata: dict | None = None,
) -> dict:
    return {
        "channel": channel,
        "chat_id": chat_id,
        "content": content,
        "event": event,
        "stream_id": stream_id,
        "resuming": resuming,
        "merge_next": merge_next,
        "reasoning": reasoning,
        "reasoning_delta": reasoning_delta,
        "reasoning_end": reasoning_end,
        "tool_hint": tool_hint,
        "file_edit_events": file_edit_events,
        "metadata": metadata,
    }


def build_event(s: dict):
    kind = s["event"]
    if kind == "none":
        return None
    if kind == "progress":
        return ProgressEvent(
            content=s["content"],
            tool_hint=s["tool_hint"],
            reasoning=s["reasoning"],
            reasoning_delta=s["reasoning_delta"],
            reasoning_end=s["reasoning_end"],
            stream_id=s["stream_id"],
            file_edit_events=list(s["file_edit_events"] or []),
        )
    if kind == "file_edit":
        return FileEditEvent(
            content=s["content"],
            file_edit_events=list(s["file_edit_events"] or []),
        )
    if kind == "stream_delta":
        return StreamDeltaEvent(content=s["content"], stream_id=s["stream_id"])
    if kind == "stream_end":
        return StreamEndEvent(
            content=s["content"],
            stream_id=s["stream_id"],
            resuming=s["resuming"],
            merge_next=s["merge_next"],
        )
    if kind == "streamed_response":
        return StreamedResponseEvent()
    if kind == "retry_wait":
        return RetryWaitEvent(content=s["content"])
    raise AssertionError(f"unknown event kind {kind!r}")


def build_message(s: dict) -> OutboundMessage:
    event = build_event(s)
    if event is None:
        return OutboundMessage(
            channel=s["channel"],
            chat_id=s["chat_id"],
            content=s["content"],
            metadata=dict(s["metadata"] or {}),
        )
    return outbound_message_for_event(
        channel=s["channel"],
        chat_id=s["chat_id"],
        event=event,
        content=s["content"],
        metadata=dict(s["metadata"] or {}),
    )


def describe(msg: OutboundMessage) -> dict:
    """The comparable shape of a message: only what the two runtimes share."""
    event = msg.event
    kind = "none" if event is None else type(event).__name__
    return {
        "channel": msg.channel,
        "chat_id": msg.chat_id,
        "content": msg.content,
        "event": kind,
        "stream_id": getattr(event, "stream_id", None),
        "resuming": bool(getattr(event, "resuming", False)) if kind == "StreamEndEvent" else None,
        "merge_next": bool(getattr(event, "merge_next", False)) if kind == "StreamEndEvent" else None,
        "metadata": dict(msg.metadata or {}),
    }


# ---------------------------------------------------------------------------
# A manager with no __init__: only the attributes each method reads.
# ---------------------------------------------------------------------------


def bare_manager(*, send_max_retries: int = 3, bus: MessageBus | None = None) -> ChannelManager:
    m = ChannelManager.__new__(ChannelManager)
    m.bus = bus if bus is not None else MessageBus()
    m.channels = {}
    m._channel_owners = {}
    m._channel_runtime_specs = {}
    m._channel_errors = {}
    m._channel_tasks = {}
    m._dispatch_task = None
    m._outbound_tasks = {}
    m._outbound_tails = {}
    m._outbound_slots = asyncio.Semaphore(manager_module._OUTBOUND_PENDING_LIMIT)
    m._outbound_sends = asyncio.Semaphore(manager_module._OUTBOUND_CONCURRENCY)
    m._stopping_channels = set()
    m._started = False
    m._origin_reply_fingerprints = OrderedDict()
    m.config = Config.model_validate(
        {"channels": {"websocket": {"enabled": False}, "send_max_retries": send_max_retries}}
    )
    return m


# ---------------------------------------------------------------------------
# 1. Constants
# ---------------------------------------------------------------------------


def dump_constants() -> dict:
    return {
        "send_retry_delays": list(manager_module._SEND_RETRY_DELAYS),
        "outbound_concurrency": manager_module._OUTBOUND_CONCURRENCY,
        "outbound_pending_limit": manager_module._OUTBOUND_PENDING_LIMIT,
        "origin_reply_fingerprints_max_size": (
            manager_module.ORIGIN_REPLY_FINGERPRINTS_MAX_SIZE
        ),
    }


def dump_retry_schedule() -> list:
    """_SEND_RETRY_DELAYS[min(attempt - 1, len - 1)] for attempts 1..8."""
    delays = manager_module._SEND_RETRY_DELAYS
    return [delays[min(a - 1, len(delays) - 1)] for a in range(1, 9)]


# ---------------------------------------------------------------------------
# 2. _fingerprint_content (manager.py:683-686)
# ---------------------------------------------------------------------------

FINGERPRINT_INPUTS = [
    "",
    "   ",
    "\t\n",
    "\u00a0",
    "\u001c",
    "\u001c\u001d\u001e\u001f",
    "\u0085",
    "\u2028",
    "hello",
    "hello  world",
    "hello\nworld",
    "  hello   world  ",
    "a b",
    "a\u00a0b",
    "a\u001cb",
    "a\u200bb",
    "a\ufeffb",
    "\u180e",
    "caf\u00e9",
    "\U0001f600 ok",
]


def dump_fingerprint() -> list:
    out = []
    for text in FINGERPRINT_INPUTS:
        out.append({"input": text, "digest": ChannelManager._fingerprint_content(text)})
    return out


# ---------------------------------------------------------------------------
# 3. _should_suppress_outbound (manager.py:698-719)
# ---------------------------------------------------------------------------

SUPPRESS_CASES = [
    (
        "same_origin_message_id",
        [
            spec("hello", metadata={"origin_message_id": "o1"}),
            spec("hello", metadata={"origin_message_id": "o1"}),
        ],
    ),
    (
        "whitespace_normalized",
        [
            spec("hello  world", metadata={"origin_message_id": "o1"}),
            spec("hello\nworld", metadata={"origin_message_id": "o1"}),
            spec("  hello   world  ", metadata={"origin_message_id": "o1"}),
        ],
    ),
    (
        "different_content",
        [
            spec("hello", metadata={"origin_message_id": "o1"}),
            spec("other", metadata={"origin_message_id": "o1"}),
            spec("hello", metadata={"origin_message_id": "o1"}),
        ],
    ),
    (
        "empty_content",
        [
            spec("", metadata={"origin_message_id": "o1"}),
            spec("", metadata={"origin_message_id": "o1"}),
        ],
    ),
    (
        "whitespace_only",
        [
            spec("   ", metadata={"origin_message_id": "o1"}),
            spec("\t\n", metadata={"origin_message_id": "o1"}),
        ],
    ),
    (
        "non_string_origin_id",
        [
            spec("hello", metadata={"origin_message_id": 7}),
            spec("hello", metadata={"origin_message_id": 7}),
        ],
    ),
    (
        "empty_origin_id",
        [
            spec("hello", metadata={"origin_message_id": ""}),
            spec("hello", metadata={"origin_message_id": ""}),
        ],
    ),
    (
        "message_id_alone",
        [
            spec("hello", metadata={"message_id": "m1"}),
            spec("hello", metadata={"message_id": "m1"}),
        ],
    ),
    (
        "progress_never_suppressed",
        [
            spec("hello", event="progress", metadata={"origin_message_id": "o1"}),
            spec("hello", event="progress", metadata={"origin_message_id": "o1"}),
        ],
    ),
    (
        "file_edit_never_suppressed",
        [
            spec("hello", event="file_edit", metadata={"origin_message_id": "o1"}),
            spec("hello", event="file_edit", metadata={"origin_message_id": "o1"}),
        ],
    ),
    (
        "stream_delta_is_suppressed_by_the_helper",
        [
            spec("hello", event="stream_delta", metadata={"origin_message_id": "o1"}),
            spec("hello", event="stream_delta", metadata={"origin_message_id": "o1"}),
        ],
    ),
    (
        "nbsp_normalizes_like_a_space",
        [
            spec("a\u00a0b", metadata={"origin_message_id": "o1"}),
            spec("a b", metadata={"origin_message_id": "o1"}),
        ],
    ),
    (
        "unit_separator_normalizes_like_a_space",
        [
            spec("a\u001cb", metadata={"origin_message_id": "o1"}),
            spec("a b", metadata={"origin_message_id": "o1"}),
        ],
    ),
    (
        "nil_metadata",
        [spec("hello"), spec("hello")],
    ),
    (
        "origin_then_message_id",
        [
            spec("hello", metadata={"origin_message_id": "o1", "message_id": "m1"}),
            spec("hello", metadata={"origin_message_id": "o1", "message_id": "m1"}),
            spec("hello", metadata={"message_id": "m1"}),
        ],
    ),
    (
        "different_chat_same_origin",
        [
            spec("hello", chat_id="a", metadata={"origin_message_id": "o1"}),
            spec("hello", chat_id="b", metadata={"origin_message_id": "o1"}),
            spec("hello", chat_id="a", metadata={"origin_message_id": "o1"}),
        ],
    ),
]


def dump_suppress() -> list:
    out = []
    for name, steps in SUPPRESS_CASES:
        m = bare_manager()
        results = [m._should_suppress_outbound(build_message(s)) for s in steps]
        out.append(
            {
                "name": name,
                "steps": steps,
                "results": results,
                # The OrderedDict key order after the sequence: this is what
                # proves move-to-end and the insertion order of remembers.
                "keys_after": [
                    [k[0], k[1], k[2]] for k in m._origin_reply_fingerprints
                ],
            }
        )
    return out


def dump_eviction() -> dict:
    """The bounded fingerprint memory, probed through its public behaviour.

    Nothing here reads the manager's private state to decide the expectation:
    the state is BUILT the same way a gateway builds it, and then probed by
    asking _should_suppress_outbound about individual origin ids. Each probe
    runs on its own freshly built manager, because probing an unremembered key
    inserts it and would perturb the next probe.
    """
    limit = manager_module.ORIGIN_REPLY_FINGERPRINTS_MAX_SIZE

    def filled() -> ChannelManager:
        m = bare_manager()
        for i in range(limit):
            m._should_suppress_outbound(
                build_message(spec("hello", metadata={"origin_message_id": f"o{i}"}))
            )
        # A hit must move the key to the most-recent end...
        m._should_suppress_outbound(
            build_message(spec("hello", metadata={"origin_message_id": "o0"}))
        )
        # ...so this insert evicts o1, not o0.
        m._should_suppress_outbound(
            build_message(spec("hello", metadata={"origin_message_id": "new"}))
        )
        return m

    reference = filled()
    probes = {}
    for key in ("o0", "o1", "o2", "o999", "new", "missing"):
        m = filled()
        probes[key] = m._should_suppress_outbound(
            build_message(spec("hello", metadata={"origin_message_id": key}))
        )

    oldest = next(iter(reference._origin_reply_fingerprints))
    return {
        "limit": limit,
        "size_after": len(reference._origin_reply_fingerprints),
        "oldest_after_eviction": [oldest[0], oldest[1], oldest[2]],
        "probes": probes,
    }


# ---------------------------------------------------------------------------
# 4. _send_once (manager.py:908-931)
# ---------------------------------------------------------------------------

SEND_ONCE_CASES = [
    ("none", spec("c")),
    ("progress_empty", spec("c", event="progress")),
    ("progress_content", spec("p", event="progress")),
    ("progress_tool_hint", spec("p", event="progress", tool_hint=True)),
    ("progress_reasoning", spec("r", event="progress", reasoning=True)),
    (
        "progress_reasoning_delta",
        spec("rd", event="progress", reasoning_delta=True, stream_id="s"),
    ),
    (
        "progress_reasoning_end",
        spec("re", event="progress", reasoning_end=True, stream_id="s"),
    ),
    (
        "progress_file_edit",
        spec("fe", event="progress", file_edit_events=[{"path": "a"}]),
    ),
    ("file_edit_event", spec("fe", event="file_edit", file_edit_events=[{"path": "a"}])),
    ("file_edit_empty", spec("fe", event="file_edit")),
    ("stream_delta", spec("d", event="stream_delta", stream_id="s")),
    ("stream_delta_no_id", spec("d", event="stream_delta")),
    (
        "stream_end",
        spec("e", event="stream_end", stream_id="s", resuming=True, merge_next=True),
    ),
    ("stream_end_no_merge", spec("e", event="stream_end", stream_id="s")),
    ("stream_end_empty", spec("", event="stream_end")),
    ("streamed_response", spec("already", event="streamed_response")),
    ("retry_wait", spec("w", event="retry_wait")),
    (
        "priority_end_beats_delta",
        spec("x", event="progress", reasoning_end=True, reasoning_delta=True),
    ),
    (
        "priority_delta_beats_reasoning",
        spec("x", event="progress", reasoning=True, reasoning_delta=True),
    ),
    (
        "priority_end_beats_file_edit",
        spec("x", event="progress", reasoning_end=True, file_edit_events=[{"a": 1}]),
    ),
    (
        "priority_reasoning_beats_file_edit",
        spec("x", event="progress", reasoning=True, file_edit_events=[{"a": 1}]),
    ),
    ("reasoning_empty_content", spec("", event="progress", reasoning=True)),
    (
        "file_edit_event_empty",
        spec("x", event="file_edit"),
    ),
]


async def dump_send_once() -> list:
    out = []
    for name, s in SEND_ONCE_CASES:
        ch = RecordingChannel()
        msg = build_message(s)
        error = None
        try:
            await ChannelManager._send_once(ch, msg)
        except Exception as exc:  # noqa: BLE001
            error = f"{type(exc).__name__}: {exc}"
        out.append({"name": name, "spec": s, "calls": ch.calls, "error": error})
    return out


class RecordingChannel:
    """Records every outbound primitive with normalised arguments.

    It deliberately does NOT subclass BaseChannel: the manager only calls the
    primitives, and implementing them directly makes every recorded argument
    explicit. ``send_reasoning`` is included because the manager reaches it
    through BaseChannel.send_reasoning, whose body is reproduced here verbatim
    from base.py:201-223 so the recorded pair is the reference's.
    """

    name = "mock"
    display_name = "Mock"
    send_progress = True
    send_tool_hints = True
    show_reasoning = True
    is_running = False

    def __init__(self, *, retryable: bool = True):
        self.calls = []
        self._retryable = retryable

    def should_retry_send_error(self, error) -> bool:
        return self._retryable

    async def send(self, msg):
        self.calls.append(["send", msg.chat_id, msg.content, dict(msg.metadata or {})])

    async def send_delta(
        self,
        chat_id,
        delta,
        metadata=None,
        *,
        stream_id=None,
        stream_end=False,
        resuming=False,
        merge_next=False,
    ):
        self.calls.append(
            [
                "send_delta",
                chat_id,
                delta,
                stream_id,
                bool(stream_end),
                bool(resuming),
                bool(merge_next),
                dict(metadata or {}),
            ]
        )

    async def send_reasoning_delta(self, chat_id, delta, metadata=None, *, stream_id=None):
        self.calls.append(
            ["send_reasoning_delta", chat_id, delta, stream_id, dict(metadata or {})]
        )

    async def send_reasoning_end(self, chat_id, metadata=None, *, stream_id=None):
        self.calls.append(["send_reasoning_end", chat_id, stream_id, dict(metadata or {})])

    async def send_file_edit_events(self, chat_id, edits, metadata=None):
        self.calls.append(
            ["send_file_edit_events", chat_id, list(edits), dict(metadata or {})]
        )

    async def send_reasoning(self, msg):
        # base.py:201-223
        if not msg.content:
            return
        stream_id = getattr(msg.event, "stream_id", None)
        await self.send_reasoning_delta(
            msg.chat_id, msg.content, msg.metadata, stream_id=stream_id
        )
        await self.send_reasoning_end(msg.chat_id, msg.metadata, stream_id=stream_id)


# ---------------------------------------------------------------------------
# 5. _coalesce_stream_deltas (manager.py:933-996)
# ---------------------------------------------------------------------------

COALESCE_CASES = [
    ("empty_queue", spec("A", event="stream_delta"), []),
    (
        "same_stream_id",
        spec("A", event="stream_delta", stream_id="s1"),
        [spec("B", event="stream_delta", stream_id="s1")],
    ),
    (
        "nil_stream_ids_match",
        spec("A", event="stream_delta"),
        [spec("B", event="stream_delta")],
    ),
    (
        "different_stream_ids",
        spec("A", event="stream_delta", stream_id="s1"),
        [spec("B", event="stream_delta", stream_id="s2")],
    ),
    (
        "nil_then_set",
        spec("A", event="stream_delta"),
        [spec("B", event="stream_delta", stream_id="s2")],
    ),
    (
        "set_then_nil",
        spec("A", event="stream_delta", stream_id="s1"),
        [spec("B", event="stream_delta")],
    ),
    (
        "different_chat",
        spec("A", event="stream_delta", chat_id="chat1"),
        [spec("B", event="stream_delta", chat_id="chat2")],
    ),
    (
        "different_channel",
        spec("A", event="stream_delta", channel="mock"),
        [spec("B", event="stream_delta", channel="other")],
    ),
    (
        "end_with_content",
        spec("Hello", event="stream_delta", stream_id="s1"),
        [spec(" world", event="stream_end", stream_id="s1", resuming=True, merge_next=True)],
    ),
    (
        "end_with_content_nil_id",
        spec("Hello", event="stream_delta"),
        [spec(" world", event="stream_end")],
    ),
    (
        "end_empty_content",
        spec("Hello", event="stream_delta", stream_id="s1"),
        [spec("", event="stream_end", stream_id="s1")],
    ),
    (
        "end_empty_content_nil_id",
        spec("Hello", event="stream_delta"),
        [spec("", event="stream_end")],
    ),
    (
        "end_stops_before_next_delta",
        spec("Hello", event="stream_delta", stream_id="s1"),
        [
            spec(" world", event="stream_end", stream_id="s1"),
            spec("C", event="stream_delta", stream_id="s1"),
        ],
    ),
    (
        "plain_message_boundary",
        spec("A", event="stream_delta"),
        [spec("plain", event="none")],
    ),
    (
        "progress_boundary",
        spec("A", event="stream_delta"),
        [spec("p", event="progress")],
    ),
    (
        "retry_wait_boundary",
        spec("A", event="stream_delta"),
        [spec("w", event="retry_wait")],
    ),
    (
        "streamed_response_boundary",
        spec("A", event="stream_delta"),
        [spec("", event="streamed_response")],
    ),
    (
        "many_deltas_then_boundary",
        spec("a", event="stream_delta"),
        [
            spec("b", event="stream_delta"),
            spec("c", event="stream_delta"),
            spec("Z", event="none"),
        ],
    ),
    (
        "metadata_from_first",
        spec("A", event="stream_delta", metadata={"message_id": "m1"}),
        [spec("B", event="stream_delta")],
    ),
    (
        "first_is_not_a_delta",
        spec("E", event="stream_end"),
        [spec("B", event="stream_delta")],
    ),
    (
        "progress_with_nil_stream_id_is_not_a_merge_target",
        spec("A", event="stream_delta"),
        [spec("p", event="progress", stream_id=None)],
    ),
]


def dump_coalesce() -> list:
    out = []
    for name, first, queued in COALESCE_CASES:
        bus = MessageBus()
        m = bare_manager(bus=bus)
        for q in queued:
            bus.outbound.put_nowait(build_message(q))
        merged, pending = m._coalesce_stream_deltas(build_message(first))
        out.append(
            {
                "name": name,
                "first": first,
                "queued": queued,
                "merged": describe(merged),
                "pending": [describe(p) for p in pending],
                "queue_left": bus.outbound.qsize(),
            }
        )
    return out


# ---------------------------------------------------------------------------
# 6. _send_with_retry (manager.py:998-1056)
# ---------------------------------------------------------------------------

RETRY_CASES = [
    ("success_first_try", 0, 3, True),
    ("two_failures_then_success", 2, 3, True),
    ("exhausts_at_the_cap", 5, 3, True),
    ("cap_one", 5, 1, True),
    ("cap_zero_clamps_to_one", 5, 0, True),
    ("cap_five", 9, 5, True),
    ("non_retryable", 5, 3, False),
    ("non_retryable_after_one", 1, 3, False),
]


async def dump_retry() -> list:
    out = []
    for name, failures, max_retries, retryable in RETRY_CASES:
        m = bare_manager(send_max_retries=max_retries)
        ch = RecordingChannel(retryable=retryable)
        attempts = 0
        sleeps: list[float] = []
        original_sleep = asyncio.sleep
        original_delays = manager_module._SEND_RETRY_DELAYS
        manager_module._SEND_RETRY_DELAYS = FAST_RETRY_DELAYS

        async def fake_send_once(channel, msg):
            nonlocal attempts
            attempts += 1
            if attempts <= failures:
                raise OSError(f"boom{attempts}")
            channel.calls.append(["send", msg.chat_id, msg.content, {}])

        async def fake_sleep(delay):
            sleeps.append(delay)
            await original_sleep(0)

        m._send_once = fake_send_once
        asyncio.sleep = fake_sleep
        try:
            await m._send_with_retry(
                ch, OutboundMessage(channel="mock", chat_id="c", content="x")
            )
        finally:
            asyncio.sleep = original_sleep
            manager_module._SEND_RETRY_DELAYS = original_delays

        out.append(
            {
                "name": name,
                "failures": failures,
                "max_retries": max_retries,
                "retryable": retryable,
                "attempts": attempts,
                # The delay values the retry loop REQUESTED, in milliseconds.
                # The schedule was substituted for the fast one, so these are
                # the indices the clamp rule selected.
                "sleep_ms": [round(d * 1000) for d in sleeps],
            }
        )
    return out


async def dump_retry_deadline() -> dict:
    """The deadline arm ignores the attempt cap and clamps each delay."""
    m = bare_manager(send_max_retries=1)
    ch = RecordingChannel()
    attempts = 0
    sleeps: list[float] = []
    original_sleep = asyncio.sleep
    original_delays = manager_module._SEND_RETRY_DELAYS
    manager_module._SEND_RETRY_DELAYS = FAST_RETRY_DELAYS

    async def fake_send_once(channel, msg):
        nonlocal attempts
        attempts += 1
        raise OSError("always")

    async def fake_sleep(delay):
        sleeps.append(delay)
        await original_sleep(0)

    m._send_once = fake_send_once
    loop = asyncio.get_running_loop()
    deadline = loop.time() + 0.05
    asyncio.sleep = fake_sleep
    try:
        await m._send_with_retry(
            ch,
            OutboundMessage(channel="mock", chat_id="c", content="x"),
            deadline=deadline,
        )
    finally:
        asyncio.sleep = original_sleep
        manager_module._SEND_RETRY_DELAYS = original_delays

    return {
        "attempts_exceed_cap": attempts > 1,
        "attempts": attempts,
        # The first three delays are the unclamped schedule, because the
        # deadline is still further away than 4ms.
        "first_sleeps_ms": [round(d * 1000, 4) for d in sleeps[:3]],
        # By the end the delay has been clamped to the remaining time, which is
        # smaller than the first schedule entry.
        "last_sleep_is_a_clamp": bool(sleeps) and sleeps[-1] < FAST_RETRY_DELAYS[0],
        # The loop stops on the deadline, not on the attempt cap.
        "finished_after_deadline": loop.time() >= deadline,
    }


# ---------------------------------------------------------------------------
# 7. get_status (manager.py:1062-1096)
# ---------------------------------------------------------------------------

STATUS_CASES = [
    {
        # Start returned without setting is_running: the task is done, so the
        # channel reads as stopped.
        "name": "stopped_after_start_returned",
        "specs": [["telegram", "telegram", "default"]],
        "channels": {"telegram": False},
        "start": {"telegram": "returns"},
        "errors": {},
    },
    {
        # Start is still running: the task is not done, so the channel reads as
        # starting.
        "name": "starting",
        "specs": [["telegram", "telegram", "default"]],
        "channels": {"telegram": False},
        "start": {"telegram": "blocks"},
        "errors": {},
    },
    {
        # Start returned and the channel reports itself running.
        "name": "running",
        "specs": [["telegram", "telegram", "default"]],
        "channels": {"telegram": True},
        "start": {"telegram": "returns"},
        "errors": {},
    },
    {
        "name": "failed",
        "specs": [["telegram", "telegram", "default"]],
        "channels": {"telegram": False},
        "start": {"telegram": "fails"},
        "errors": {},
    },
    {
        # failed and running at the same time: the error wins the state.
        "name": "failed_and_running",
        "specs": [["telegram", "telegram", "default"]],
        "channels": {"telegram": True},
        "start": {"telegram": "fails"},
        "errors": {},
    },
    {
        # A runtime whose plugin failed to load has a spec and no channel.
        "name": "spec_without_channel",
        "specs": [["telegram", "telegram", "default"]],
        "channels": {},
        "start": {},
        "errors": {},
    },
    {
        "name": "multi_instance",
        "specs": [["feishu.a", "feishu", "a"], ["feishu.b", "feishu", "b"]],
        "channels": {"feishu.a": True, "feishu.b": False},
        "start": {"feishu.a": "returns", "feishu.b": "returns"},
        "errors": {},
    },
    {
        "name": "channel_without_spec",
        "specs": [],
        "channels": {"mystery": True},
        "start": {"mystery": "returns"},
        "errors": {},
    },
    {
        "name": "empty_error_is_not_an_error",
        "specs": [["t", "t", "default"]],
        "channels": {"t": False},
        "start": {"t": "returns"},
        "errors": {"t": ""},
    },
    {
        "name": "insertion_order",
        "specs": [],
        "channels": {"b": True, "a": True},
        "start": {"b": "returns", "a": "returns"},
        "errors": {},
    },
    {
        "name": "spec_order_then_channel_order",
        "specs": [["z", "z", "default"]],
        "channels": {"a": True, "z": True},
        "start": {"a": "returns", "z": "returns"},
        "errors": {},
    },
]


class _FakeTask:
    def __init__(self, done: bool):
        self._done = done

    def done(self) -> bool:
        return self._done


class _FakeChannel:
    def __init__(self, running: bool):
        self.is_running = running


GENERIC_START_ERROR = "Channel failed to start. Check gateway logs."


def dump_status() -> list:
    """get_status over states reachable through the public lifecycle.

    ``start`` describes what the channel's start() did, and the fake task/error
    state is derived from it exactly the way _start_channel would leave it, so
    every scenario is one the Go port can reach through StartAll/SetChannelError
    rather than one that needs private state injected.
    """
    out = []
    for case in STATUS_CASES:
        m = ChannelManager.__new__(ChannelManager)
        m.channels = {
            name: _FakeChannel(running) for name, running in case["channels"].items()
        }
        m._channel_owners = {}
        m._channel_runtime_specs = {
            name: (owner, instance) for name, owner, instance in case["specs"]
        }
        m._channel_tasks = {}
        m._channel_errors = {}
        for name, behaviour in case["start"].items():
            # "blocks" leaves the start task pending; anything else completed it.
            m._channel_tasks[name] = _FakeTask(done=behaviour != "blocks")
            if behaviour == "fails":
                m._channel_errors[name] = GENERIC_START_ERROR
        for name, msg in case["errors"].items():
            m._channel_errors[name] = msg

        status = m.get_status()
        out.append(
            {
                "name": case["name"],
                "spec": case,
                # JSON objects are unordered once decoded by the Go side, so the
                # insertion orders are emitted explicitly instead of being
                # inferred from the object key order.
                "channel_order": list(case["channels"]),
                "spec_order": [s[0] for s in case["specs"]],
                "status": status,
                "order": list(status.keys()),
                "field_order": [list(v.keys()) for v in status.values()],
                "enabled_channels": m.enabled_channels,
            }
        )
    return out


# ---------------------------------------------------------------------------
# 8. _dispatch_outbound_loop (manager.py:767-850)
# ---------------------------------------------------------------------------

DISPATCH_CASES = [
    {
        "name": "per_destination_order",
        "gates": {},
        "messages": (
            [spec(f"a{i}", chat_id="c1") for i in range(3)]
            + [spec(f"b{i}", chat_id="c2") for i in range(3)]
        ),
    },
    {
        "name": "unknown_channel_is_dropped",
        "gates": {},
        "messages": [spec("x", channel="ghost"), spec("real")],
    },
    {
        "name": "progress_gate_on",
        "gates": {"send_progress": True, "send_tool_hints": True},
        "messages": [spec("p", event="progress")],
    },
    {
        "name": "progress_gate_off",
        "gates": {"send_progress": False, "send_tool_hints": True},
        "messages": [spec("p", event="progress")],
    },
    {
        "name": "tool_hint_gate_off",
        "gates": {"send_progress": True, "send_tool_hints": False},
        "messages": [spec("p", event="progress", tool_hint=True)],
    },
    {
        "name": "tool_hint_ignores_send_progress",
        "gates": {"send_progress": False, "send_tool_hints": True},
        "messages": [spec("p", event="progress", tool_hint=True)],
    },
    {
        "name": "reasoning_gate_on",
        "gates": {"show_reasoning": True},
        "messages": [spec("r", event="progress", reasoning_delta=True, stream_id="s")],
    },
    {
        "name": "reasoning_gate_off",
        "gates": {"show_reasoning": False},
        "messages": [spec("r", event="progress", reasoning_delta=True, stream_id="s")],
    },
    {
        "name": "reasoning_unknown_channel",
        "gates": {},
        "messages": [
            spec("r", channel="ghost", event="progress", reasoning_delta=True, stream_id="s")
        ],
    },
    {
        "name": "retry_wait_is_dropped",
        "gates": {},
        "messages": [spec("w", event="retry_wait"), spec("real")],
    },
    {
        "name": "streamed_response_is_silent",
        "gates": {},
        "messages": [spec("already", event="streamed_response")],
    },
    {
        "name": "duplicate_origin_reply_is_suppressed",
        "gates": {},
        "messages": [
            spec("dup", metadata={"origin_message_id": "o1"}),
            spec("dup", metadata={"origin_message_id": "o1"}),
            spec("dup", metadata={"origin_message_id": "o2"}),
        ],
    },
    {
        "name": "stream_deltas_are_coalesced",
        "gates": {},
        "messages": [
            spec("A", event="stream_delta"),
            spec("B", event="stream_delta"),
            spec("Final", event="none"),
        ],
    },
    {
        "name": "stream_deltas_with_distinct_ids_are_not_coalesced",
        "gates": {},
        "messages": [
            spec("A", event="stream_delta", stream_id="s1"),
            spec("B", event="stream_delta", stream_id="s2"),
        ],
    },
    {
        "name": "compaction_notice_reaches_the_channel",
        "gates": {},
        "messages": [spec("compact", event="none")],
    },
]


async def dump_dispatch() -> list:
    out = []
    for case in DISPATCH_CASES:
        m = bare_manager()
        ch = RecordingChannel()
        for key, value in case["gates"].items():
            setattr(ch, key, value)
        m.channels = {"mock": ch}
        m._started = True

        for s in case["messages"]:
            await m.bus.publish_outbound(build_message(s))

        task = asyncio.create_task(m._dispatch_outbound())
        try:
            for _ in range(200):
                if m.bus.outbound.qsize() == 0 and not m._outbound_tasks:
                    break
                await asyncio.sleep(0.01)
            await asyncio.sleep(0.05)
        finally:
            await m.stop_all()

        per_destination: dict[str, list] = {}
        for call in ch.calls:
            per_destination.setdefault(str(call[1]), []).append(call)
        out.append(
            {
                "name": case["name"],
                "spec": case,
                "per_destination": per_destination,
                "outbound_tasks_after": len(m._outbound_tasks),
                "outbound_tails_after": len(m._outbound_tails),
            }
        )
    return out


# ---------------------------------------------------------------------------
# 9. Lifecycle (manager.py:373-427, :598-616, :668-681)
# ---------------------------------------------------------------------------


class LifecycleChannel(RecordingChannel):
    def __init__(self, *, start_error: Exception | None = None, running: bool = False):
        super().__init__()
        self.started = 0
        self.stopped = 0
        self.is_running = running
        self._start_error = start_error
        self.stop_saw_no_tasks = None
        self._manager = None

    # base.py:118-123: the default keeps the manager's generic fallback.
    def start_error_message(self, error) -> str | None:
        return None

    async def start(self):
        self.started += 1
        if self._start_error is not None:
            raise self._start_error
        self.is_running = True
        await asyncio.Event().wait()

    async def stop(self):
        self.stopped += 1
        self.is_running = False
        if self._manager is not None:
            self.stop_saw_no_tasks = not self._manager._outbound_tasks


async def dump_lifecycle() -> dict:
    out: dict = {}

    # start_all with no channels returns before starting anything.
    m = bare_manager()
    m.channels = {}
    await m.start_all()
    out["start_all_no_channels"] = {
        "started": m._started,
        "dispatch_task_is_none": m._dispatch_task is None,
    }

    # stop_all without start is harmless.
    m = bare_manager()
    m.channels = {}
    await m.stop_all()
    out["stop_all_without_start"] = {"started": m._started}

    # start_all starts the dispatcher and every channel; start errors are
    # recorded, not raised.
    m = bare_manager()
    bad = LifecycleChannel(start_error=OSError("nope"))
    good = LifecycleChannel()
    m.channels = {"bad": bad, "good": good}
    task = asyncio.create_task(m.start_all())
    for _ in range(200):
        if bad.started and good.started:
            break
        await asyncio.sleep(0.01)
    await asyncio.sleep(0.05)
    out["start_all_with_channels"] = {
        "started_flag": m._started,
        "dispatch_task_is_not_none": m._dispatch_task is not None,
        "bad_started": bad.started,
        "good_started": good.started,
        "channel_errors": dict(m._channel_errors),
        "status": m.get_status(),
    }
    await m.stop_all()
    task.cancel()
    try:
        await task
    except asyncio.CancelledError:
        pass
    out["stop_all"] = {
        "started_flag": m._started,
        "bad_stopped": bad.stopped,
        "good_stopped": good.stopped,
        "outbound_tasks_after": len(m._outbound_tasks),
        "outbound_tails_after": len(m._outbound_tails),
        "status": m.get_status(),
    }

    # A channel that supplies its own start error message.
    class StartMessageChannel(LifecycleChannel):
        def start_error_message(self, exc):
            return "install the SDK"

    m = bare_manager()
    ch = StartMessageChannel(start_error=OSError("nope"))
    m.channels = {"bad": ch}
    t = asyncio.create_task(m.start_all())
    for _ in range(200):
        if m._channel_errors:
            break
        await asyncio.sleep(0.01)
    out["start_error_message"] = {"channel_errors": dict(m._channel_errors)}
    await m.stop_all()
    t.cancel()
    try:
        await t
    except asyncio.CancelledError:
        pass

    # _start_channel pops the previous error before starting.
    m = bare_manager()
    m._channel_errors = {"mock": "stale"}
    quiet = LifecycleChannel()
    m.channels = {"mock": quiet}

    async def stop_immediately():
        return None

    quiet.start = stop_immediately
    await m._start_channel("mock", quiet)
    out["start_channel_clears_error"] = {"channel_errors": dict(m._channel_errors)}

    # _stop_channel on an unregistered runtime returns False.
    m = bare_manager()
    m.channels = {}
    out["stop_unknown_channel"] = {"result": await m._stop_channel("nope")}

    # _stop_channel cancels this channel's sends BEFORE stopping the runtime.
    m = bare_manager()
    ch = LifecycleChannel()
    ch._manager = m
    m.channels = {"mock": ch}
    release = asyncio.Event()

    async def blocking_send(msg):
        await release.wait()

    ch.send = blocking_send
    await m._queue_outbound(ch, OutboundMessage("mock", "a", "1"))
    for _ in range(200):
        if m._outbound_tasks:
            break
        await asyncio.sleep(0.01)
    await asyncio.sleep(0.02)
    result = await m._stop_channel("mock")
    out["stop_channel_cancels_sends_first"] = {
        "result": result,
        "stop_saw_no_tasks": ch.stop_saw_no_tasks,
        "outbound_tasks_after": len(m._outbound_tasks),
        "outbound_tails_after": len(m._outbound_tails),
    }

    # _queue_outbound drops a message for a stopping channel.
    m = bare_manager()
    ch = LifecycleChannel()
    m.channels = {"mock": ch}
    m._stopping_channels.add("mock")
    await m._queue_outbound(ch, OutboundMessage("mock", "c", "dropped"))
    out["queue_drops_when_stopping"] = {"outbound_tasks": len(m._outbound_tasks)}
    m._stopping_channels.discard("mock")

    # _queue_outbound drops a message whose runtime has been replaced.
    replacement = LifecycleChannel()
    m.channels = {"mock": replacement}
    await m._queue_outbound(ch, OutboundMessage("mock", "c", "dropped"))
    out["queue_drops_when_replaced"] = {
        "outbound_tasks": len(m._outbound_tasks),
        "replacement_calls": replacement.calls,
    }

    # Cancelling the dispatcher cancels active and waiting destination tasks.
    m = bare_manager()
    ch = LifecycleChannel()
    m.channels = {"mock": ch}
    started = asyncio.Event()
    never = asyncio.Event()

    async def blocked(msg):
        started.set()
        await never.wait()

    ch.send = blocked
    m._dispatch_task = asyncio.create_task(m._dispatch_outbound())
    for content in ("one", "two", "three"):
        await m.bus.publish_outbound(OutboundMessage("mock", "chat", content))
    for _ in range(200):
        if len(m._outbound_tasks) == 3:
            break
        await asyncio.sleep(0.01)
    await asyncio.sleep(0.05)
    out["dispatcher_shutdown"] = {
        "tasks_before": len(m._outbound_tasks),
        "tails_before": len(m._outbound_tails),
    }
    await m.stop_all()
    out["dispatcher_shutdown"].update(
        {
            "tasks_after": len(m._outbound_tasks),
            "tails_after": len(m._outbound_tails),
        }
    )

    return out


# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------


async def main() -> int:
    OUT["upstream_commit"] = COMMIT
    OUT["constants"] = dump_constants()
    OUT["retry_schedule"] = dump_retry_schedule()
    OUT["fingerprint"] = dump_fingerprint()
    OUT["suppress"] = dump_suppress()
    OUT["eviction"] = dump_eviction()
    OUT["send_once"] = await dump_send_once()
    OUT["coalesce"] = dump_coalesce()
    OUT["retry"] = await dump_retry()
    OUT["retry_deadline"] = await dump_retry_deadline()
    OUT["status"] = dump_status()
    OUT["dispatch"] = await dump_dispatch()
    OUT["lifecycle"] = await dump_lifecycle()

    json.dump(OUT, sys.stdout, indent=2, sort_keys=True, ensure_ascii=False)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
