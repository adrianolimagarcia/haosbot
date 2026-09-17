#!/usr/bin/env python3
"""Dump reasoning-extraction ground truth from the frozen nanobot reference.

This is the Python half of the reasoning differential harness. It executes the
REAL Python implementation (upstream/nanobot @ 1bb712d3) and emits a single JSON
document on stdout; compat/reasoning_differential_test.go drives the Go port
through the same inputs and compares field by field.

Subject (nanobot/utils/helpers.py):

  * strip_reasoning_tags (helpers.py:226) — wrapper-tag removal for text that is
    ALREADY known to be reasoning. Unlike strip_think it never removes a block
    in the middle of the text, and its trailing partial-tag rule is aggressive:
    `\\s*(?:</?(?:thinking|...|th|t)>?)$` eats any trailing "t"/"th"/"tho"...
  * extract_think (helpers.py:240) — inline <think>/<thinking>/<thought> block
    extraction. Only CLOSED blocks are surfaced; unclosed prefixes are stripped
    from the cleaned text but not returned.
  * extract_reasoning (helpers.py:292) — the three-tier fallback
    (reasoning_content, thinking_blocks, inline content tags) that runner.py:467
    calls on EVERY response.

Nothing is transcribed from reading the source by hand: every expected value is
produced by running the reference.

The `runner_emission` section goes further and drives the REAL AgentRunner with a
scripted provider and a recording hook, so the emit gate of runner.py:477-480
  (reasoning_text and not context.streamed_reasoning -> emit_reasoning,
   emit_reasoning_end, streamed_reasoning = True)
is observed as the reference's own behaviour rather than a restatement of it.

Run with the project venv:
    .tools/venv/bin/python compat/python/dump_reasoning.py
"""
from __future__ import annotations

import asyncio
import json
import sys
from pathlib import Path
from typing import Any

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
sys.path.insert(0, str(UPSTREAM / "tests"))

from nanobot.utils.helpers import (  # noqa: E402
    extract_reasoning,
    extract_think,
    strip_reasoning_tags,
)

UPSTREAM_COMMIT = "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9"

# ---------------------------------------------------------------------------
# Corpora
# ---------------------------------------------------------------------------
#
# The whitespace dimension is deliberate. Python's `\s` in a str pattern is
# exactly str.isspace(), which includes U+001C..U+001F (FILE/GROUP/RECORD/UNIT
# SEPARATOR), NBSP, U+3000 and U+2028 — but NOT U+200B (ZERO WIDTH SPACE) or
# U+FEFF (BOM). Go's regexp `\s` is the ASCII set `[\t\n\f\r ]` and matches
# NONE of those, so a port that keeps `\s` diverges on every one of these
# entries. They are in the corpus precisely so such a port fails loudly.
STRIP_REASONING_TAGS_CORPUS: list[tuple[str, str]] = [
    ("empty", ""),
    ("space", " "),
    ("ascii_ws", " \t\n\r\v\f "),
    ("vt_only", "\v"),
    ("u001c", "\x1c"),
    ("u001c_u001f", "\x1c\x1d\x1e\x1f"),
    ("nbsp", "\u00a0"),
    ("u3000", "\u3000"),
    ("u2028", "\u2028"),
    ("nel", "\x85"),
    ("u1680", "\u1680"),
    ("u200b_not_space", "\u200b"),
    ("feff_not_space", "\ufeff"),
    ("plain", "hello world"),
    ("plain_ws", "  hello world  "),
    # Opening wrapper at the START and closing wrapper at the END only.
    ("open_close", "<think>reasoning</think>"),
    ("open_close_thinking", "<thinking>r</thinking>"),
    ("open_close_thought", "<thought>r</thought>"),
    ("open_close_ws", "  <think>r</think>  "),
    ("open_close_inner_ws", "  <think>  spaced  </think>  "),
    ("open_only", "<think>reasoning"),
    ("close_only", "reasoning</think>"),
    # Mid-text tags survive: strip_reasoning_tags has no mid-text rule.
    ("mid_text_block", "before<think>mid</think>after"),
    ("leading_orphan_close", "</think>answer"),
    ("trailing_orphan_close", "answer</think>"),
    ("trailing_orphan_close_thinking", "answer</thinking>"),
    ("trailing_orphan_close_thought", "answer</thought>"),
    # Self-closing markers are edge-only here too.
    ("self_closing_only", "<thinking/>"),
    ("self_closing_ws", "  <thinking/>  "),
    ("self_closing_lead", "<thinking/>answer"),
    ("self_closing_trail", "answer<thinking/>"),
    ("self_closing_think", "<think/>"),
    ("self_closing_thought", "<thought/>"),
    # Trailing partial control tags, including the single-character prefixes.
    ("partial_thi", "answer<thi"),
    ("partial_th", "answer<th"),
    ("partial_t", "answer<t"),
    ("partial_thinking", "answer<thinking"),
    ("partial_lt", "answer<"),
    ("partial_lt_slash", "answer</"),
    ("partial_gt", "answer>"),
    ("partial_bare_t", "answert"),
    ("partial_bare_th", "answerth"),
    ("partial_bare_tho", "answertho"),
    ("partial_bare_t_space", "answer t"),
    ("partial_thin_space", "answer thin"),
    ("partial_slash_th", "answer</th"),
    ("partial_slash_t", "answer</t"),
    # Malformed opening tags: strip_reasoning_tags has NO malformed-tag rule
    # (that lives in strip_think), so these survive here.
    ("malformed_cjk", "<think\u5e7f\u573aanswer"),
    ("malformed_then_text", "<thinkx answer"),
    # Whitespace-sensitive boundaries.
    ("nbsp_boundary", "\u00a0<think>r</think>\u00a0"),
    ("u001c_boundary", "\x1c<think>r</think>\x1f"),
    ("u3000_boundary", "\u3000<thinking/>\u3000"),
    ("u2028_boundary", "\u2028<think>r</think>\u2029"),
    ("vt_boundary", "\v<think>r</think>\v"),
    ("u200b_boundary", "\u200b<think>r</think>"),
    ("nbsp_partial", "answer\u00a0<thi"),
    ("u001c_partial", "answer\x1c<th"),
    ("u3000_bare_t", "answer\u3000t"),
    ("nbsp_close", "answer\u00a0</think>\u00a0"),
    ("empty_block", "<think></think>"),
    ("two_blocks", "<think>a</think><think>b</think>"),
    ("newline_block", "<think>\n\n</think>"),
    # The partial harmony-channel alternative of _PARTIAL_THINKING_TAG's
    # sibling branch. A port that only carries the thinking-tag prefixes keeps
    # these fragments.
    ("channel_partial_c", "answer<|c"),
    ("channel_partial_chan", "answer<|chan"),
    ("channel_partial_full", "answer<|channel"),
    ("channel_partial_gt", "answer<|channel>"),
    ("channel_partial_bar_gt", "answer<|channel|>"),
    ("channel_partial_bare_lt_bar", "answer<|"),
    ("channel_partial_ws", "answer <|chan"),
    # Python's `$` also matches immediately before a TRAILING newline; Go's
    # does not. Every pattern below ends at such a boundary.
    ("trailing_nl_plain", "answer\n"),
    ("trailing_nl_partial_t", "answer t\n"),
    ("trailing_nl_partial_thi", "answer<thi\n"),
    ("trailing_nl_partial_chan", "answer<|chan\n"),
    ("trailing_nl_close", "answer</think>\n"),
    ("trailing_nl_open_close", "<think>r</think>\n"),
    ("trailing_nl_self_closing", "answer<thinking/>\n"),
    ("lone_pipe_nl", "<|\n"),
    ("nbsp_lead_unclosed", "\u00a0<think>never closed"),
    ("u3000_lead_unclosed", "\u3000<think>x"),
    ("u001c_lead_unclosed", "\x1c<think>y"),
]

EXTRACT_THINK_CORPUS: list[tuple[str, str]] = [
    ("empty", ""),
    ("space", "   "),
    ("plain", "no tags here"),
    ("plain_ws", "  no tags here  "),
    ("plain_nbsp", "\u00a0no tags\u00a0"),
    ("plain_u001c", "\x1cno tags\x1f"),
    ("plain_u3000", "\u3000no tags\u3000"),
    ("plain_vt", "\vno tags\v"),
    ("plain_u200b", "\u200bno tags\u200b"),
    # Well-formed blocks.
    ("think_then_answer", "<think>r</think>answer"),
    ("think_only", "<think>r</think>"),
    ("thinking_then_answer", "<thinking>r</thinking>answer"),
    ("thought_then_answer", "<thought>r</thought>answer"),
    ("answer_around", "x<think>r</think>y"),
    ("inner_ws", "<think>  spaced  </think>answer"),
    ("multiline", "<think>line1\nline2</think>\nfinal"),
    ("two_blocks", "<think>a</think><think>b</think>"),
    ("two_blocks_text_between", "<think>a</think>one<think>b</think>two"),
    ("mixed_tag_names", "<think>a</think>x<thought>b</thought>y"),
    # An empty block is a NON-None empty string: `"\n\n".join([""])` is "" and
    # the `if parts` test is about the LIST, not about its contents.
    ("empty_block", "<think></think>answer"),
    ("empty_block_only", "<think></think>"),
    ("empty_block_ws_inner", "<think>   </think>answer"),
    ("empty_and_full", "<think></think><think>b</think>"),
    # Unclosed prefixes are stripped from cleaned but NOT surfaced.
    ("unclosed", "<think>never closed"),
    ("unclosed_ws", "  <think>never closed"),
    ("unclosed_then_text", "a<think>b"),
    ("orphan_close_start", "</think>answer"),
    ("orphan_close_end", "answer</think>"),
    ("orphan_close_middle", "ans</think>wer"),
    # Nested: the non-greedy body stops at the FIRST closing tag.
    ("nested", "<think>outer<think>inner</think>tail</think>answer"),
    # Malformed opening tag (no `>`): strip_think removes `<think` when the
    # next char cannot continue a tag name, so it does not leak.
    ("malformed_cjk", "<think\u5e7f\u573ax</think>y"),
    ("malformed_then_text", "<thinkx answer"),
    ("self_closing", "<thinking/>answer"),
    ("channel_marker", "<|channel|>answer"),
    ("partial_trailing", "answer<thi"),
    # Whitespace-sensitive: `if "<" not in text` fast path uses .strip(), and
    # strip_think's own boundaries use Python's `\s`.
    ("nbsp_around", "\u00a0<think>r</think>\u00a0x"),
    ("u001c_around", "\x1c<think>r</think>\x1cx"),
    ("u3000_around", "\u3000<think>r</think>\u3000x"),
    ("nbsp_inside_block", "<think>\u00a0r\u00a0</think>answer"),
    ("u001c_inside_block", "<think>\x1cr\x1c</think>answer"),
    ("u3000_inside_block", "<think>\u3000r\u3000</think>answer"),
    ("vt_inside_block", "<think>\vr\v</think>answer"),
    ("nbsp_unclosed", "\u00a0<think>never closed"),
    ("u3000_only", "\u3000"),
    ("u2028_only", "\u2028"),
    # strip_think's `^\s*<TAG>[\s\S]*$` unclosed-block rule is anchored on
    # Python's `\s`, which matches these leading spaces and Go's `\s` does not.
    # When the rule fails, the `<think>` literal leaks into the cleaned text.
    ("unclosed_nbsp_lead", "\u00a0<think>never closed"),
    ("unclosed_u3000_lead", "\u3000<think>x"),
    ("unclosed_u001c_lead", "\x1c<think>y"),
    ("unclosed_u2028_lead", "\u2028<think>z"),
    ("unclosed_vt_lead", "\v<think>z"),
    ("unclosed_nbsp_lead_trailing_nl", "\u00a0<think>never closed\n"),
    # Python's `$` matches before a trailing newline; Go's does not.
    ("trailing_nl_plain", "answer\n"),
    ("trailing_nl_partial", "answer<thi\n"),
    ("trailing_nl_think", "<think>r</think>\n"),
    ("trailing_nl_close", "answer</think>\n"),
    ("channel_partial_tail", "answer<|chan"),
    ("channel_partial_c_tail", "answer<|c"),
    ("malformed_cjk_unclosed", "<think\u5e7f\u573a"),
]

# reasoning_content values. "" and "   " are FALSE/TRUE respectively under
# Python truthiness, so "" falls through to the next tier while "   " selects
# the first tier and then strips to "".
REASONING_CONTENTS: list[tuple[str, str | None]] = [
    ("none", None),
    ("empty", ""),
    ("ws_only", "   "),
    ("nbsp_only", "\u00a0"),
    ("u001c_only", "\x1c"),
    ("plain", "rc"),
    ("tagged", "<thinking>rc</thinking>"),
    ("tagged_open_only", "<thinking>rc"),
    ("self_closing", "<thinking/>"),
    ("nbsp_boundary", "\u00a0rc\u00a0"),
    ("multiline", "line1\nline2"),
    ("partial", "<thinking"),
]

# thinking_blocks values. Only entries whose `type` is exactly "thinking"
# contribute, and a non-str `thinking` value strips to "" via the isinstance
# guard in strip_reasoning_tags.
THINKING_BLOCKS: list[tuple[str, list[dict[str, Any]] | None]] = [
    ("none", None),
    ("empty_list", []),
    ("one", [{"type": "thinking", "thinking": "a"}]),
    ("two", [
        {"type": "thinking", "thinking": "a"},
        {"type": "thinking", "thinking": "b"},
    ]),
    ("wrong_type", [{"type": "text", "thinking": "a"}]),
    ("missing_type", [{"thinking": "a"}]),
    ("missing_thinking", [{"type": "thinking"}]),
    ("empty_thinking", [{"type": "thinking", "thinking": ""}]),
    ("ws_thinking", [{"type": "thinking", "thinking": "   "}]),
    ("mixed", [
        {"type": "thinking", "thinking": "a"},
        {"type": "text", "thinking": "b"},
    ]),
    ("mixed_with_empty", [
        {"type": "thinking", "thinking": ""},
        {"type": "thinking", "thinking": "a"},
    ]),
    ("tagged", [{"type": "thinking", "thinking": "  <think>x</think>  "}]),
    ("tagged_partial", [{"type": "thinking", "thinking": "x<thi"}]),
    ("nbsp_boundary", [{"type": "thinking", "thinking": "\u00a0a\u00a0"}]),
    ("u001c_boundary", [{"type": "thinking", "thinking": "\x1ca\x1f"}]),
    ("non_str_int", [{"type": "thinking", "thinking": 123}]),
    ("non_str_none", [{"type": "thinking", "thinking": None}]),
    ("non_str_list", [{"type": "thinking", "thinking": ["a"]}]),
    ("signature_only", [
        {"type": "thinking", "thinking": "a", "signature": "sig"},
        {"type": "thinking", "thinking": "b", "signature": "sig2"},
    ]),
]

# content values.
CONTENTS: list[tuple[str, str | None]] = [
    ("none", None),
    ("empty", ""),
    ("plain", "answer"),
    ("plain_ws", "  answer  "),
    ("inline_think", "<think>x</think>answer"),
    ("inline_think_only", "<think>x</think>"),
    ("inline_empty_block", "<think></think>answer"),
    ("inline_unclosed", "<think>unclosed"),
    ("inline_thinking", "<thinking>t</thinking>a"),
    ("nbsp_boundary", "\u00a0answer\u00a0"),
    ("u001c_boundary", "\x1canswer\x1f"),
    ("u3000_boundary", "\u3000answer\u3000"),
    ("vt_boundary", "\vanswer\v"),
]

# ---------------------------------------------------------------------------
# Runner-level scenarios (real AgentRunner)
# ---------------------------------------------------------------------------


class _RecordingHook:
    """Deferred import target: subclassed in _make_hook once AgentHook is in."""


def _make_hook(streaming: bool):
    from nanobot.agent.hook import AgentHook

    class RecordingHook(AgentHook):
        def __init__(self) -> None:
            super().__init__()
            self.emitted: list[str] = []
            self.end_calls = 0

        def wants_streaming(self) -> bool:
            return streaming

        async def emit_reasoning(self, reasoning_content: str | None) -> None:
            if reasoning_content:
                self.emitted.append(reasoning_content)

        async def emit_reasoning_end(self) -> None:
            self.end_calls += 1

    return RecordingHook()


class _ScriptedProvider:
    """Provider whose single response is scripted, with optional deltas."""

    def __init__(
        self,
        response: Any,
        thinking_deltas: tuple[str, ...] = (),
        content_deltas: tuple[str, ...] = (),
    ) -> None:
        self._response = response
        self._thinking = thinking_deltas
        self._content = content_deltas
        self.generation = None

    async def chat_stream_with_retry(self, **kwargs: Any) -> Any:
        for delta in self._thinking:
            cb = kwargs.get("on_thinking_delta")
            if cb is not None:
                await cb(delta)
        for delta in self._content:
            cb = kwargs.get("on_content_delta")
            if cb is not None:
                await cb(delta)
        return self._response


class _Tools:
    def get_definitions(self) -> list[Any]:
        return []

    async def execute(self, *args: Any, **kwargs: Any) -> str:  # pragma: no cover
        return ""


async def _run_scenario(
    *,
    response: Any,
    streaming: bool,
    thinking_deltas: tuple[str, ...] = (),
    content_deltas: tuple[str, ...] = (),
) -> dict[str, Any]:
    from agent.runner_helpers import make_run_spec
    from nanobot.agent.runner import AgentRunner
    from nanobot.config.schema import AgentDefaults

    hook = _make_hook(streaming)
    provider = _ScriptedProvider(response, thinking_deltas, content_deltas)

    # Snapshot the SCRIPTED response BEFORE the run. The runner mutates it —
    # `response.content = cleaned_content` (runner.py:473) — so reading these
    # fields afterwards would record the already-cleaned text, and the Go half
    # would be handed a response with no thinking tags left in it and could
    # never reproduce the extraction under test.
    scripted = {
        "content": response.content,
        "reasoning_content": response.reasoning_content,
        "thinking_blocks": response.thinking_blocks,
    }

    spec = make_run_spec(
        provider,
        initial_messages=[{"role": "user", "content": "question"}],
        tools=_Tools(),
        model="test-model",
        max_iterations=1,
        max_tool_result_chars=AgentDefaults().max_tool_result_chars,
        hook=hook,
    )
    result = await AgentRunner().run(spec)

    assistant = [
        msg.get("content")
        for msg in result.messages
        if msg.get("role") == "assistant"
    ]
    return {
        "response": scripted,
        "emitted": list(hook.emitted),
        "end_calls": hook.end_calls,
        "final_content": result.final_content,
        "assistant_contents": assistant,
        # The stop reason is what tells the Go half whether this scenario
        # reached the normal terminal path or fell through to the
        # max-iterations finalizer. The finalizer calls
        # hook.finalize_content directly and NEVER calls extract_reasoning
        # (runner.py:1207-1215), so a blank response emits nothing there and
        # its final content is unrelated to this harness's subject.
        "stop_reason": result.stop_reason,
    }


def _resp(content: str | None, reasoning_content: str | None = None,
          thinking_blocks: list[dict[str, Any]] | None = None) -> Any:
    from nanobot.providers.base import LLMResponse, LLMUsage

    return LLMResponse(
        content=content,
        tool_calls=[],
        finish_reason="stop",
        usage=LLMUsage.reported(input_tokens=1, output_tokens=1),
        reasoning_content=reasoning_content,
        thinking_blocks=thinking_blocks,
    )


# (label, response, streaming, thinking_deltas, content_deltas)
RUNNER_SCENARIOS: list[tuple[str, Any, bool, tuple[str, ...], tuple[str, ...]]] = [
    ("reasoning_content_plain", _resp("answer", "rc"), False, (), ()),
    ("reasoning_content_tagged", _resp("answer", "<thinking>rc</thinking>"), False, (), ()),
    ("inline_think", _resp("<think>x</think>answer"), False, (), ()),
    ("inline_think_only", _resp("<think>x</think>"), False, (), ()),
    ("thinking_blocks", _resp("answer", None, [
        {"type": "thinking", "thinking": "a"},
        {"type": "thinking", "thinking": "b"},
    ]), False, (), ()),
    ("thinking_blocks_wrong_type", _resp("answer", None, [
        {"type": "text", "thinking": "a"},
    ]), False, (), ()),
    ("thinking_blocks_empty", _resp("answer", None, []), False, (), ()),
    ("priority_reasoning_over_inline", _resp(
        "<think>inline</think>The answer.", "dedicated reasoning field"
    ), False, (), ()),
    ("no_reasoning", _resp("answer"), False, (), ()),
    ("reasoning_content_empty_str", _resp("answer", ""), False, (), ()),
    ("reasoning_content_ws_only", _resp("answer", "   "), False, (), ()),
    ("reasoning_content_nbsp_only", _resp("answer", "\u00a0"), False, (), ()),
    ("content_none", _resp(None), False, (), ()),
    ("content_blank", _resp("   "), False, (), ()),
    # Streaming: reasoning deltas already emitted must not be re-emitted from
    # the final response's thinking_blocks.
    ("streamed_reasoning_no_dup", _resp("done", None, [
        {"type": "thinking", "thinking": "part1part2"},
    ]), True, ("part1", "part2"), ()),
    # Streaming the ANSWER must not suppress a reasoning_content that only
    # arrives on the final response.
    ("streamed_answer_keeps_reasoning", _resp(
        "The answer.", "step-by-step deduction"
    ), True, (), ("The ", "answer.")),
    # Streaming reasoning plus a dedicated reasoning_content on the final
    # response: the gate is already set, so nothing is emitted again.
    ("streamed_reasoning_with_rc", _resp(
        "done", "final rc"
    ), True, ("part1",), ()),
]


def dump_runner_emission() -> list[dict[str, Any]]:
    out = []
    for label, response, streaming, thinking_deltas, content_deltas in RUNNER_SCENARIOS:
        result = asyncio.run(_run_scenario(
            response=response,
            streaming=streaming,
            thinking_deltas=thinking_deltas,
            content_deltas=content_deltas,
        ))
        out.append({
            "label": label,
            "streaming": streaming,
            "thinking_deltas": list(thinking_deltas),
            "content_deltas": list(content_deltas),
            **result,
        })
    return out


# ---------------------------------------------------------------------------
# Pure-function dumps
# ---------------------------------------------------------------------------


def dump_strip_reasoning_tags() -> list[dict[str, Any]]:
    out = []
    for label, text in STRIP_REASONING_TAGS_CORPUS:
        out.append({
            "label": label,
            "in": text,
            "result": strip_reasoning_tags(text),
            # The isinstance guard: a non-str is not an error, it is "".
            "result_non_str_int": strip_reasoning_tags(123),
            "result_non_str_none": strip_reasoning_tags(None),
        })
    return out


def dump_extract_think() -> list[dict[str, Any]]:
    out = []
    for label, text in EXTRACT_THINK_CORPUS:
        thinking, cleaned = extract_think(text)
        out.append({
            "label": label,
            "in": text,
            "thinking": thinking,
            "cleaned": cleaned,
        })
    return out


def dump_extract_reasoning() -> list[dict[str, Any]]:
    out = []
    for rc_label, rc in REASONING_CONTENTS:
        for tb_label, tb in THINKING_BLOCKS:
            for c_label, content in CONTENTS:
                reasoning_text, cleaned = extract_reasoning(rc, tb, content)
                out.append({
                    "label": f"{rc_label}|{tb_label}|{c_label}",
                    "reasoning_content": rc,
                    "thinking_blocks": tb,
                    "content": content,
                    "reasoning_text": reasoning_text,
                    "cleaned_content": cleaned,
                })
    return out


def main() -> dict[str, Any]:
    return {
        "upstream_commit": UPSTREAM_COMMIT,
        "strip_reasoning_tags": dump_strip_reasoning_tags(),
        "extract_think": dump_extract_think(),
        "extract_reasoning": dump_extract_reasoning(),
        "runner_emission": dump_runner_emission(),
    }


if __name__ == "__main__":
    print(json.dumps(main(), ensure_ascii=False, sort_keys=True, indent=None))
