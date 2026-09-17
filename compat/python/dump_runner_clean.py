#!/usr/bin/env python3
"""Dump `clean`-related ground truth from the frozen nanobot reference.

This is one half of the runner-clean differential harness. It executes the REAL
Python implementation (upstream/nanobot @ 1bb712d3) and emits a single JSON
document on stdout; compat/runner_clean_differential_test.go drives the Go port
through the same inputs and compares field by field.

The subject is the value the reference calls `clean`:

    clean = hook.finalize_content(context, response.content)   # runner.py:588

`build_agent_turn_hook` always installs an AgentProgressHook as the first hook
of a turn (turn_hooks.py:45), and that hook's finalize_content is
`strip_think(content) or None` (progress_hook.py:46-49, :180-181). Everything
below is therefore the real thing, not a paraphrase of it:

  * AgentProgressHook._strip_think / utils.helpers.strip_think over an
    adversarial corpus (inline tags, unclosed tags, nested tags, tags only,
    Python-only whitespace at the boundaries, multi-line thinking);
  * utils.runtime.is_blank_text over the same corpus;
  * AgentRunner._restore_outer_whitespace over content/original combinations;
  * the empty-content retry predicate of runner.py:588-593 as a full truth
    table, built from real LLMResponse objects so `should_execute_tools` and
    `has_tool_calls` are the reference's own properties;
  * LLMProvider._error_response_from_exception, exercised with exceptions whose
    str() is empty or whitespace-only, to pin
    `detail = str(exc).strip() or type(exc).__name__` (base.py:994).

Nothing is transcribed from reading the source: every value is produced by
running the reference. No temporary files are created, so this dumper cannot
collide with any other harness.

Run with the project venv:
    .tools/venv/bin/python compat/python/dump_runner_clean.py
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

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

from nanobot.agent.progress_hook import AgentProgressHook  # noqa: E402
from nanobot.agent.runner import _restore_outer_whitespace  # noqa: E402
from nanobot.providers.base import LLMProvider, LLMResponse, ToolCallRequest  # noqa: E402
from nanobot.utils.helpers import strip_think  # noqa: E402
from nanobot.utils.runtime import is_blank_text  # noqa: E402

UPSTREAM_COMMIT = "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9"

# The whitespace set Python's str.strip() removes but Go's strings.TrimSpace /
# unicode.IsSpace does not — U+001C..U+001F, FILE/GROUP/RECORD/UNIT SEPARATOR —
# is the trap this harness exists to catch, so every corpus entry that can
# separate the two definitions is present. NBSP U+00A0 and the other Unicode
# spaces (U+3000, U+2028, U+0085, U+1680, U+2000..U+200A, U+202F, U+205F) are
# whitespace for BOTH runtimes and are included as controls: the Go side counts
# how many entries actually diverge, so a corpus that stopped exercising the
# trap fails instead of passing vacuously.
CORPUS: list[tuple[str, str]] = [
    ("empty", ""),
    ("space", " "),
    ("ascii_ws", " \t\n\r\v\f "),
    ("nbsp", "\u00a0"),
    ("nbsp_boundary", "\u00a0hello\u00a0"),
    ("u001c", "\x1c"),
    ("u001c_u001f", "\x1c\x1d\x1e\x1f"),
    ("u001c_boundary", "\x1ctext\x1f"),
    ("u3000", "\u3000"),
    ("u3000_boundary", "\u3000text\u3000"),
    ("u2028", "\u2028"),
    ("u2028_boundary", "\u2028text\u2029"),
    ("u0085", "\u0085"),
    ("u1680", "\u1680"),
    ("u2000_boundary", "\u2000text\u200a"),
    ("u200b_not_space", "\u200b"),
    ("u200b_boundary", "\u200btext\u200b"),
    ("feff_not_space", "\ufefftext\ufeff"),
    ("plain", "hello world"),
    ("plain_ws", "  hello world  "),
    ("think_block", "<think>reasoning</think>answer"),
    ("think_block_ws", "  <think>reasoning</think>  answer  "),
    ("think_only", "<think>reasoning</think>"),
    ("think_only_ws", "  <think>reasoning</think>  "),
    ("thinking_block", "<thinking>r</thinking>a"),
    ("thought_block", "<thought>r</thought>a"),
    ("nested_think", "<think>outer<think>inner</think>tail</think>answer"),
    ("middle_tag", "before<think>mid</think>after"),
    ("unclosed", "<think>never closed"),
    ("unclosed_ws", "  <think>never closed"),
    ("close_only_start", "</think>answer"),
    ("close_only_end", "answer</think>"),
    ("close_only_middle", "ans</think>wer"),
    ("malformed_open_cjk", "<think\u5e7f\u573aanswer"),
    ("malformed_open_then_text", "<thinkx answer"),
    ("self_closing", "<thinking/>answer"),
    ("channel_marker", "<|channel|>answer"),
    ("channel_marker2", "<channel|>answer"),
    ("partial_tag", "answer<thi"),
    ("partial_tag_th", "answer<th"),
    ("lone_pipe", "<|"),
    ("multiline_think", "<think>line1\nline2\n\nline4</think>\nfinal answer\n"),
    ("multiline_plain", "line1\nline2\n\n"),
    ("think_and_nbsp", "\u00a0<think>x</think>\u00a0answer\u00a0"),
    ("think_inner_ws", "<think>  spaced  </think>  answer"),
    ("multiple_blocks", "<think>a</think>one<think>b</think>two"),
    ("ws_around_tag_only", "   <think>x</think>   "),
    ("u001c_then_think", "\x1c<think>x</think>\x1canswer\x1c"),
]

# Content values fed to _restore_outer_whitespace, and the originals they are
# paired with. Both dimensions deliberately include the non-ASCII whitespace
# set: the first version of the Go port used the six ASCII characters as the
# cutset and silently DROPPED every one of these characters.
RESTORE_CONTENTS = ["", "X", " X ", "X\n", "\u00a0X", "X\u200b", "seg"]
RESTORE_ORIGINALS = [
    None, "", " ", "  b  ", "\u00a0hi", "hi\u00a0", "\x1chi\x1f",
    "\u3000hi\u3000", "   ", "\n x \n", "hi\u200b", "\u2028hi\u2029",
    "\u00a0", "\x1c\x1d", "b",
]

# The finish reasons the port models, spelled exactly as LLMResponse carries
# them.
FINISH_REASONS = ["stop", "tool_calls", "function_call", "length",
                  "content_filter", "refusal", "error"]

# None, blank and non-blank content, plus a non-blank input whose `clean` is
# empty — the case a "is the raw text empty?" test gets wrong.
CONTENT_KINDS: list[tuple[str, str | None]] = [
    ("none", None),
    ("blank", "   "),
    ("nonblank", "answer"),
    ("think_only", "<think>x</think>"),
]


def dump_strip_think() -> list[dict]:
    """strip_think / AgentProgressHook._strip_think over the corpus.

    `clean` is the hook's output, which is None whenever the stripped text is
    empty — the `or None` that makes "" and None indistinguishable downstream.
    """
    out = []
    for label, text in CORPUS:
        out.append({
            "label": label,
            "in": text,
            "strip_think": strip_think(text),
            "clean": AgentProgressHook._strip_think(text),
            "clean_idempotent": AgentProgressHook._strip_think(
                AgentProgressHook._strip_think(text) or ""
            ),
        })
    return out


def dump_is_blank_text() -> list[dict]:
    """is_blank_text over the corpus, plus a None entry.

    The Go side compares agent.IsBlankText against `result` and separately
    counts how many entries disagree with strings.TrimSpace, so a corpus that
    stopped exercising the U+001C..U+001F trap fails instead of passing.
    """
    out = []
    for label, text in CORPUS:
        out.append({
            "label": label,
            "in": text,
            "result": is_blank_text(text),
        })
    out.append({"label": "none", "in": None, "result": is_blank_text(None)})
    return out


def dump_restore_outer_whitespace() -> list[dict]:
    out = []
    for content in RESTORE_CONTENTS:
        for original in RESTORE_ORIGINALS:
            out.append({
                "content": content,
                "original": original,
                "result": _restore_outer_whitespace(content, original),
            })
    return out


def _tool_call() -> ToolCallRequest:
    return ToolCallRequest(id="c1", name="echo", arguments={})


def dump_empty_retry_truth_table() -> list[dict]:
    """The runner.py:588-593 predicate over every combination.

    Built from real LLMResponse objects so `has_tool_calls` and
    `should_execute_tools` are the reference's own properties rather than a
    restatement of them (base.py:598-608).

    `clean` is what the hook chain produces for that content, and
    `retry_eligible` is the predicate the reference actually evaluates:
    finish_reason not in the exclusion set AND is_blank_text(clean). Whether
    the check is REACHED at all is `not should_execute_tools`, because the
    tool-execution branch above line 588 ends in `continue`.
    """
    excluded = {"error", "length", "refusal", "content_filter"}
    out = []
    for reason in FINISH_REASONS:
        for kind, content in CONTENT_KINDS:
            for with_tools in (False, True):
                calls = [_tool_call()] if with_tools else []
                response = LLMResponse(
                    content=content,
                    tool_calls=calls,
                    finish_reason=reason,
                )
                clean = AgentProgressHook._strip_think(content)
                reached = not response.should_execute_tools
                out.append({
                    "finish_reason": reason,
                    "content_kind": kind,
                    "content": content,
                    "with_tools": with_tools,
                    "has_tool_calls": response.has_tool_calls,
                    "should_execute_tools": response.should_execute_tools,
                    "clean": clean,
                    "retry_check_reached": reached,
                    "retry_eligible": (
                        reached and reason not in excluded and is_blank_text(clean)
                    ),
                })
    return out


class _EmptyStr(Exception):
    """An exception whose str() is the empty string."""

    def __str__(self) -> str:  # pragma: no cover - trivial
        return ""


class _WsStr(Exception):
    """An exception whose str() is Python whitespace only."""

    def __init__(self, s: str) -> None:
        super().__init__(s)
        self._s = s

    def __str__(self) -> str:  # pragma: no cover - trivial
        return self._s


def dump_error_detail() -> list[dict]:
    """_error_response_from_exception's detail formatting (base.py:994).

    The point of the blank-str cases is the `or type(exc).__name__` fallback:
    the reference never emits an empty detail, it emits the CLASS NAME.
    """
    cases: list[tuple[str, BaseException]] = [
        ("plain", Exception("connection refused")),
        ("ws_only", Exception("   ")),
        ("nbsp_only", Exception("\u00a0")),
        ("u001c_only", Exception("\x1c")),
        ("empty", Exception("")),
        ("valueerror_ws", ValueError("  ")),
        ("runtimeerror_empty", RuntimeError("")),
        ("custom_empty_str", _EmptyStr()),
        ("custom_ws_str", _WsStr("\t\n")),
        ("custom_nbsp_str", _WsStr("\u00a0")),
        ("padded", Exception("  padded message  ")),
    ]
    out = []
    for label, exc in cases:
        response = LLMProvider._error_response_from_exception(exc)
        out.append({
            "label": label,
            "class_name": type(exc).__name__,
            "str_value": str(exc),
            "str_stripped": str(exc).strip(),
            "detail": response.content,
            "finish_reason": response.finish_reason,
        })
    return out


def main() -> dict:
    return {
        "upstream_commit": UPSTREAM_COMMIT,
        "strip_think": dump_strip_think(),
        "is_blank_text": dump_is_blank_text(),
        "restore_outer_whitespace": dump_restore_outer_whitespace(),
        "empty_retry_truth_table": dump_empty_retry_truth_table(),
        "error_detail": dump_error_detail(),
    }


if __name__ == "__main__":
    print(json.dumps(main(), ensure_ascii=False, sort_keys=True, indent=None))
