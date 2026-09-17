#!/usr/bin/env python3
"""Dump terminal-response ground truth from the frozen nanobot reference.

This is one half of the terminal-response differential harness. It EXECUTES the
real `AgentRunner.run` (upstream/nanobot @ 1bb712d3) against a stub provider and
emits a single JSON document on stdout; compat/runner_terminal_differential_test.go
drives the Go port through the same scripts and compares field by field.

Nothing here is transcribed from reading the source. The subject is the terminal
section of `AgentRunner._run_core`, runner.py:675-789, and in particular the
blank-content branch that the port was missing:

    if is_blank_text(clean):
        final_content = EMPTY_FINAL_RESPONSE_MESSAGE
        stop_reason = "empty_final_response"
        error = final_content
        self._append_final_message(messages, final_content)
        ...                                                       # runner.py:737-754

Three things are dumped:

  * `cases` — full runs. Each case is a script of model responses; the real
    runner consumes them through `spec.runtime.provider.chat_stream_with_retry`
    and the dump records final_content, stop_reason, error, the PERSISTED
    transcript (`messages`), the number of provider calls and every request
    payload the runner built. The transcript is the interesting half: the blank
    branch writes EMPTY_FINAL_RESPONSE_MESSAGE through _append_final_message
    rather than persisting the blank response itself, so a port that keeps the
    old "append the raw assistant message first" order produces a different
    transcript even when its returned content happens to match.

  * `append_final_message` — AgentRunner._append_final_message (runner.py:1356)
    called directly over a corpus of transcripts, because its two non-trivial
    branches (replace the trailing assistant turn, or leave it alone) are not
    all reachable through a run.

  * `append_model_error_placeholder` — AgentRunner._append_model_error_placeholder
    (runner.py:1371), the error path's counterpart.

The provider is a stub, so this is the real control flow with a scripted model —
not a paraphrase of the branch. `AgentProgressHook` is installed explicitly
because that is the hook `build_agent_turn_hook` puts first in every turn
(turn_hooks.py:45) and its `finalize_content` is `strip_think(content) or None`
(progress_hook.py:46-49, :180-181), which is what makes `clean` the cleaned text
rather than the raw response content.

No temporary files are created, so this dumper cannot collide with any other
harness.

Run with the project venv:
    .tools/venv/bin/python compat/python/dump_runner_terminal.py
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

from nanobot.agent.progress_hook import AgentProgressHook  # noqa: E402
from nanobot.agent.runner import AgentRunner, AgentRunSpec  # noqa: E402
from nanobot.agent.tools.base import Tool  # noqa: E402
from nanobot.agent.tools.registry import ToolRegistry  # noqa: E402
from nanobot.providers.base import (  # noqa: E402
    GenerationSettings,
    LLMResponse,
    LLMUsage,
    ToolCallRequest,
)
from nanobot.utils.llm_runtime import LLMRuntime  # noqa: E402
from nanobot.utils.runtime import EMPTY_FINAL_RESPONSE_MESSAGE  # noqa: E402

UPSTREAM_COMMIT = "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9"

# _PERSISTED_MODEL_ERROR_PLACEHOLDER (runner.py:68) and _DEFAULT_ERROR_MESSAGE
# (runner.py:65), read off the module rather than retyped.
from nanobot.agent import runner as _runner_module  # noqa: E402

PERSISTED_MODEL_ERROR_PLACEHOLDER = _runner_module._PERSISTED_MODEL_ERROR_PLACEHOLDER
DEFAULT_ERROR_MESSAGE = _runner_module._DEFAULT_ERROR_MESSAGE


# ---------------------------------------------------------------------------
# Stub provider
# ---------------------------------------------------------------------------


class StubProvider:
    """A scripted provider.

    The runner only needs `chat_stream_with_retry`, so this is duck-typed
    rather than an LLMProvider subclass: implementing the abstract base would
    add behaviour the harness does not exercise.
    """

    def __init__(self, script: list[dict[str, Any]]) -> None:
        self.script = list(script)
        self.requests: list[dict[str, Any]] = []
        self.name = "stub"
        self.model = "stub-model"
        self.generation = GenerationSettings()

    async def chat_stream_with_retry(self, **kwargs: Any) -> LLMResponse:
        messages = [dict(m) for m in (kwargs.get("messages") or [])]
        self.requests.append(
            {
                "messages": messages,
                "has_tools": bool(kwargs.get("tools")),
            }
        )
        item = self.script.pop(0) if self.script else {"content": "script-exhausted"}
        tool_calls = [
            ToolCallRequest(
                id=raw["id"],
                name=raw["function"]["name"],
                arguments=raw["function"]["arguments"],
            )
            for raw in (item.get("tool_calls") or [])
        ]
        return LLMResponse(
            content=item.get("content", ""),
            finish_reason=item.get("finish_reason", "stop"),
            tool_calls=tool_calls,
            reasoning_content=item.get("reasoning_content"),
            thinking_blocks=item.get("thinking_blocks"),
            # A non-zero reported usage keeps _usage_or_estimate on the
            # reported path, so the local token estimator never runs and
            # cannot influence the branch under test.
            usage=LLMUsage.reported(input_tokens=10, output_tokens=1),
        )


class EchoTool(Tool):
    """Minimal registered tool, so a run can produce a real tool result."""

    @property
    def name(self) -> str:
        return "echo"

    @property
    def description(self) -> str:
        return "Echo the value back."

    @property
    def parameters(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {"v": {"type": "string"}},
            "required": ["v"],
        }

    @property
    def read_only(self) -> bool:
        return True

    async def execute(self, **kwargs: Any) -> Any:
        return f"echo:{kwargs.get('v', '')}"


def tool_call(call_id: str = "call_1", arguments: str = '{"v":"x"}') -> dict[str, Any]:
    """One OpenAI-style tool call, as a provider would report it."""
    return {
        "id": call_id,
        "type": "function",
        "function": {"name": "echo", "arguments": arguments},
    }


# ---------------------------------------------------------------------------
# Response scripts
# ---------------------------------------------------------------------------


def blank(content: str, finish_reason: str = "stop", **extra: Any) -> dict[str, Any]:
    return {"content": content, "finish_reason": finish_reason, **extra}


def answer(content: str, finish_reason: str = "stop", **extra: Any) -> dict[str, Any]:
    return {"content": content, "finish_reason": finish_reason, **extra}


def calls(*items: dict[str, Any], **extra: Any) -> dict[str, Any]:
    return {"content": "", "finish_reason": "tool_calls", "tool_calls": list(items), **extra}


def blank_terminal(content: str, **extra: Any) -> list[dict[str, Any]]:
    """Three identical blank responses.

    A blank response with a retry-eligible finish reason is not terminal on the
    first call: the runner retries it once (runner.py:595-606) and then issues a
    finalization retry (runner.py:615-620), so the blank branch is only reached
    on the THIRD call. Scripting three entries is what makes these cases land on
    the branch under test rather than on a recovered response; the Go side
    asserts the same call count, so a port that stopped retrying would fail
    loudly instead of silently comparing different things.
    """
    return [blank(content, "stop", **extra) for _ in range(3)]


# Each case is (label, script, use_tools, max_iterations).
#
# The blankness flavours are the point: "" and Python-only whitespace (U+001C
# ..U+001F) are blank to `is_blank_text` but NOT to Go's unicode.IsSpace, NBSP is
# blank to both (control), and U+200B is blank to NEITHER (control). The
# thinking-only rows are blank only after `strip_think`.
CASES: list[tuple[str, list[dict[str, Any]], bool, int]] = [
    # --- the missing terminal blank branch -------------------------------
    ("stop_empty", blank_terminal(""), False, 6),
    ("stop_space", blank_terminal(" "), False, 6),
    ("stop_newline", blank_terminal("\n"), False, 6),
    ("stop_tabs", blank_terminal(" \t\r\n\v\f "), False, 6),
    ("stop_u001c", blank_terminal("\x1c"), False, 6),
    ("stop_u001c_u001f", blank_terminal("\x1c\x1d\x1e\x1f"), False, 6),
    ("stop_nbsp_control", blank_terminal("\u00a0"), False, 6),
    ("stop_ideographic_space", blank_terminal("\u3000"), False, 6),
    ("stop_think_only", blank_terminal("<think>hidden</think>"), False, 6),
    ("stop_think_only_padded", blank_terminal("  <think>hidden</think>  "), False, 6),
    ("stop_think_unclosed", blank_terminal("<think>never closed"), False, 6),
    ("stop_empty_with_reasoning", blank_terminal("", reasoning_content="thought"), False, 6),
    ("stop_zwsp_not_blank", [blank("\u200b")], False, 6),
    ("stop_bom_not_blank", [blank("\ufeff")], False, 6),
    # Non-tool-capable finish reasons reach the same terminal section without
    # the empty-content retry, so they land on the blank branch immediately.
    ("refusal_empty", [blank("", "refusal")], False, 6),
    ("content_filter_empty", [blank("", "content_filter")], False, 6),
    ("refusal_empty_with_tools", [blank("", "refusal", tool_calls=[tool_call()])], True, 6),
    ("content_filter_think_only", [blank("<think>x</think>", "content_filter")], False, 6),
    # --- the error branch must win over the blank branch -----------------
    ("error_empty", [blank("", "error")], False, 6),
    ("error_u001c", [blank("\x1c", "error")], False, 6),
    ("error_with_text", [blank("boom", "error")], False, 6),
    # --- the length branch must win over the blank branch ----------------
    ("length_empty_then_answer", [blank("", "length"), answer("recovered")], False, 6),
    ("length_padded_then_answer", [blank("  seg  ", "length"), answer("rest")], False, 6),
    ("length_exhausted_then_blank", [
        blank("a", "length"), blank("b", "length"), blank("c", "length"), blank("", "length"),
    ], False, 6),
    ("length_then_blank_terminal", [
        blank("seg", "length"), blank(""), blank(""), blank(""),
    ], False, 6),
    # --- blank terminal after real tool work -----------------------------
    ("tool_then_blank", [
        calls(tool_call()), blank(""), blank(""), blank(""),
    ], True, 6),
    ("tool_then_answer", [calls(tool_call()), answer("after tool")], True, 6),
    ("tool_clean_content", [
        calls(tool_call(), content="  <think>x</think>Let me check.  "), answer("done"),
    ], True, 6),
    # --- retry recovery, then a successful terminal answer ---------------
    ("blank_then_answer", [blank(""), answer("recovered")], False, 6),
    ("blank_blank_then_answer", [blank(""), blank(""), answer("third time")], False, 6),
    ("blank_forever", blank_terminal("") + [blank("")], False, 6),
    # A response whose ONLY tool call is degenerate is not tool work: the call
    # is dropped inside _request_model, which forces finish_reason to "stop", so
    # the blank response is retried as an empty one. Both sides spend their
    # second request here, but for different reasons — the reference's is
    # _malformed_tool_call_retry_messages (runner.py:1039-1058), which this port
    # does not model, and the port's is the empty-content retry. Only the
    # result, the transcript and the call count are comparable.
    ("degenerate_call_then_answer", [
        {"content": "", "finish_reason": "tool_calls", "tool_calls": [
            {"id": "bad", "type": "function", "function": {"name": "", "arguments": "{}"}},
        ]},
        answer("after degenerate"),
    ], True, 6),
    # One valid call and one degenerate one: the degenerate call is dropped
    # (no malformed retry is triggered, because not ALL calls were dropped), the
    # valid one runs, and only the valid one is persisted.
    ("partial_malformed_calls", [
        {"content": "", "finish_reason": "tool_calls", "tool_calls": [
            {"id": "good", "type": "function", "function": {"name": "echo", "arguments": '{"v":"y"}'}},
            {"id": "bad", "type": "function", "function": {"name": "", "arguments": "{}"}},
        ]},
        answer("done"),
    ], True, 6),
    # --- controls: non-blank terminal responses, raw != clean ------------
    ("stop_padded", [answer("  padded answer  ")], False, 6),
    ("stop_think_then_text", [answer("<think>x</think>The answer.")], False, 6),
    ("stop_leading_u001c", [answer("\x1cThe answer.")], False, 6),
    ("stop_reasoning_content", [answer("answer", reasoning_content="thought")], False, 6),
    ("stop_multi_blank_after_text", [answer("real answer"), blank("")], False, 6),
    # --- the max-iterations terminal, the other _append_final_message caller
    ("max_iterations_salvaged", [
        calls(tool_call()), calls(tool_call()), answer("salvaged answer"),
    ], True, 2),
    ("max_iterations_fallback", [
        calls(tool_call()), calls(tool_call()), blank(""),
    ], True, 2),
    ("max_iterations_tool_calls_ignored", [
        calls(tool_call()), calls(tool_call()), calls(tool_call()),
    ], True, 2),
]


# Cases whose REQUEST SEQUENCE the port cannot match, because the reference
# spends a request on a feature this port does not model. Dumped as evidence,
# not asserted: (label, script, use_tools, max_iterations, why).
UNPORTED_CASES: list[tuple[str, list[dict[str, Any]], bool, int, str]] = [
    (
        "unported_degenerate_call_then_blank",
        [
            {"content": "", "finish_reason": "tool_calls", "tool_calls": [
                {"id": "bad", "type": "function", "function": {"name": "", "arguments": "{}"}},
            ]},
            blank(""), blank(""), blank(""),
        ],
        True,
        6,
        "the reference retries a fully-malformed tool response inside _request_model "
        "(_malformed_tool_call_retry_messages, runner.py:1039-1058), so it spends one "
        "more request than a port without that feature; the terminal decision itself "
        "is the same blank branch",
    ),
]


async def run_case(
    label: str,
    script: list[dict[str, Any]],
    use_tools: bool,
    max_iterations: int,
) -> dict[str, Any]:
    provider = StubProvider(script)
    registry = ToolRegistry()
    if use_tools:
        registry.register(EchoTool())
    runtime = LLMRuntime(
        provider=provider,
        model="stub-model",
        generation=GenerationSettings(),
        context_window_tokens=0,
    )
    spec = AgentRunSpec(
        initial_messages=[{"role": "user", "content": "hi"}],
        tools=registry,
        runtime=runtime,
        max_iterations=max_iterations,
        max_tool_result_chars=16000,
        hook=AgentProgressHook(),
    )
    result = await AgentRunner().run(spec)
    return {
        "label": label,
        "script": script,
        "use_tools": use_tools,
        "max_iterations": max_iterations,
        "final_content": result.final_content,
        "stop_reason": result.stop_reason,
        "error": result.error,
        "messages": result.messages,
        "tools_used": result.tools_used,
        "had_injections": result.had_injections,
        "provider_calls": len(provider.requests),
        "requests": provider.requests,
    }


# ---------------------------------------------------------------------------
# _append_final_message / _append_model_error_placeholder corpora
# ---------------------------------------------------------------------------

# (label, transcript, content)
APPEND_FINAL_CORPUS: list[tuple[str, list[dict[str, Any]], str | None]] = [
    ("empty_transcript", [], "hello"),
    ("user_then_content", [{"role": "user", "content": "hi"}], "hello"),
    ("tool_tail", [
        {"role": "user", "content": "hi"},
        {"role": "assistant", "content": "", "tool_calls": [tool_call()]},
        {"role": "tool", "tool_call_id": "call_1", "name": "echo", "content": "echo:x"},
    ], "hello"),
    ("assistant_same_content", [
        {"role": "user", "content": "hi"},
        {"role": "assistant", "content": "hello"},
    ], "hello"),
    ("assistant_different_content", [
        {"role": "user", "content": "hi"},
        {"role": "assistant", "content": "other"},
    ], "hello"),
    ("assistant_empty_content", [
        {"role": "user", "content": "hi"},
        {"role": "assistant", "content": ""},
    ], "hello"),
    ("assistant_with_tool_calls", [
        {"role": "user", "content": "hi"},
        {"role": "assistant", "content": "", "tool_calls": [tool_call()]},
    ], "hello"),
    ("none_content", [{"role": "user", "content": "hi"}], None),
    ("empty_content", [{"role": "user", "content": "hi"}], ""),
    ("u001c_content", [{"role": "user", "content": "hi"}], "\x1c"),
    ("unicode_content", [{"role": "user", "content": "hi"}], "olá \u2014 ok"),
]

# (label, transcript)
APPEND_ERROR_CORPUS: list[tuple[str, list[dict[str, Any]]]] = [
    ("empty_transcript", []),
    ("user_tail", [{"role": "user", "content": "hi"}]),
    ("assistant_text_tail", [
        {"role": "user", "content": "hi"},
        {"role": "assistant", "content": "an answer"},
    ]),
    ("assistant_tool_calls_tail", [
        {"role": "user", "content": "hi"},
        {"role": "assistant", "content": "", "tool_calls": [tool_call()]},
    ]),
    ("tool_tail", [
        {"role": "user", "content": "hi"},
        {"role": "assistant", "content": "", "tool_calls": [tool_call()]},
        {"role": "tool", "tool_call_id": "call_1", "name": "echo", "content": "echo:x"},
    ]),
    ("placeholder_already_present", [
        {"role": "user", "content": "hi"},
        {"role": "assistant", "content": PERSISTED_MODEL_ERROR_PLACEHOLDER},
    ]),
]


def main() -> None:
    cases = [asyncio.run(run_case(*case)) for case in CASES]

    # Cases the port cannot be compared against, dumped as EVIDENCE rather than
    # as assertions. Each one exercises a reference feature this port does not
    # model, so its request count legitimately differs; recording the reference
    # answer here keeps the divergence documented and reproducible instead of
    # silently absent.
    unported = []
    for label, script, use_tools, max_iterations, reason in UNPORTED_CASES:
        case = asyncio.run(run_case(label, script, use_tools, max_iterations))
        case["unported_reason"] = reason
        unported.append(case)

    append_final = []
    for label, transcript, content in APPEND_FINAL_CORPUS:
        messages = [dict(m) for m in transcript]
        AgentRunner._append_final_message(messages, content)
        append_final.append(
            {"label": label, "messages_in": transcript, "content": content, "messages_out": messages}
        )

    append_error = []
    for label, transcript in APPEND_ERROR_CORPUS:
        messages = [dict(m) for m in transcript]
        AgentRunner._append_model_error_placeholder(messages)
        append_error.append({"label": label, "messages_in": transcript, "messages_out": messages})

    document = {
        "upstream_commit": UPSTREAM_COMMIT,
        "empty_final_response_message": EMPTY_FINAL_RESPONSE_MESSAGE,
        "persisted_model_error_placeholder": PERSISTED_MODEL_ERROR_PLACEHOLDER,
        "default_error_message": DEFAULT_ERROR_MESSAGE,
        "cases": cases,
        "unported_cases": unported,
        "append_final_message": append_final,
        "append_model_error_placeholder": append_error,
    }
    json.dump(document, sys.stdout, ensure_ascii=False, indent=1, sort_keys=True)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
