#!/usr/bin/env python3
"""Dump reference values for the outbound-event layer.

This is a second, independent dumper alongside dump_reference.py. It exists
because dump_reference.py is a shared file that several agents edit
concurrently, and a read-modify-write race there would destroy work. Keeping
this section in its own file and its own Go test file removes the race
entirely.

Run with the project venv:
    .tools/venv/bin/python compat/python/dump_outbound_events.py

Covers nanobot/bus/outbound_events.py and nanobot/bus/notification_delivery.py
at HKUDS/nanobot@1bb712d3488915ca4ed9ccc1a93067ff722f5ab9.
"""
from __future__ import annotations

import itertools
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "upstream" / "nanobot"))

# loguru writes diagnostics to stderr; this harness must emit exactly one JSON
# document on stdout so the Go side can parse it.
try:
    from loguru import logger as _loguru_logger

    _loguru_logger.remove()
except Exception:
    pass


def _events():
    """Every event type in the hierarchy, built with representative values."""
    from nanobot.bus.outbound_events import (
        FileEditEvent,
        ProgressEvent,
        StreamDeltaEvent,
        StreamEndEvent,
        StreamedResponseEvent,
    )
    from nanobot.events import (
        ContextCompactionEvent,
        RecoveryStateEvent,
        RetryStatusEvent,
        RetryWaitEvent,
    )

    return [
        ("ProgressEvent", ProgressEvent(content="working")),
        ("ProgressEvent.empty", ProgressEvent()),
        ("FileEditEvent", FileEditEvent(content="edited", file_edit_events=[{"path": "a"}])),
        ("FileEditEvent.empty", FileEditEvent()),
        ("RetryWaitEvent", RetryWaitEvent(content="retrying")),
        ("RetryWaitEvent.empty", RetryWaitEvent()),
        ("RetryStatusEvent", RetryStatusEvent(state="waiting", attempt=1, max_attempts=None, error_kind="rate_limit")),
        ("RecoveryStateEvent", RecoveryStateEvent(status="recovered", recovery_id="r1")),
        ("StreamDeltaEvent", StreamDeltaEvent(content="chunk", stream_id="s1")),
        ("StreamEndEvent", StreamEndEvent(content="final", stream_id="s1", resuming=True, merge_next=True)),
        ("StreamedResponseEvent", StreamedResponseEvent()),
        ("ContextCompactionEvent.started", ContextCompactionEvent(compaction_id="c1", phase="started")),
        ("ContextCompactionEvent.succeeded", ContextCompactionEvent(compaction_id="c1", phase="succeeded")),
        ("ContextCompactionEvent.failed", ContextCompactionEvent(compaction_id="c1", phase="failed")),
        ("ContextCompactionEvent.cancelled", ContextCompactionEvent(compaction_id="c1", phase="cancelled")),
    ]


def dump_event_content():
    """_event_content (outbound_events.py:148) — the text fallback for events
    that reach a channel which cannot render them natively."""
    from nanobot.bus.outbound_events import _event_content

    out = []
    for name, event in _events():
        out.append({
            "name": name,
            # The Python events carry no name attribute; the class name IS the
            # identity, and it is what the Go EventName() must reproduce.
            "event_name": type(event).__name__,
            "content": _event_content(event),
        })
    return out


def dump_progress_isinstance():
    """The subclass relation Go has to reproduce without inheritance.

    FileEditEvent subclasses ProgressEvent, so isinstance(event, ProgressEvent)
    is True for it. A naive Go type assertion to ProgressEvent would reject it
    and silently send file-edit events down the wrong branch.
    """
    from nanobot.bus.outbound_events import FileEditEvent, ProgressEvent, StreamEndEvent

    def mro_names(obj):
        return [t.__name__ for t in type(obj).__mro__]

    return {
        "file_edit_is_progress": isinstance(FileEditEvent(content="x"), ProgressEvent),
        "file_edit_mro": mro_names(FileEditEvent(content="x")),
        "progress_mro": mro_names(ProgressEvent(content="x")),
        "stream_end_is_progress": isinstance(StreamEndEvent(content="x"), ProgressEvent),
    }


def dump_outbound_message_for_event():
    """outbound_message_for_event (outbound_events.py:114).

    `content` is tri-state in the reference: omitted, an explicit string, and an
    explicit empty string all behave differently. JSON has no "omitted", so the
    Go side passes a pointer; the dump records which arm was exercised.
    """
    from nanobot.bus.outbound_events import ProgressEvent, outbound_message_for_event

    event = ProgressEvent(content="derived text")
    arms = [
        ("omitted", {}),
        ("explicit", {"content": "explicit text"}),
        ("explicit_empty", {"content": ""}),
        ("metadata", {"metadata": {"message_id": "7"}}),
    ]
    out = []
    for name, kwargs in arms:
        msg = outbound_message_for_event(
            channel="telegram", chat_id="42", event=event, **kwargs
        )
        out.append({
            "name": name,
            "content": msg.content,
            "metadata": dict(msg.metadata),
            "channel": msg.channel,
            "chat_id": msg.chat_id,
        })
    return out


def dump_replace_outbound_event():
    """replace_outbound_event (outbound_events.py:133).

    Records that metadata is SHARED with the base message rather than copied,
    that an explicit empty content string is honoured, and that the base
    message is left untouched.
    """
    from nanobot.bus.events import OutboundMessage
    from nanobot.bus.outbound_events import StreamEndEvent, replace_outbound_event

    base = OutboundMessage(channel="slack", chat_id="9", content="old", metadata={"k": "v"})
    replaced = replace_outbound_event(base, StreamEndEvent(content="new"))
    explicit_empty = replace_outbound_event(base, StreamEndEvent(content="new"), content="")
    return {
        "content": replaced.content,
        "channel": replaced.channel,
        "chat_id": replaced.chat_id,
        "metadata": dict(replaced.metadata),
        "metadata_is_shared": replaced.metadata is base.metadata,
        "explicit_empty_content": explicit_empty.content,
        "base_content_unchanged": base.content,
        "base_event_unchanged": base.event is None,
    }


def dump_notification_delivery():
    """notification_is_deliverable (notification_delivery.py:28) over the full
    cross product of event type x channel x publish_lifecycle.

    The default is DENY: an event type absent from the table is not deliverable
    on any channel.
    """
    from nanobot.bus.notification_delivery import (
        NOTIFICATION_AUDIENCES,
        notification_is_deliverable,
    )
    from nanobot.bus.outbound_events import (
        FileEditEvent,
        ProgressEvent,
        StreamDeltaEvent,
        StreamEndEvent,
        StreamedResponseEvent,
    )
    from nanobot.events import (
        ContextCompactionEvent,
        RecoveryStateEvent,
        RetryStatusEvent,
        RetryWaitEvent,
    )

    types = [
        ProgressEvent, FileEditEvent, StreamDeltaEvent, StreamEndEvent,
        StreamedResponseEvent, ContextCompactionEvent, RetryWaitEvent,
        RetryStatusEvent, RecoveryStateEvent,
    ]
    audiences = {t.__name__: a for t, a in NOTIFICATION_AUDIENCES.items()}
    cases = []
    for typ, channel, publish in itertools.product(
        types, ["telegram", "websocket", "slack"], [False, True]
    ):
        cases.append({
            "type": typ.__name__,
            "channel": channel,
            "publish_lifecycle": publish,
            "deliverable": notification_is_deliverable(
                typ, channel=channel, publish_lifecycle=publish
            ),
        })
    return {"audiences": audiences, "cases": cases}


def main() -> int:
    doc = {
        "upstream_commit": "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9",
        "event_content": dump_event_content(),
        "progress_isinstance": dump_progress_isinstance(),
        "outbound_message_for_event": dump_outbound_message_for_event(),
        "replace_outbound_event": dump_replace_outbound_event(),
        "notification_delivery": dump_notification_delivery(),
    }
    json.dump(doc, sys.stdout, indent=2, sort_keys=True, ensure_ascii=False)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
