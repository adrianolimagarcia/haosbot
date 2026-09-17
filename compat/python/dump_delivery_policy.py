#!/usr/bin/env python3
"""Dump per-channel delivery-policy ground truth from the frozen nanobot reference.

This is one half of the delivery-policy differential harness. It EXECUTES the
real Python implementation (upstream/nanobot @ 1bb712d3,
nanobot/channels/manager.py and nanobot/channels/base.py) and emits a single
JSON document on stdout; compat/delivery_policy_differential_test.go feeds the
Go port the same sections and compares the resolved triples.

Nothing here is transcribed from documentation or from reading the source: every
expected value is produced by running the reference.

What is executed, exactly:

  Config.model_validate(raw)                     global channels policy
  BaseChannel.progress_transport_defaults(None)  the hook's default -> None
  ChannelManager._resolve_bool_override(...)     manager.py:355-368
  `hook or (global, global)`                     manager.py:219-221, literally

It is a separate file from dump_manager.py on purpose: that dumper and its Go
test are edited concurrently by several agents, and a read-modify-write race
there would destroy work. This dumper and its Go test share nothing but repoRoot.

Run with the project venv:
    .tools/venv/bin/python compat/python/dump_delivery_policy.py
"""
from __future__ import annotations

import ast
import json
import sys
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

from nanobot.config.schema import Config  # noqa: E402
from nanobot.channels.base import BaseChannel  # noqa: E402
import nanobot.channels.manager as manager_module  # noqa: E402
from nanobot.channels.manager import ChannelManager  # noqa: E402

UPSTREAM_COMMIT = "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9"

# The channel key used throughout. It is a plain dict inside ChannelsConfig
# (extra="allow"), which is the dict branch of _resolve_bool_override.
CHANNEL = "telegram"


# ---------------------------------------------------------------------------
# Cases
#
# `hook` is the value progress_transport_defaults() returns for the channel
# under test. None means "the channel keeps the global policy", which is what
# the reference's BaseChannel returns and what Telegram inherits. A two-element
# list models a channel that declares its own transport default.
# ---------------------------------------------------------------------------
CASES: list[dict] = [
    {
        "label": "absent_keys_use_the_global_defaults",
        "config": {"channels": {CHANNEL: {"enabled": True, "token": "T"}}},
        "hook": None,
    },
    {
        "label": "global_false_is_the_default_the_channel_inherits",
        "config": {"channels": {
            "sendProgress": False, "sendToolHints": False, "showReasoning": False,
            CHANNEL: {"enabled": True, "token": "T"},
        }},
        "hook": None,
    },
    {
        "label": "global_policy_is_per_key_not_all_or_nothing",
        "config": {"channels": {
            "sendProgress": False,
            CHANNEL: {"enabled": True, "token": "T"},
        }},
        "hook": None,
    },
    {
        "label": "global_snake_case_spelling_is_accepted_too",
        "config": {"channels": {
            "send_progress": False, "send_tool_hints": False, "show_reasoning": False,
            CHANNEL: {"enabled": True, "token": "T"},
        }},
        "hook": None,
    },
    {
        "label": "per_channel_false_overrides_the_true_global",
        "config": {"channels": {CHANNEL: {
            "enabled": True, "token": "T",
            "sendProgress": False, "sendToolHints": False, "showReasoning": False,
        }}},
        "hook": None,
    },
    {
        "label": "per_channel_true_overrides_the_false_global",
        "config": {"channels": {
            "sendProgress": False, "sendToolHints": False, "showReasoning": False,
            CHANNEL: {
                "enabled": True, "token": "T",
                "sendProgress": True, "sendToolHints": True, "showReasoning": True,
            },
        }},
        "hook": None,
    },
    {
        "label": "snake_case_section_key_is_read_directly",
        "config": {"channels": {
            "sendProgress": False, "sendToolHints": False, "showReasoning": False,
            CHANNEL: {
                "enabled": True, "token": "T",
                "send_progress": True, "send_tool_hints": True, "show_reasoning": True,
            },
        }},
        "hook": None,
    },
    {
        "label": "camel_case_alias_is_the_fallback_key",
        "config": {"channels": {
            "sendProgress": False, "sendToolHints": False, "showReasoning": False,
            CHANNEL: {
                "enabled": True, "token": "T",
                "sendProgress": True, "sendToolHints": True, "showReasoning": True,
            },
        }},
        "hook": None,
    },
    {
        "label": "string_false_is_not_a_bool_so_the_true_default_wins",
        "config": {"channels": {CHANNEL: {
            "enabled": True, "token": "T",
            "sendProgress": "false", "sendToolHints": "false", "showReasoning": "false",
        }}},
        "hook": None,
    },
    {
        "label": "string_true_is_not_a_bool_so_the_false_default_wins",
        "config": {"channels": {
            "sendProgress": False, "sendToolHints": False, "showReasoning": False,
            CHANNEL: {
                "enabled": True, "token": "T",
                "sendProgress": "true", "sendToolHints": "true", "showReasoning": "true",
            },
        }},
        "hook": None,
    },
    {
        "label": "non_bool_numbers_fall_back_to_the_default",
        "config": {"channels": {CHANNEL: {
            "enabled": True, "token": "T",
            "sendProgress": 0, "sendToolHints": 0, "showReasoning": 0,
        }}},
        "hook": None,
    },
    {
        "label": "non_bool_containers_fall_back_to_the_default",
        "config": {"channels": {CHANNEL: {
            "enabled": True, "token": "T",
            "sendProgress": [], "sendToolHints": {}, "showReasoning": [],
        }}},
        "hook": None,
    },
    {
        "label": "json_null_falls_back_to_the_default",
        "config": {"channels": {
            "sendProgress": False, "sendToolHints": False, "showReasoning": False,
            CHANNEL: {
                "enabled": True, "token": "T",
                "sendProgress": None, "sendToolHints": None, "showReasoning": None,
            },
        }},
        "hook": None,
    },
    {
        "label": "null_snake_case_key_falls_through_to_the_camel_alias",
        "config": {"channels": {
            "sendProgress": False, "sendToolHints": False, "showReasoning": False,
            CHANNEL: {
                "enabled": True, "token": "T",
                "send_progress": None, "sendProgress": True,
                "send_tool_hints": None, "sendToolHints": True,
                "show_reasoning": None, "showReasoning": True,
            },
        }},
        "hook": None,
    },
    {
        "label": "snake_case_key_wins_over_the_camel_alias",
        "config": {"channels": {
            "sendProgress": True, "sendToolHints": True, "showReasoning": True,
            CHANNEL: {
                "enabled": True, "token": "T",
                "send_progress": False, "sendProgress": True,
                "send_tool_hints": False, "sendToolHints": True,
                "show_reasoning": False, "showReasoning": True,
            },
        }},
        "hook": None,
    },
    {
        # `if value is None` is the ONLY thing that reaches the alias: a present
        # but non-bool snake_case value stops there and the DEFAULT wins, even
        # when the alias holds a real bool.
        "label": "present_non_bool_snake_case_value_blocks_the_camel_alias",
        "config": {"channels": {
            "sendProgress": False, "sendToolHints": False, "showReasoning": False,
            CHANNEL: {
                "enabled": True, "token": "T",
                "send_progress": "false", "sendProgress": True,
                "send_tool_hints": 0, "sendToolHints": True,
                "show_reasoning": [], "showReasoning": True,
            },
        }},
        "hook": None,
    },
    # --- the transport-default hook (manager.py:219-221) -------------------
    {
        "label": "transport_default_false_false_beats_the_true_global",
        "config": {"channels": {CHANNEL: {"enabled": True, "token": "T"}}},
        "hook": [False, False],
    },
    {
        "label": "transport_default_true_true_beats_the_false_global",
        "config": {"channels": {
            "sendProgress": False, "sendToolHints": False,
            CHANNEL: {"enabled": True, "token": "T"},
        }},
        "hook": [True, True],
    },
    {
        "label": "transport_default_does_not_cover_show_reasoning",
        "config": {"channels": {
            "showReasoning": False,
            CHANNEL: {"enabled": True, "token": "T"},
        }},
        "hook": [True, True],
    },
    {
        "label": "section_override_still_beats_the_transport_default",
        "config": {"channels": {CHANNEL: {
            "enabled": True, "token": "T", "sendProgress": True,
        }}},
        "hook": [False, False],
    },
]


def hook_owners() -> dict[str, list[str]]:
    """Every class in the reference that defines progress_transport_defaults.

    Read with ast rather than by eye, and rather than by import: the telegram
    runtime imports the third-party `telegram` package, which this venv does not
    install.
    """
    owners: dict[str, list[str]] = {}
    for path in sorted((ROOT / "upstream" / "nanobot" / "nanobot").rglob("*.py")):
        tree = ast.parse(path.read_text())
        for node in ast.walk(tree):
            if not isinstance(node, ast.ClassDef):
                continue
            for item in node.body:
                if isinstance(item, (ast.FunctionDef, ast.AsyncFunctionDef)) and (
                    item.name == "progress_transport_defaults"
                ):
                    owners.setdefault(node.name, []).append(
                        str(path.relative_to(ROOT / "upstream" / "nanobot"))
                    )
    return owners


def resolve_case(case: dict) -> dict:
    """Run the reference over one case and return its inputs and its answer."""
    cfg = Config.model_validate(case["config"])
    section = getattr(cfg.channels, CHANNEL, None)

    # manager.py:219-221, literally. `hook` is None for a channel that does not
    # override the hook; `or` on a 2-tuple tests LENGTH, so (False, False) is
    # truthy and is honoured.
    if case["hook"] is None:
        hook = BaseChannel.progress_transport_defaults(None)
    else:
        hook = tuple(case["hook"])
    progress_default, tool_hints_default = hook or (
        cfg.channels.send_progress,
        cfg.channels.send_tool_hints,
    )

    # manager.py:222-231. Called unbound: the method never touches `self`.
    resolved = [
        ChannelManager._resolve_bool_override(
            None, section, "send_progress", progress_default),
        ChannelManager._resolve_bool_override(
            None, section, "send_tool_hints", tool_hints_default),
        ChannelManager._resolve_bool_override(
            None, section, "show_reasoning", cfg.channels.show_reasoning),
    ]

    return {
        "label": case["label"],
        "section": section,
        "global": [
            cfg.channels.send_progress,
            cfg.channels.send_tool_hints,
            cfg.channels.show_reasoning,
        ],
        "hook": None if case["hook"] is None else list(case["hook"]),
        "expected": resolved,
    }


def main() -> dict:
    return {
        "upstream_commit": UPSTREAM_COMMIT,
        "channel": CHANNEL,
        "base_hook_returns_none": BaseChannel.progress_transport_defaults(None) is None,
        "hook_owners": hook_owners(),
        "camel_aliases": dict(manager_module._BOOL_CAMEL_ALIASES),
        "cases": [resolve_case(case) for case in CASES],
    }


if __name__ == "__main__":
    print(json.dumps(main(), ensure_ascii=False, sort_keys=True, indent=None))
