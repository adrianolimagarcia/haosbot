#!/usr/bin/env python3
"""Bus dispatch differential harness — the Python half.

Drives the frozen reference ``MessageBus`` through a fixed set of subscribe /
unsubscribe / publish scenarios and prints the resulting handler-call sequence
for each, as JSON on stdout.

The Go counterpart (compat/bus_differential_test.go) runs the identical
scenarios against internal/bus and requires the sequences to match. Nothing here
is transcribed from documentation: every expectation is the reference's actual
output.

The scenario that motivates the harness is ``peer_unsubscribed_mid_dispatch``.
The reference snapshots the handler LIST at the top of publish
(queue.py:120, ``list(self._handlers)``) but each entry re-tests its own
``active`` flag when it is CALLED (queue.py:104-107), so a handler that
unsubscribes a peer suppresses that peer for the dispatch already running.
"""
from __future__ import annotations

import asyncio
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "upstream" / "nanobot"))

from nanobot.bus.queue import MessageBus  # noqa: E402
from nanobot.events import AgentEvent  # noqa: E402


class ProbeEvent(AgentEvent):
    """A concrete event type; the bus never inspects it."""


def scenario_peer_unsubscribed_mid_dispatch() -> list[str]:
    """A unsubscribes its peer B during the dispatch. B must not be called."""
    bus = MessageBus()
    calls: list[str] = []
    box: dict[str, object] = {}

    def a(event: AgentEvent) -> None:
        calls.append("A")
        box["unsub_b"]()

    def b(event: AgentEvent) -> None:
        calls.append("B")

    bus.subscribe(a)
    box["unsub_b"] = bus.subscribe(b)

    asyncio.run(bus.publish(ProbeEvent()))
    return calls


def scenario_peer_unsubscribed_before_publish() -> list[str]:
    """The control: unsubscribing before the dispatch suppresses B as well."""
    bus = MessageBus()
    calls: list[str] = []

    def a(event: AgentEvent) -> None:
        calls.append("A")

    def b(event: AgentEvent) -> None:
        calls.append("B")

    bus.subscribe(a)
    unsub_b = bus.subscribe(b)
    unsub_b()

    asyncio.run(bus.publish(ProbeEvent()))
    return calls


def scenario_subscription_added_mid_dispatch() -> dict[str, list[str]]:
    """A handler registered during a dispatch is not reached until the next one."""
    bus = MessageBus()
    calls: list[str] = []

    def a(event: AgentEvent) -> None:
        calls.append("A")
        bus.subscribe(c)

    def b(event: AgentEvent) -> None:
        calls.append("B")

    def c(event: AgentEvent) -> None:
        calls.append("C")

    bus.subscribe(a)
    bus.subscribe(b)

    asyncio.run(bus.publish(ProbeEvent()))
    first = list(calls)
    asyncio.run(bus.publish(ProbeEvent()))
    return {"first": first, "second": calls}


def scenario_self_unsubscribe_mid_dispatch() -> dict[str, list[str]]:
    """A handler that unsubscribes itself is not called again."""
    bus = MessageBus()
    calls: list[str] = []

    def a(event: AgentEvent) -> None:
        calls.append("A")
        unsub_a()

    def b(event: AgentEvent) -> None:
        calls.append("B")

    unsub_a = bus.subscribe(a)
    bus.subscribe(b)

    asyncio.run(bus.publish(ProbeEvent()))
    first = list(calls)
    asyncio.run(bus.publish(ProbeEvent()))
    return {"first": first, "second": calls}


def scenario_registration_order() -> list[str]:
    """Handlers run in registration order."""
    bus = MessageBus()
    calls: list[str] = []
    for name in ("A", "B", "C"):
        bus.subscribe(lambda event, name=name: calls.append(name))
    asyncio.run(bus.publish(ProbeEvent()))
    return calls


def scenario_raising_handler_is_isolated() -> list[str]:
    """A raising handler must not stop delivery to the ones after it."""
    bus = MessageBus()
    calls: list[str] = []

    def boom(event: AgentEvent) -> None:
        calls.append("A")
        raise RuntimeError("boom")

    def b(event: AgentEvent) -> None:
        calls.append("B")

    bus.subscribe(boom)
    bus.subscribe(b)

    asyncio.run(bus.publish(ProbeEvent()))
    return calls


def main() -> int:
    doc = {
        "peer_unsubscribed_mid_dispatch": scenario_peer_unsubscribed_mid_dispatch(),
        "peer_unsubscribed_before_publish": scenario_peer_unsubscribed_before_publish(),
        "subscription_added_mid_dispatch": scenario_subscription_added_mid_dispatch(),
        "self_unsubscribe_mid_dispatch": scenario_self_unsubscribe_mid_dispatch(),
        "registration_order": scenario_registration_order(),
        "raising_handler_is_isolated": scenario_raising_handler_is_isolated(),
    }
    json.dump(doc, sys.stdout, sort_keys=True)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
