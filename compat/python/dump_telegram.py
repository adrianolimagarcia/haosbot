#!/usr/bin/env python3
"""Dump Telegram-channel ground truth from the frozen nanobot reference.

This is one half of the Telegram differential harness. It emits a single JSON
document on stdout; compat/telegram_differential_test.go drives the Go port of
internal/channels/telegram through the same inputs and compares the results
element by element.

HOW THE REFERENCE VALUES ARE OBTAINED
-------------------------------------
`nanobot.channels.telegram.runtime` cannot be imported: it does `from telegram
import ...` at module scope and python-telegram-bot is not installed in the
reference venv. Two different techniques are therefore used, and every section
below records which one produced it:

1. AST EXTRACTION (the `_extract` helper). The module is parsed with `ast` and
   the *source text* of the wanted top-level functions/constants/classes is cut
   out with `ast.get_source_segment` and exec'd in a prepared namespace. The
   code that runs is byte-identical to the frozen file; only the module-level
   imports it would have needed are supplied by hand (re, unicodedata, pydantic,
   ...). Nothing is retyped from reading the source.

   Used for: the constants, every pure helper in runtime.py, TelegramConfig,
   TelegramChannel.is_allowed and TelegramChannel._normalize_telegram_command.

2. DIRECT IMPORT. `nanobot.channels.telegram.validation` and
   `nanobot.channels.telegram.manifest` import cleanly (they only need httpx),
   so they are imported as real modules and called as real functions.

Every input case is emitted in the dump, so the Go side never re-derives a
fixture: it replays exactly the inputs that produced these outputs.

Run with the project venv:
    .tools/venv/bin/python compat/python/dump_telegram.py
"""
from __future__ import annotations

import ast
import json
import os
import random
import re
import sys
import asyncio
import textwrap
from types import SimpleNamespace
import unicodedata
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[2]
UPSTREAM = ROOT / "upstream" / "nanobot"
sys.path.insert(0, str(UPSTREAM))

# Silence loguru: it writes diagnostics to stderr and this harness must emit
# exactly one JSON document on stdout.
try:
    from loguru import logger as _loguru_logger

    _loguru_logger.remove()
except Exception:
    pass


# ---------------------------------------------------------------------------
# 1. AST extraction of the pure parts of runtime.py
# ---------------------------------------------------------------------------

RUNTIME_PATH = UPSTREAM / "nanobot" / "channels" / "telegram" / "runtime.py"
RUNTIME_SRC = RUNTIME_PATH.read_text(encoding="utf-8")
RUNTIME_TREE = ast.parse(RUNTIME_SRC)


def _top_level_nodes() -> dict[str, ast.stmt]:
    out: dict[str, ast.stmt] = {}
    for node in RUNTIME_TREE.body:
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
            out[node.name] = node
        elif isinstance(node, ast.Assign) and len(node.targets) == 1:
            target = node.targets[0]
            if isinstance(target, ast.Name):
                out[target.id] = node
    return out


_NODES = _top_level_nodes()


def _segment(name: str) -> str:
    node = _NODES[name]
    src = ast.get_source_segment(RUNTIME_SRC, node)
    if src is None:  # pragma: no cover - only reachable on a mangled checkout
        raise RuntimeError(f"cannot extract {name} from {RUNTIME_PATH}")
    return src


CONSTANT_NAMES = (
    "TELEGRAM_MAX_MESSAGE_LEN",
    "TELEGRAM_HTML_MAX_LEN",
    "TELEGRAM_RICH_MAX_LEN",
    "TELEGRAM_REPLY_CONTEXT_MAX_LEN",
    "TELEGRAM_RICH_DRAFT_MIN_INTERVAL",
    "COMPACTION_NOTICES_MAX",
    "POLL_STALE_SECONDS",
    "POLL_WATCH_INTERVAL",
    "RESTART_BACKOFF_INITIAL_SECONDS",
    "RESTART_BACKOFF_MAX_SECONDS",
    "APP_RESTART_SEND_WAIT_SECONDS",
    "_SEND_MAX_RETRIES",
    "_SEND_RETRY_BASE_DELAY",
    "_STREAM_EDIT_INTERVAL_DEFAULT",
)

PURE_FUNCTION_NAMES = (
    "_split_telegram_markdown",
    "_escape_telegram_html",
    "_tool_hint_to_telegram_blockquote",
    "_strip_md",
    "_strip_md_block",
    "_render_table_box",
    "_markdown_to_telegram_html",
    "_split_telegram_markdown_html_chunks",
    "_split_telegram_markdown_html",
    "_telegram_command_text",
)

# The extraction namespace. `re` and `unicodedata` are the only module-level
# names the extracted functions actually use.
_PURE_NS: dict[str, Any] = {"re": re, "unicodedata": unicodedata}

# Constants first, in SOURCE order, because TELEGRAM_REPLY_CONTEXT_MAX_LEN is
# defined in terms of TELEGRAM_MAX_MESSAGE_LEN.
_CONST_SRC = [_segment(name) for name in CONSTANT_NAMES]
_FUNC_SRC = [_segment(name) for name in PURE_FUNCTION_NAMES]
_ALIAS_SRC = [_segment("_TELEGRAM_COMMAND_ALIASES"), _segment("_TELEGRAM_DISPLAY_COMMAND_RE")]
exec(compile("\n\n".join(_CONST_SRC + _FUNC_SRC + _ALIAS_SRC), str(RUNTIME_PATH), "exec"), _PURE_NS)

_split_telegram_markdown = _PURE_NS["_split_telegram_markdown"]
_escape_telegram_html = _PURE_NS["_escape_telegram_html"]
_tool_hint_to_telegram_blockquote = _PURE_NS["_tool_hint_to_telegram_blockquote"]
_strip_md = _PURE_NS["_strip_md"]
_strip_md_block = _PURE_NS["_strip_md_block"]
_render_table_box = _PURE_NS["_render_table_box"]
_markdown_to_telegram_html = _PURE_NS["_markdown_to_telegram_html"]
_split_telegram_markdown_html_chunks = _PURE_NS["_split_telegram_markdown_html_chunks"]
_split_telegram_markdown_html = _PURE_NS["_split_telegram_markdown_html"]
_telegram_command_text = _PURE_NS["_telegram_command_text"]
_TELEGRAM_COMMAND_ALIASES = _PURE_NS["_TELEGRAM_COMMAND_ALIASES"]
_TELEGRAM_DISPLAY_COMMAND_RE = _PURE_NS["_TELEGRAM_DISPLAY_COMMAND_RE"]

# The whole `_TELEGRAM_COMMAND_ALIASES` / `_TELEGRAM_DISPLAY_COMMAND_RE` block is
# extracted verbatim above; `_normalize_telegram_command` is a staticmethod, so
# it is extracted on its own and re-attached below.

# TelegramConfig: a pydantic model whose only runtime dependency is Base
# (nanobot.config_base, importable) plus pydantic itself.
from nanobot.config_base import Base  # noqa: E402
from pydantic import Field, field_validator, model_validator  # noqa: E402
from typing import Literal  # noqa: E402
from urllib.parse import urlparse  # noqa: E402

_CFG_NS: dict[str, Any] = {
    "Base": Base,
    "Field": Field,
    "field_validator": field_validator,
    "model_validator": model_validator,
    "Literal": Literal,
    "urlparse": urlparse,
    "re": re,
    "Any": Any,
    "_STREAM_EDIT_INTERVAL_DEFAULT": _PURE_NS["_STREAM_EDIT_INTERVAL_DEFAULT"],
}
exec(compile(_segment("TelegramConfig"), str(RUNTIME_PATH), "exec"), _CFG_NS)
TelegramConfig = _CFG_NS["TelegramConfig"]
# runtime.py has `from __future__ import annotations`, and compile()/exec()
# inherit the calling module's future flags, so the extracted class keeps string
# annotations exactly like the reference. pydantic then needs the names it would
# normally find in the runtime module's globals; they are supplied explicitly
# instead of importing a module that cannot be imported.
TelegramConfig.model_rebuild(force=True, _types_namespace=dict(_CFG_NS))

# is_allowed is a real method: it calls `super().is_allowed(sender_id)`, which
# only resolves inside a class body. Re-declaring it in a probe subclass of the
# REAL BaseChannel keeps the zero-argument super() working and keeps the body
# byte-identical.
import nanobot.channels.base as _base_mod  # noqa: E402
from nanobot.channels.base import BaseChannel  # noqa: E402

_IS_ALLOWED_SRC = None
_NORMALIZE_SRC = None
_MENTION_SRC = None
_GROUP_MESSAGE_SRC = None
for _node in RUNTIME_TREE.body:
    if isinstance(_node, ast.ClassDef) and _node.name == "TelegramChannel":
        for _member in _node.body:
            if isinstance(_member, ast.FunctionDef) and _member.name == "is_allowed":
                _IS_ALLOWED_SRC = ast.get_source_segment(RUNTIME_SRC, _member)
            if isinstance(_member, ast.FunctionDef) and _member.name == "_normalize_telegram_command":
                _NORMALIZE_SRC = ast.get_source_segment(RUNTIME_SRC, _member)
            if isinstance(_member, (ast.FunctionDef, ast.AsyncFunctionDef)) and _member.name == "_has_mention_entity":
                # FunctionDef.lineno points at `def`, so the decorators are NOT
                # part of the segment; and a decorator node's own segment covers
                # only the expression, not the "@". Both have to be put back,
                # otherwise the first argument is bound as `self` and the
                # signature silently shifts by one.
                _decorators = "".join(
                    "@" + ast.get_source_segment(RUNTIME_SRC, d) + "\n"
                    for d in _member.decorator_list
                )
                _MENTION_SRC = _decorators + ast.get_source_segment(RUNTIME_SRC, _member)
            if isinstance(_member, (ast.FunctionDef, ast.AsyncFunctionDef)) and _member.name == "_is_group_message_for_bot":
                _GROUP_MESSAGE_SRC = ast.get_source_segment(RUNTIME_SRC, _member)
if _IS_ALLOWED_SRC is None or _NORMALIZE_SRC is None:  # pragma: no cover
    raise RuntimeError("TelegramChannel.is_allowed / _normalize_telegram_command not found")
if _MENTION_SRC is None or _GROUP_MESSAGE_SRC is None:  # pragma: no cover
    raise RuntimeError("TelegramChannel._has_mention_entity / _is_group_message_for_bot not found")

# TELEGRAM_BUS_SLASH_COMMAND_RE is a class attribute, so it is extracted the same
# way and re-executed with a real `re` in scope.
_BUS_SLASH_SRC = None
for _node in RUNTIME_TREE.body:
    if isinstance(_node, ast.ClassDef) and _node.name == "TelegramChannel":
        for _member in _node.body:
            if isinstance(_member, ast.Assign) and any(
                getattr(t, "id", None) == "TELEGRAM_BUS_SLASH_COMMAND_RE"
                for t in _member.targets
            ):
                _BUS_SLASH_SRC = ast.get_source_segment(RUNTIME_SRC, _member)
if _BUS_SLASH_SRC is None:  # pragma: no cover
    raise RuntimeError("TELEGRAM_BUS_SLASH_COMMAND_RE not found")
_BUS_SLASH_NS: dict[str, Any] = {"re": re}
exec(compile(_BUS_SLASH_SRC, str(RUNTIME_PATH), "exec"), _BUS_SLASH_NS)
_BUS_SLASH_RE = _BUS_SLASH_NS["TELEGRAM_BUS_SLASH_COMMAND_RE"]

# The reference replacement callback for _TELEGRAM_DISPLAY_COMMAND_RE inside
# _telegram_command_text, isolated so the scanner can be compared on one line.
_DISPLAY_NAMES = {value: key for key, value in _TELEGRAM_COMMAND_ALIASES.items()}
_DISPLAY_NAMES_SUB = lambda text: _TELEGRAM_DISPLAY_COMMAND_RE.sub(  # noqa: E731
    lambda match: _DISPLAY_NAMES[match[0]], text
)

# The probe overrides _ensure_bot_identity with a fixed identity so the group
# policy decision can be driven without an Application. The override is written
# here rather than extracted, because the reference's version awaits getMe.
_GROUP_OVERRIDE_SRC = (
    "    _PROBE_BOT_ID = None\n"
    "    _PROBE_BOT_USERNAME = None\n"
    "    async def _ensure_bot_identity(self):\n"
    "        return self._PROBE_BOT_ID, self._PROBE_BOT_USERNAME\n"
)

_PROBE_SRC = (
    "class _TelegramProbe(BaseChannel):\n"
    "    name = 'telegram'\n"
    "    async def start(self): pass\n"
    "    async def stop(self): pass\n"
    "    async def send(self, msg): pass\n"
    + textwrap.indent(_IS_ALLOWED_SRC, "    ")
    + "\n"
    + textwrap.indent(_NORMALIZE_SRC, "    ")
    + "\n"
    + textwrap.indent(_MENTION_SRC, "    ")
    + "\n"
    + textwrap.indent(_GROUP_MESSAGE_SRC, "    ")
    + "\n"
    + _GROUP_OVERRIDE_SRC
)
_PROBE_NS: dict[str, Any] = {
    "BaseChannel": BaseChannel,
    "_TELEGRAM_COMMAND_ALIASES": _TELEGRAM_COMMAND_ALIASES,
}
exec(compile(_PROBE_SRC, str(RUNTIME_PATH), "exec"), _PROBE_NS)
_TelegramProbe = _PROBE_NS["_TelegramProbe"]

# Approvals are read from the pairing store through the module-global
# `is_approved` name imported into nanobot.channels.base. It is replaced with a
# deterministic in-memory lookup so both runtimes can be driven from the same
# fixture without touching ~/.nanobot/pairing.json.
APPROVED: dict[str, set[str]] = {}


def _fake_is_approved(channel: str, sender_id: str) -> bool:
    return sender_id in APPROVED.get(channel, set())


_base_mod.is_approved = _fake_is_approved


# ---------------------------------------------------------------------------
# 2. Direct imports that work
# ---------------------------------------------------------------------------

from nanobot.channels.telegram import manifest as _manifest_mod  # noqa: E402
from nanobot.channels.telegram import validation as _validation_mod  # noqa: E402
from nanobot.channels._manifest import GROUP_POLICIES  # noqa: E402

# httpx is needed to build the exact exception types validate() branches on.
import httpx  # noqa: E402


# ---------------------------------------------------------------------------
# 3. Corpora
# ---------------------------------------------------------------------------

# --- 3a. split corpus -------------------------------------------------------

HAND_SPLIT: list[tuple[str, int]] = [
    ("", 10),
    ("", 0),
    ("   ", 10),
    ("\t\n  \r\n", 4),
    # Python-whitespace that Go's strings.TrimSpace also strips, plus the four
    # separators Go does NOT strip, plus NBSP and IDEOGRAPHIC SPACE.
    ("\u001c\u001d\u001e\u001fabc", 10),
    ("\u00a0\u3000abc", 10),
    ("\u200b\u200babc", 10),  # ZERO WIDTH SPACE is NOT Python whitespace
    ("\ufeffabc", 10),  # BOM is NOT Python whitespace
    ("\u0085abc", 10),  # NEL IS Python whitespace
    ("abcde", 5),  # exactly max_len
    ("abcdef", 5),  # max_len + 1
    ("abcd", 5),  # shorter than max_len
    ("x" * 10, 10),
    ("x" * 11, 10),
    ("x" * 4000, 4000),
    ("x" * 4001, 4000),
    # newline exactly at the boundary
    ("abcde\nfghij", 6),
    ("abcde\nfghij", 5),
    ("abcde\nfghij", 7),
    ("a\nb", 2),
    ("\n\n\nabc", 2),
    # spaces / no break opportunity
    ("no newline at all just text here", 12),
    ("hello world foo bar", 11),
    ("hello   world   foo", 8),
    # fenced code blocks
    ("```\ncode line 1\ncode line 2\n```", 20),
    ("```\ncode line 1\ncode line 2\n```", 10),
    ("```python\nprint(1)\nprint(2)\nprint(3)\n```", 15),
    ("```", 3),
    ("```", 2),
    ("```\n", 3),
    ("```\n", 2),
    ("```\n\n\n\n\n", 3),
    ("```\ncode", 8),
    ("```\ncode", 4),
    ("```\ncode", 5),
    ("```\ncode", 6),
    ("```\ncode", 7),
    ("```a\nb\nc\n```d```e\nf", 6),
    ("```\n" + "z" * 30 + "\n```", 12),
    ("```\n" + "z" * 30 + "\n```", 4000),
    ("before\n```\naaaaaaaaaaaaaaaaaaaa\n```\nafter", 18),
    ("before\n```\naaaaaaaaaaaaaaaaaaaa\n```\nafter", 12),
    ("before\n```\naaaaaaaaaaaaaaaaaaaa\n```\nafter", 25),
    # odd number of fence markers
    ("a```b```c```d", 5),
    ("a```b```c```d", 8),
    ("```x```y", 4),
    # inline code
    ("`inline code` and more text", 8),
    ("`a` `b` `c` `d`", 5),
    ("prefix `code` suffix", 4000),
    # multiple fences
    ("```\na\n```\n```\nb\n```", 12),
    ("```\na\n```\n```\nb\n```", 7),
    ("```\na\n```\n```\nb\n```", 5),
    # non-ASCII: byte-vs-character indexing diverges
    ("\U0001f389" * 10, 4),
    ("\U0001f389\U0001f389\U0001f389", 4000),
    ("\u65e5\u672c\u8a9e\u306e\u30c6\u30ad\u30b9\u30c8\u3067\u3059\u3002\u3053\u308c\u306f\u30c6\u30b9\u30c8\u3067\u3059\u3002", 5),
    ("\u65e5\u672c\u8a9e" * 200, 4000),
    ("e\u0301e\u0301e\u0301e\u0301e\u0301", 3),
    ("\U0001f469\u200d\U0001f4bb ok \U0001f469\u200d\U0001f4bb ok", 6),
    ("```\n\U0001f389\u65e5\u672c\u8a9e\n```", 6),
    ("\u00e9\u00e8\u00ea mixed ascii text", 7),
    ("\U0001f600" * 4000 + "tail", 4000),
]

_RANDOM_ALPHABET = [
    "a", "b", " ", "\n", "`", "```", "\n```\n", "```python\n", "\n", " ",
    "\u00a0", "\u3000", "\U0001f389", "\u65e5", "e\u0301", "\t", "\u001c", "|", "-",
]

# Seeded: the dump must be byte-identical across runs so a failure can be
# reproduced. The generator is deliberately biased towards fence markers and
# newlines because that is where _split_telegram_markdown is subtle.
_rng = random.Random(0xC0FFEE)
RANDOM_SPLIT: list[tuple[str, int]] = []
for _ in range(2000):
    _length = _rng.randint(0, 60)
    _text = "".join(_rng.choice(_RANDOM_ALPHABET) for _ in range(_length))
    RANDOM_SPLIT.append((_text, _rng.randint(1, 24)))
for _ in range(200):
    _length = _rng.randint(0, 9000)
    _text = "".join(_rng.choice(_RANDOM_ALPHABET) for _ in range(_length))
    RANDOM_SPLIT.append((_text, _rng.choice([1, 2, 7, 64, 512, 3999, 4000, 4096])))

SPLIT_CASES = HAND_SPLIT + RANDOM_SPLIT

# --- 3b. markdown -> HTML corpus -------------------------------------------

# Seeded random Markdown for the renderer. The alphabet is biased towards the
# constructs the RE2 rewrites touch: bold/italic/strike delimiters, inline and
# fenced code, headers, blockquotes, links, list markers and pipe-table rows.
_RANDOM_MD_ALPHABET = [
    "*", "**", "_", "__", "~", "~~", "`", "``", "```", "```python\n", "\n",
    "# ", "###### ", "> ", "- ", "1. ", "[a](https://x/y)", "|", " | ", "\n|",
    "---", "| a | b |\n|---|---|\n| 1 | 2 |", " ", "\t", "text", "<", ">", "&",
    "\u00e9", "\U0001f389", "\u65e5", "e\u0301", "\u00a0", "\u200b",
]
_rng_md = random.Random(0xBEEF)
RANDOM_HTML: list[str] = []
for _ in range(1500):
    _length = _rng_md.randint(0, 40)
    RANDOM_HTML.append("".join(_rng_md.choice(_RANDOM_MD_ALPHABET) for _ in range(_length)))
# Whole documents built from whole lines, so table and list handling is reached.
for _ in range(200):
    _lines = _rng_md.randint(1, 12)
    RANDOM_HTML.append("\n".join(_rng_md.choice(_RANDOM_MD_ALPHABET) for _ in range(_lines)))

HTML_CASES: list[str] = [
    "",
    "plain text",
    "**bold** and __also bold__",
    "*not bold* and _italic_",
    "~~struck~~",
    "`inline code`",
    "```\nblock code\n```",
    "```python\nprint('<hi>')\n```",
    "```\n\n```",
    "````\nnot a fence\n````",
    "# Header",
    "###### Six",
    "####### Seven",
    "#no space",
    "> quoted text",
    ">no space quote",
    ">",
    "1. one\n2. two",
    "1.one",
    "- bullet\n* star",
    "-nospace",
    "[text](https://example.com)",
    "[text](url with space)",
    "[](empty)",
    "**<b>nested</b>**",
    "**bold with `code` inside**",
    "<script>alert('x')</script>",
    "a & b < c > d \" e ' f",
    "&amp; already escaped",
    "some_var_name and _real italic_",
    "_italic at start_ and _end_",
    "__dunder__",
    "a_b_c",
    "\u65e5\u672c\u8a9e **\u592a\u5b57**",
    "\U0001f389 emoji \U0001f389",
    "line1\nline2\nline3",
    "trailing newline\n",
    "\nleading newline",
    "**unclosed bold",
    "`unclosed code",
    "```\nunclosed fence",
    "text with \u00a0 nbsp",
    "text with \u3000 ideographic space",
    "| a | b |\n|---|---|\n| 1 | 2 |",
    "| a | b |\n| --- | --- |\n| 1 | 2 |",
    "| a | b |\n|---|---|\n| 1 | 2 |\n| 3 | 4 |",
    "|no sep|\n|only one row|",
    "| a | b |\n|---|---|",
    "  | a | b |\n  |---|---|\n  | 1 | 2 |",
    "||\n||\n||",
    "| a |\n|---|\n| \u65e5\u672c |",
    "| \U0001f389 | b |\n|---|---|\n| 1 | 2 |",
    "| a | b |\n|:--|--:|\n| 1 | 2 |",
    "| a | b |\n| :-: | - |\n| 1 | 2 |",
    "| a |\n|---|\n| **bold** |",
    "| a |\n|---|\n| `code` |",
    "| a | b | c |\n|---|---|---|\n| 1 | 2 |\n| 1 | 2 | 3 | 4 |",
    "text\n| a | b |\n|---|---|\n| 1 | 2 |\ntext",
    "| a | b |\n|---|---|\n\n| c | d |\n|---|---|\n| 3 | 4 |",
    "| a |\u00a0b |\n|---|---|\n| 1 | 2 |",
    "|\ta\t|\tb\t|\n|---|---|\n| 1 | 2 |",
    "| a | b |\n| - | - |\n| 1 | 2 |",
    "| a | b |\n| | |\n| 1 | 2 |",
    "| a | b |\n|  |\n| 1 | 2 |",
    "\u00a0| a | b |\n|---|---|\n| 1 | 2 |",
    "\u3000| a | b |\n|---|---|\n| 1 | 2 |",
    "```\n| a | b |\n|---|---|\n| 1 | 2 |\n```",
    "| a | b |\n|---|---|\n| `x|y` | 2 |",
    "**a** ~~b~~ `c` [d](e) _f_",
    "1. one\n- two\n> three\n# four",
]

for _i in range(120):
    _length = _rng.randint(0, 200)
    HTML_CASES.append("".join(_rng.choice(_RANDOM_ALPHABET + ["*", "_", "~", "#", ">", "[", "]", "(", ")", "<", "&"]) for _ in range(_length)))

STRIP_MD_CASES = list(HTML_CASES)
TABLE_BOX_CASES: list[list[str]] = [
    ["| a | b |", "|---|---|", "| 1 | 2 |"],
    ["| a | b |", "|---|---|"],
    ["| a | b |"],
    ["||", "||", "||"],
    ["| a |", "|---|", "| \u65e5\u672c |"],
    ["| \U0001f389 | b |", "|---|---|", "| 1 | 2 |"],
    ["  | a |  b |  ", "  |---|---|  ", "  | 1 | 2 |  "],
    ["| a | b | c |", "|---|---|---|", "| 1 | 2 |"],
    ["| a | b |", "|---|---|---|", "| 1 | 2 | 3 | 4 |"],
    ["| a |", "|:--|", "| 1 |"],
    ["| a |", "|--:|", "| 1 |"],
    ["| a |", "| :-: |", "| 1 |"],
    ["| a |", "| - |", "| 1 |"],
    ["| **a** | `b` |", "|---|---|", "| ~~c~~ | _d_ |"],
    ["|\ta\t|\tb\t|", "|---|---|", "| 1 | 2 |"],
    ["| a |", "| |", "| 1 |"],
    ["| a |", "|  |", "| 1 |"],
    ["| a |", "", "| 1 |"],
    ["| a | b |", "|---|---|", "| 1 | 2 |", "| 3 | 4 |"],
    ["no pipes here"],
    ["| a | b |", "| not a separator |", "| 1 | 2 |"],
    ["| a | b |", "|---|---|", "| \U0001f469\u200d\U0001f4bb | \u65e5 |"],
    ["| \u00a0 | b |", "|---|---|", "| 1 | 2 |"],
    ["| a | b |", "|---|---|", "|  |  |"],
    ["| a | b |", "| --- | --- |", "| 1 | 2 |"],
]

HTML_CASES = HTML_CASES + RANDOM_HTML

# --- 3c. proxy / validation corpus -----------------------------------------

PROXY_CASES: list[str] = [
    "",
    " ",
    "   \t ",
    "http://proxy.local:8080",
    "https://proxy.local:8080",
    "socks5://proxy.local:1080",
    "socks5h://proxy.local:1080",
    "HTTP://proxy.local:8080",
    "HtTpS://proxy.local:8080",
    "SOCKS5://proxy.local:1080",
    "Socks5H://proxy.local:1080",
    "ftp://proxy.local:21",
    "proxy.local:8080",
    "proxy.local",
    "127.0.0.1:8080",
    "http://user:pass@proxy.local:8080",
    "http://user@proxy.local:8080",
    "http://proxy.local:abc",
    "http://proxy.local:99999",
    "http://proxy.local:0",
    "http://[::1]:8080",
    "http://[2001:db8::1]:8080",
    "socks5://[::1]:1080",
    "http://",
    "https://",
    "socks5://",
    "http://:8080",
    "://proxy.local:8080",
    "http://proxy.local:8080/path",
    "http://proxy.local:8080?q=1",
    "\u00a0http://proxy.local:8080",
    "http://proxy.local:8080\u00a0",
    "http:/proxy.local:8080",
    "http//proxy.local:8080",
    "http://proxy local:8080",
    "socks4://proxy.local:1080",
    "http://PROXY.LOCAL:8080",
    "http://proxy.local:8080:8081",
    # Bracket handling, port validation and the NFKC netloc check.
    "http://[1.2.3.4]:80",
    "http://[::1",
    "http://::1]:80",
    "http://[v1.abc]:80",
    "http://[vZ.abc]:80",
    "http://[gggg::1]:80",
    "http://[fe80::1%25eth0]:80",
    "http://[fe80::822a:a8ff:fe49:470c%tESt]:80",
    "http://proxy.local:080",
    "http://proxy.local: 8080",
    "http://proxy.local:-1",
    "http://proxy.local:+8080",
    "http://proxy.local:65535",
    "http://proxy.local:65536",
    "http://\u2100.example:8080",
    "http://\uff1a8080",
    "http://\u3000proxy:8080",
    "\u0000http://proxy.local:8080",
    "http://proxy.local:8080\u0000",
    "\x0bhttp://proxy.local:8080",
    "\x0chttp://proxy.local:8080",
    "http://proxy.local:8080 ",
    "socks5h://user:pass@[::1]:1080",
    "http://@proxy.local:8080",
    "http://user@:8080",
    "socks5://",
    "Socks5://proxy.local:1080",
    "SOCKS5H://proxy.local:1080",
    "http:/",
    "http:",
    ":8080",
    "://",
    "a://b",
    "http://proxy.local:8080/",
]

VALIDATE_CASES: list[dict[str, Any]] = [
    {},
    {"token": ""},
    {"token": "   "},
    {"token": "123456:ABC"},
    {"token": "123456:" + "A" * 20},
    {"token": "notanumber:" + "A" * 20},
    {"token": "123456:" + "A" * 19},
    {"token": "123456:" + "A" * 20 + "-_"},
    {"token": "123456:ABCdefGHIjklMNOpqrs"},
    {"token": "123456:ABCdefGHIjklMNOpqrs", "proxy": "http://proxy.local:8080"},
    {"token": "123456:ABCdefGHIjklMNOpqrs", "proxy": "ftp://proxy.local:21"},
    {"token": "123456:ABCdefGHIjklMNOpqrs", "proxy": "not a proxy"},
    {"token": "123456:ABCdefGHIjklMNOpqrs", "proxy": ""},
    {"proxy": "http://proxy.local:8080"},
    {"token": "${TELEGRAM_BOT_TOKEN}"},
    {"token": "${MISSING_TOKEN_VAR}"},
    {"proxy": "${PROXY_URL}"},
    {"proxy": "${MISSING_PROXY_VAR}"},
    {"token": "${TELEGRAM_BOT_TOKEN}", "proxy": "${PROXY_URL}"},
    {"token": "${MISSING_TOKEN_VAR}", "proxy": "${PROXY_URL}"},
    {"token": "  ${TELEGRAM_BOT_TOKEN}  "},
    {"token": 12345},
    {"token": None},
    {"token": True},
    {"token": "123456:ABCdefGHIjklMNOpqrs", "extra": "ignored"},
]

# Environment visible to resolve_env_refs in the reference. The Go side installs
# exactly these values for the same cases.
VALIDATE_ENV = {
    "TELEGRAM_BOT_TOKEN": "123456:ABCdefGHIjklMNOpqrs",
    "PROXY_URL": "socks5h://proxy.local:1080",
}

# Scenarios for the getMe request. Each is replayed identically on the Go side
# through the injectable transport hook.
GETME_SCENARIOS: list[dict[str, Any]] = [
    {"id": "ok_full", "kind": "ok", "data": {"ok": True, "result": {"id": 42, "username": "nanobot", "first_name": "Nano"}}},
    {"id": "ok_id_only", "kind": "ok", "data": {"ok": True, "result": {"id": 42}}},
    {"id": "ok_no_identity", "kind": "ok", "data": {"ok": True, "result": {}}},
    {"id": "ok_result_not_dict", "kind": "ok", "data": {"ok": True, "result": "nope"}},
    {"id": "not_ok_description", "kind": "ok", "data": {"ok": False, "description": "Unauthorized"}},
    {"id": "not_ok_error", "kind": "ok", "data": {"ok": False, "error": "bad token"}},
    {"id": "not_ok_empty", "kind": "ok", "data": {"ok": False}},
    {"id": "http_400", "kind": "http_status", "status": 400},
    {"id": "http_401", "kind": "http_status", "status": 401},
    {"id": "http_403", "kind": "http_status", "status": 403},
    {"id": "http_404", "kind": "http_status", "status": 404},
    {"id": "http_429", "kind": "http_status", "status": 429},
    {"id": "http_500", "kind": "http_status", "status": 500},
    {"id": "transport", "kind": "transport"},
    {"id": "other", "kind": "other"},
]

# --- 3d. TelegramConfig corpus ---------------------------------------------

CONFIG_CASES: list[dict[str, Any]] = [
    {},
    {"enabled": True, "token": "123:abc"},
    {"allowFrom": ["alice", "bob"]},
    {"allow_from": ["alice"]},
    {"allowFrom": [], "allow_from": ["bob"]},
    {"allowFrom": ["alice"], "allow_from": ["bob"]},
    {"allowFrom": "*"},
    {"allowFrom": ["a", 1, None, True]},
    {"groupPolicy": "open"},
    {"groupPolicy": "allowlist"},
    {"groupPolicy": "mention"},
    {"groupPolicy": "nope"},
    {"mode": "webhook"},
    {"mode": "webhook", "webhookUrl": "https://example.com/hook", "webhookSecretToken": "abc"},
    {"mode": "webhook", "webhookUrl": "http://example.com/hook", "webhookSecretToken": "abc"},
    {"mode": "webhook", "webhookUrl": "https://example.com/hook", "webhookSecretToken": ""},
    {"mode": "webhook", "webhookUrl": "https://example.com/hook", "webhookSecretToken": "bad token!"},
    {"mode": "webhook", "webhookUrl": "https://example.com/hook", "webhookSecretToken": "A" * 256},
    {"mode": "webhook", "webhookUrl": "https://example.com/hook", "webhookSecretToken": "A" * 257},
    {"mode": "webhook", "webhookUrl": "  https://example.com/hook  ", "webhookSecretToken": "a-b_C9"},
    {"mode": "webhook", "webhookUrl": "example.com/hook", "webhookSecretToken": "abc"},
    {"mode": "webhook", "webhookUrl": "ftp://example.com/hook", "webhookSecretToken": "abc"},
    {"mode": "webhook", "webhookUrl": "https://", "webhookSecretToken": "abc"},
    {"mode": "polling", "webhookUrl": "http://example.com/hook"},
    {"mode": "nope"},
    {"mode": 1},
    {"webhookPath": ""},
    {"webhookPath": "   "},
    {"webhookPath": "telegram"},
    {"webhookPath": "  /hook  "},
    {"webhookPath": "\u00a0/hook"},
    {"webhookPath": "/hook/"},
    {"webhookListenPort": 0},
    {"webhookListenPort": 1},
    {"webhookListenPort": 65535},
    {"webhookListenPort": 65536},
    {"webhookListenPort": -1},
    {"webhookListenPort": "8080"},
    {"webhookListenPort": 8080.0},
    {"webhookListenPort": 8080.5},
    {"webhookListenPort": True},
    {"webhookMaxConnections": 0},
    {"webhookMaxConnections": 1},
    {"webhookMaxConnections": 100},
    {"webhookMaxConnections": 101},
    {"streamEditInterval": 0.1},
    {"streamEditInterval": 0.0999},
    {"streamEditInterval": 0},
    {"streamEditInterval": -1},
    {"streamEditInterval": "0.5"},
    {"streamEditInterval": 1},
    {"connectionPoolSize": 0},
    {"connectionPoolSize": -5},
    {"connectionPoolSize": 3.0},
    {"connectionPoolSize": 3.5},
    {"connectionPoolSize": "3"},
    {"poolTimeout": 0},
    {"poolTimeout": -1.5},
    {"poolTimeout": "2.5"},
    {"replyToMessage": True},
    {"replyToMessage": "true"},
    {"replyToMessage": "yes"},
    {"replyToMessage": "no"},
    {"replyToMessage": 1},
    {"replyToMessage": 0},
    {"replyToMessage": "maybe"},
    {"streaming": False},
    {"streaming": "false"},
    {"streaming": "0"},
    {"inlineKeyboards": True},
    {"richMessages": True},
    {"reactEmoji": ""},
    {"reactEmoji": "\U0001f440"},
    {"reactEmoji": 5},
    {"proxy": None},
    {"proxy": "http://p:1"},
    {"proxy": 5},
    {"unknownKey": "ignored"},
    {"token": 123},
    {"enabled": "yes"},
    {"token": "123:abc", "mode": "webhook", "webhookUrl": "https://e.com/h", "webhookSecretToken": "s", "webhookListenPort": 9000},
    # pydantic lax-coercion boundaries.
    {"replyToMessage": 2},
    {"replyToMessage": -1},
    {"replyToMessage": 0.0},
    {"replyToMessage": 1.0},
    {"replyToMessage": 0.5},
    {"replyToMessage": "TRUE"},
    {"replyToMessage": "True"},
    {"replyToMessage": "t"},
    {"replyToMessage": "f"},
    {"replyToMessage": "on"},
    {"replyToMessage": "off"},
    {"replyToMessage": "y"},
    {"replyToMessage": "n"},
    {"replyToMessage": "  true  "},
    {"replyToMessage": ""},
    {"replyToMessage": None},
    {"replyToMessage": []},
    {"streaming": None},
    {"webhookListenPort": None},
    {"webhookListenPort": "abc"},
    {"webhookListenPort": "  8080  "},
    {"webhookListenPort": "+8080"},
    {"webhookListenPort": "8_080"},
    {"webhookListenPort": "8080.0"},
    {"webhookListenPort": "0x10"},
    {"webhookListenPort": ""},
    {"webhookListenPort": False},
    {"webhookListenPort": [1]},
    {"connectionPoolSize": True},
    {"connectionPoolSize": "abc"},
    {"connectionPoolSize": 3.0000001},
    {"connectionPoolSize": 1e3},
    {"poolTimeout": "abc"},
    {"poolTimeout": True},
    {"poolTimeout": False},
    {"poolTimeout": " 2.5 "},
    {"poolTimeout": "inf"},
    {"poolTimeout": "nan"},
    {"poolTimeout": "1e3"},
    {"streamEditInterval": "abc"},
    {"streamEditInterval": None},
    {"streamEditInterval": True},
    {"allowFrom": None},
    {"allowFrom": 5},
    {"allowFrom": ["ok", ["nested"]]},
    {"allowFrom": ("tuple",)},
    {"token": None},
    {"token": []},
    {"reactEmoji": None},
    {"reactEmoji": True},
    {"mode": None},
    {"mode": ["polling"]},
    {"mode": True},
    {"groupPolicy": None},
    {"webhookPath": None},
    {"webhookPath": 5},
    # Python-whitespace trap: U+001C is stripped by Python's str.strip() but NOT
    # by Go's strings.TrimSpace.
    {"webhookPath": "\u001c/hook"},
    {"webhookPath": "\u001d\u001e/hook"},
    {"webhookPath": "\u0085/hook"},
    {"webhookPath": "\u2028/hook"},
    {"webhookPath": "\u200b/hook"},
    {"webhookPath": "/hook\u00a0"},
    {"webhookPath": "\t/hook\n"},
    # Multiple simultaneous field errors: pins pydantic's aggregation ORDER.
    {"token": 1, "webhookListenPort": 0, "streamEditInterval": 0.0},
    {"streamEditInterval": 0.0, "webhookListenPort": 0, "token": 1},
    {"mode": "nope", "token": 1},
    {"mode": "webhook", "token": 1},
    {"allowFrom": "*", "groupPolicy": "nope", "webhookMaxConnections": 0},
    {"replyToMessage": 2.0},
    {"replyToMessage": -1.0},
    {"replyToMessage": 1e0},
    {"replyToMessage": 1e-1},
    # Which whitespace the numeric string parsers trim.
    {"webhookListenPort": "\u001c8080"},
    {"webhookListenPort": "\u00a08080"},
    {"webhookListenPort": "\u30008080"},
    {"webhookListenPort": "\u200b8080"},
    {"webhookListenPort": "8080\u001c"},
    {"poolTimeout": "\u001c2.5"},
    {"poolTimeout": "\u00a02.5"},
    {"poolTimeout": "\u200b2.5"},
]

# webhook_url acceptance is driven by urllib.parse.urlparse, whose scheme/netloc
# split has its own rules (the scheme is lower-cased; netloc is only split for
# schemes in `uses_netloc`). Each case is paired with a valid secret so the ONLY
# failing check can be the URL.
WEBHOOK_URL_CASES: list[str] = [
    "",
    "   ",
    "https://example.com/hook",
    "  https://example.com/hook  ",
    "HTTPS://example.com/hook",
    "Https://example.com/hook",
    "http://example.com/hook",
    "ftp://example.com/hook",
    "https://",
    "https:///path",
    "https:example.com/hook",
    "https:/example.com/hook",
    "//example.com/hook",
    "example.com/hook",
    "https://example.com",
    "https://example.com/hook/path",
    "https://example.com:8443/hook",
    "https://user:pass@example.com/hook",
    "https://example.com/hook?a=1&b=2",
    "https://example.com/hook#frag",
    "https://[::1]/hook",
    "https://[::1]:8443/hook",
    "https://exa mple.com/hook",
    "https://example.com/hook\u00a0",
    "\u00a0https://example.com/hook",
    "https://\u00a0example.com/hook",
    "file:///etc/passwd",
    "wss://example.com/hook",
    "https://xn--bcher-kva.example/hook",
    "https://\u4f8b\u3048.jp/hook",
    # urlsplit removes TAB, CR and LF ANYWHERE in the URL before parsing.
    "https://exa\tmple.com/hook",
    "https://example.com/ho\nok",
    "https://example.com/ho\rok",
    "https://exa\u001cmple.com/hook",
    # Observable proof that the unsafe bytes are removed BEFORE the split: with
    # the TAB kept, `rest` would not start with "//" and netloc would be empty.
    "https:\t//example.com/hook",
    "https:\r//example.com/hook",
    "https:\n//example.com/hook",
    # urlparse itself raises ValueError on these, and validate_webhook_config
    # does NOT catch it, so the failure surfaces as a pydantic value_error with
    # urllib's message text.
    "https://[::1",
    "https://::1]/hook",
    "https://\u2100.example/hook",
    "https://\uff1a8080/hook",
    "https://[1.2.3.4]/hook",
    "https://[gggg::1]/hook",
]

for _url in WEBHOOK_URL_CASES:
    CONFIG_CASES.append(
        {"mode": "webhook", "webhookUrl": _url, "webhookSecretToken": "valid-secret_1"}
    )

# webhook_secret_token acceptance: `.strip()`, length <= 256, charset [A-Za-z0-9_-].
WEBHOOK_SECRET_CASES: list[str] = [
    "",
    "   ",
    "abc",
    "  abc  ",
    "\u00a0abc\u00a0",
    "\u001cabc",
    "a" * 256,
    "a" * 257,
    "A-Z_a-z0-9",
    "bad token",
    "bad!token",
    "bad.token",
    "bad/token",
    "\U0001f389",
    "abc\ndef",
]

for _secret in WEBHOOK_SECRET_CASES:
    CONFIG_CASES.append(
        {"mode": "webhook", "webhookUrl": "https://example.com/hook", "webhookSecretToken": _secret}
    )


# --- 3e. is_allowed matrix --------------------------------------------------

ALLOW_CONFIGS: list[dict[str, Any]] = [
    {},
    {"allow_from": []},
    {"allow_from": ["*"]},
    {"allowFrom": ["*"]},
    {"allow_from": ["alice"]},
    {"allowFrom": ["alice"]},
    {"allow_from": [], "allowFrom": ["bob"]},
    {"allow_from": ["alice"], "allowFrom": ["bob"]},
    {"allow_from": "alice"},
    {"allow_from": "alice,bob"},
    {"allow_from": {"alice": 1}},
    {"allow_from": ["123"]},
    {"allow_from": ["alice", "123"]},
    {"allow_from": ["alice|bob"]},
    {"allow_from": [123]},
    {"allow_from": ["ALICE"]},
    {"allow_from": ["\u00e1lice"]},
    {"allow_from": ["\u00b2"]},
]

ALLOW_SENDERS: list[str] = [
    "alice",
    "ALICE",
    "bob",
    "123",
    "123|alice",
    "alice|123",
    "123|bob",
    "alice|bob",
    "123|",
    "|alice",
    "|",
    "12|3|alice",
    "abc|alice",
    "123|alice|extra",
    "0|alice",
    "00|alice",
    "\u00b2|alice",
    "\u0660|alice",
    "123|\u00e1lice",
    "",
    "*",
    "\U0001f389|alice",
    "123|*",
]

# Which configs are replayed as pydantic TelegramConfig objects (the production
# shape) rather than raw dicts. Only configs that survive model_validate are.
ALLOW_MODEL_CONFIGS: list[dict[str, Any]] = [
    {},
    {"allowFrom": []},
    {"allowFrom": ["*"]},
    {"allowFrom": ["alice"]},
    {"allowFrom": ["123"]},
    {"allowFrom": ["alice", "123"]},
    {"allowFrom": ["alice|bob"]},
    {"allowFrom": ["ALICE"]},
    {"allowFrom": ["\u00e1lice"]},
    {"allowFrom": ["\u00b2"]},
    {"allowFrom": ["alice"], "groupPolicy": "open"},
]

ALLOW_APPROVED: list[list[str]] = [[], ["777"], ["alice"], ["123|alice"]]

# --- 3f. command corpus -----------------------------------------------------

NORMALIZE_CASES: list[str] = [
    "",
    "/dream_log",
    "/dream_log extra args",
    "/dream_log\n",
    "/dream_logextra",
    "/dream_logx",
    "/dream_restore",
    "/dream_restore now",
    "/dream_prompt set",
    "/evaluator_prompt",
    "/evaluator_prompt x",
    "/dream-log",
    "/dream-log extra",
    "/new",
    "/new now",
    "/help",
    "not a command",
    " /dream_log",
    "/DREAM_LOG",
    "/dream_log ",
    "/dream_log\tx",
    "/dream_log_prompt",
    "/dream_promptx",
    "/evaluator_prompt_extra",
    "/dream_restorex y",
]

COMMAND_TEXT_CASES: list[str] = [
    "",
    "run /dream-log now",
    "run /dream-restore now",
    "run /dream-prompt now",
    "run /evaluator-prompt now",
    "/dream-log",
    "x/dream-log",
    "/dream-logx",
    "x/dream-logx",
    "/dream-log.",
    "./dream-log",
    "-/dream-log",
    "/dream-log/",
    "a/dream-log/b",
    "\u65e5/dream-log\u65e5",
    "a\u00e9/dream-log",
    "\u00e9/dream-log",
    "1/dream-log",
    "_/dream-log",
    "`/dream_log`",
    "`/dream_log arg`",
    "`/dream-log`",
    "``/dream_log``",
    "text `/dream_restore now` more",
    "`/new`",
    "```\n/dream-log\n```",
    "```\n/dream_log\n```",
    "~~~\n/dream-log\n~~~",
    "```python\n/dream-log\n```\n/dream-log",
    "```\n```\n/dream-log",
    "```\nnot closed\n/dream-log",
    "  ```\n  /dream-log\n  ```",
    "before\n```\n/dream-log\n```\nafter /dream-log",
    "/dream-log and /dream-restore and /dream-prompt and /evaluator-prompt",
    "/dream-log,/dream-restore",
    "/dream-log\n/dream-restore",
    "/dream-log \n",
    "mixed /dream-log text /dream_log text",
    "\u00a0/dream-log",
]

# Single-line cases for the lookaround scanner. They deliberately include every
# character in the reference's `[\w/.-]` class on both sides of the command, the
# non-ASCII members of Python's `\w` that Go's ASCII `\w` would miss, and the
# boundary-free cases at the start and end of the line.
DISPLAY_LINE_CASES: list[str] = [
    "",
    "/dream-log",
    "/dream-log ",
    " /dream-log",
    "/dream-logx",
    "x/dream-log",
    "x /dream-log",
    "/dream-log /dream-restore",
    "/dream-log,/dream-restore",
    "/dream-log./dream-restore",
    "/dream-log-/dream-restore",
    "/dream-log_/dream-restore",
    "/dream-log//dream-restore",
    "/dream-logX",
    "X/dream-log",
    "1/dream-log",
    "_/dream-log",
    "./dream-log",
    "-/dream-log",
    "//dream-log",
    "\u00e9/dream-log",
    "\u00e9 /dream-log",
    "/dream-log\u00e9",
    "/dream-log \u00e9",
    "\u4e2d/dream-log",
    "/dream-log\u4e2d",
    "\u0660/dream-log",
    "\u00b2/dream-log",
    "\u0301/dream-log",
    "\u200b/dream-log",
    "/dream-log\u200b",
    "\u00a0/dream-log",
    "/dream-log\u00a0",
    "/dream-log\u0301",
    "/evaluator-prompt",
    "x/evaluator-prompt",
    "/dream_restore",
    "/dream-log /dream-log",
    "a/dream-log b/dream-log c",
    "\U00001C89/dream-log",
    "\u3164/dream-log",
    "/dream-log\u3164",
]

# Slash commands routed to AgentLoop. The corpus covers the `@bot` suffix, the
# Unicode `\w` and `\s` classes, and the trailing-newline case where Python's
# `$` and RE2's `$` disagree.
BUS_SLASH_CASES: list[str] = [
    "",
    "/new",
    "/new now",
    "/new  ",
    "/new\n",
    "/new\r\n",
    "/new\n\n",
    "/new\n/x",
    "/new@nanobot",
    "/new@nanobot arg",
    "/new@nanobot\n",
    "/new@nanobotx",
    "/new@",
    "/new@\u00e9",
    "/new@\u4e2d",
    "/new\u00a0arg",
    "/new\u3000arg",
    "/new\u200barg",
    "/newx",
    "/newx arg",
    "/NEW",
    "/compact",
    "/stop",
    "/restart",
    "/status",
    "/dream",
    "/history",
    "/goal",
    "/trigger",
    "/pairing",
    "/model",
    "/skill",
    "/dream_log",
    "/dream_restore",
    "/dream_prompt",
    "/evaluator_prompt",
    "/evaluator-prompt",
    "/dream-log",
    "/help",
    " /new",
    "x/new",
    "/new\targ",
    "/new\u001carg",
    "/new\x0barg",
    "/new arg\nmore",
]

# _has_mention_entity. The entity type, the presence of offset/length, the
# text_mention user id, out-of-range offsets and the substring fallback are all
# covered.
MENTION_CASES: list[dict[str, Any]] = [
    {"text": "@nanobot hi", "bot_username": "nanobot", "bot_id": 42, "entities": []},
    {
        "text": "@nanobot hi",
        "bot_username": "nanobot",
        "bot_id": 42,
        "entities": [{"type": "mention", "offset": 0, "length": 8}],
    },
    {
        "text": "@NANOBOT hi",
        "bot_username": "nanobot",
        "bot_id": 42,
        "entities": [{"type": "mention", "offset": 0, "length": 8}],
    },
    {
        "text": "@Nanobot hi",
        "bot_username": "NANOBOT",
        "bot_id": 42,
        "entities": [{"type": "mention", "offset": 0, "length": 8}],
    },
    {
        "text": "hey @nanobot",
        "bot_username": "nanobot",
        "bot_id": 42,
        "entities": [{"type": "mention", "offset": 4, "length": 8}],
    },
    {
        "text": "hey @nanobotx",
        "bot_username": "nanobot",
        "bot_id": 42,
        "entities": [{"type": "mention", "offset": 4, "length": 9}],
    },
    {
        "text": "hey @nanobotx",
        "bot_username": "nanobot",
        "bot_id": 42,
        "entities": [],
    },
    {
        "text": "\u00e9@nanobot",
        "bot_username": "nanobot",
        "bot_id": 42,
        "entities": [{"type": "mention", "offset": 1, "length": 8}],
    },
    {
        "text": "\u00e9@nanobot",
        "bot_username": "nanobot",
        "bot_id": 42,
        "entities": [{"type": "mention", "offset": 0, "length": 9}],
    },
    {
        "text": "@nanobot",
        "bot_username": "nanobot",
        "bot_id": 42,
        "entities": [{"type": "text_mention", "user_id": 42}],
    },
    {
        "text": "@nanobot",
        "bot_username": "nanobot",
        "bot_id": 42,
        "entities": [{"type": "text_mention", "user_id": 7}],
    },
    {
        "text": "@nanobot",
        "bot_username": "nanobot",
        "bot_id": None,
        "entities": [{"type": "text_mention", "user_id": 42}],
    },
    {
        "text": "@nanobot",
        "bot_username": "nanobot",
        "bot_id": 42,
        "entities": [{"type": "text_mention"}],
    },
    {
        "text": "@nanobot",
        "bot_username": "nanobot",
        "bot_id": 42,
        "entities": [{"type": "bold", "offset": 0, "length": 8}],
    },
    {
        "text": "@nanobot",
        "bot_username": "nanobot",
        "bot_id": 42,
        "entities": [{"type": "mention"}],
    },
    {
        "text": "@nanobot",
        "bot_username": "nanobot",
        "bot_id": 42,
        "entities": [{"type": "mention", "offset": 0}],
    },
    {
        "text": "@nanobot",
        "bot_username": "nanobot",
        "bot_id": 42,
        "entities": [{"type": "mention", "offset": 0, "length": 999}],
    },
    {
        "text": "@nanobot",
        "bot_username": "nanobot",
        "bot_id": 42,
        "entities": [{"type": "mention", "offset": -5, "length": 3}],
    },
    {"text": "", "bot_username": "nanobot", "bot_id": 42, "entities": []},
    {"text": "@nanobot", "bot_username": "", "bot_id": 42, "entities": []},
    {"text": "no mention", "bot_username": "nanobot", "bot_id": 42, "entities": []},
    {"text": "@nano", "bot_username": "nanobot", "bot_id": 42, "entities": []},
]

# _is_group_message_for_bot. A None text/caption exercises `message.text or ""`.
GROUP_MESSAGE_CASES: list[dict[str, Any]] = [
    {"chat_type": "private", "group_policy": "mention", "bot_id": 42, "bot_username": "nanobot", "text": None},
    {"chat_type": "group", "group_policy": "open", "bot_id": 42, "bot_username": "nanobot", "text": None},
    {"chat_type": "group", "group_policy": "mention", "bot_id": 42, "bot_username": "nanobot", "text": "@nanobot hi"},
    {"chat_type": "group", "group_policy": "mention", "bot_id": 42, "bot_username": "nanobot", "text": "hi"},
    {"chat_type": "group", "group_policy": "mention", "bot_id": 42, "bot_username": None, "text": "@nanobot hi"},
    {"chat_type": "group", "group_policy": "mention", "bot_id": 42, "bot_username": "", "text": "@nanobot hi"},
    {"chat_type": "group", "group_policy": "mention", "bot_id": 42, "bot_username": "nanobot", "caption": "@nanobot look", "text": None},
    {"chat_type": "group", "group_policy": "mention", "bot_id": 42, "bot_username": "nanobot", "text": "hi", "reply_user_id": 42},
    {"chat_type": "group", "group_policy": "mention", "bot_id": 42, "bot_username": "nanobot", "text": "hi", "reply_user_id": 7},
    {"chat_type": "group", "group_policy": "mention", "bot_id": 0, "bot_username": "nanobot", "text": "hi", "reply_user_id": 0},
    {"chat_type": "group", "group_policy": "mention", "bot_id": None, "bot_username": None, "text": "hi", "reply_user_id": None},
    {"chat_type": "group", "group_policy": "mention", "bot_id": 42, "bot_username": None, "text": "hi", "reply_user_id": 42},
    {"chat_type": "channel", "group_policy": "mention", "bot_id": 42, "bot_username": "nanobot", "text": "hi"},
    {
        "chat_type": "group",
        "group_policy": "mention",
        "bot_id": 42,
        "bot_username": "nanobot",
        "text": "hey @nanobot",
        "entities": [{"type": "mention", "offset": 4, "length": 8}],
    },
    {
        "chat_type": "group",
        "group_policy": "mention",
        "bot_id": 42,
        "bot_username": "nanobot",
        "caption": "hey @nanobot",
        "text": None,
        "caption_entities": [{"type": "mention", "offset": 4, "length": 8}],
    },
]


# ---------------------------------------------------------------------------
# 4. Helpers
# ---------------------------------------------------------------------------


def _dump_split() -> list[dict[str, Any]]:
    out = []
    for content, max_len in SPLIT_CASES:
        chunks = _split_telegram_markdown(content, max_len)
        out.append(
            {
                "content": content,
                "max_len": max_len,
                # Whether joining the chunks reproduces the input with leading
                # whitespace stripped. This is FALSE whenever the split dropped
                # whitespace at a cut point or re-emitted a fence, which is
                # normal — the dump records the reference's own answer so the Go
                # test can assert the same predicate rather than an assumption.
                "join_equals_lstrip": "".join(chunks) == content.lstrip(),
                "chunks": chunks,
            }
        )
    return out


def _dump_html() -> list[dict[str, Any]]:
    return [
        {
            "text": text,
            "html": _markdown_to_telegram_html(text),
            "stripped": _strip_md(text),
            "stripped_block": _strip_md_block(text),
            "escaped": _escape_telegram_html(text),
            "blockquote": _tool_hint_to_telegram_blockquote(text),
        }
        for text in HTML_CASES
    ]


def _dump_html_chunks() -> list[dict[str, Any]]:
    out = []
    for content, max_html_len in [
        ("", 4096),
        ("hello", 4096),
        ("**bold** " * 200, 4096),
        ("```\n" + "z" * 5000 + "\n```", 4096),
        ("`x` " * 100, 4096),
        ("<&>" * 2000, 4096),
        ("**bold**", 3),
        ("abc", 0),
        ("abc", 1),
        ("a" * 100, 10),
        ("\U0001f389" * 50, 40),
        ("| a | b |\n|---|---|\n| " + "\u65e5" * 200 + " | 2 |", 4096),
        ("para\n\n" * 500, 4096),
    ]:
        try:
            pairs = _split_telegram_markdown_html_chunks(content, max_html_len)
            out.append(
                {
                    "content": content,
                    "max_html_len": max_html_len,
                    "ok": True,
                    "chunks": [[raw, html] for raw, html in pairs],
                    "html_only": [html for _, html in pairs],
                }
            )
        except Exception as exc:  # noqa: BLE001 - the reference raises ValueError
            out.append(
                {
                    "content": content,
                    "max_html_len": max_html_len,
                    "ok": False,
                    "error": f"{type(exc).__name__}: {exc}",
                }
            )
    return out


def _dump_table_box() -> list[dict[str, Any]]:
    return [{"lines": lines, "out": _render_table_box(lines)} for lines in TABLE_BOX_CASES]


def _dump_proxy() -> list[dict[str, Any]]:
    return [
        {"input": value, "valid": _validation_mod._proxy_url_is_valid(value)}
        for value in PROXY_CASES
    ]


def _dump_config() -> list[dict[str, Any]]:
    out = []
    for values in CONFIG_CASES:
        try:
            model = TelegramConfig.model_validate(values)
        except Exception as exc:  # noqa: BLE001 - pydantic ValidationError
            errors = getattr(exc, "errors", lambda **_: [])()
            out.append(
                {
                    "values": values,
                    "ok": False,
                    "errors": [
                        {
                            "type": err.get("type"),
                            "loc": [str(part) for part in err.get("loc", ())],
                            "msg": err.get("msg"),
                        }
                        for err in errors
                    ],
                }
            )
            continue
        out.append({"values": values, "ok": True, "dump": _normalise_floats(model.model_dump(by_alias=True))})
    return out


def _normalise_floats(value: Any) -> Any:
    """Replace non-finite floats with their Python repr strings.

    `pool_timeout: "inf"` really does produce float('inf'). Python's json.dumps
    writes a bare `Infinity` token, which Go's encoding/json refuses to decode —
    and the Go port's own value is +Inf, not a number it can compare either. Both
    sides therefore agree on the string "inf"/"-inf"/"nan" instead.
    """
    if isinstance(value, float):
        if value != value:
            return "nan"
        if value == float("inf"):
            return "inf"
        if value == float("-inf"):
            return "-inf"
        return value
    if isinstance(value, dict):
        return {k: _normalise_floats(v) for k, v in value.items()}
    if isinstance(value, list):
        return [_normalise_floats(v) for v in value]
    return value


def _dump_allowed() -> list[dict[str, Any]]:
    out = []
    # Model-form configs are the PRODUCTION shape (TelegramChannel.__init__ runs
    # TelegramConfig.model_validate on a dict). `config_dump` is emitted so the
    # Go side builds an attribute-shaped section from the same validated values
    # rather than from the raw input, which pydantic may have coerced.
    model_configs: list[tuple[dict[str, Any], dict[str, Any]]] = []
    for cfg in ALLOW_MODEL_CONFIGS:
        model = TelegramConfig.model_validate(cfg)
        model_configs.append((cfg, model.model_dump(by_alias=True)))
    for approved in ALLOW_APPROVED:
        APPROVED["telegram"] = set(approved)
        for cfg in ALLOW_CONFIGS:
            for sender in ALLOW_SENDERS:
                out.append(
                    {
                        "config": cfg,
                        "config_form": "dict",
                        "approved": list(approved),
                        "sender": sender,
                        "result": _call_is_allowed(cfg, sender, as_model=False),
                    }
                )
        for cfg, model_dump in model_configs:
            for sender in ALLOW_SENDERS:
                out.append(
                    {
                        "config": cfg,
                        "config_dump": model_dump,
                        "config_form": "model",
                        "approved": list(approved),
                        "sender": sender,
                        "result": _call_is_allowed(cfg, sender, as_model=True),
                    }
                )
    APPROVED["telegram"] = set()
    return out


def _call_is_allowed(cfg: dict[str, Any], sender: str, *, as_model: bool) -> Any:
    probe = _TelegramProbe.__new__(_TelegramProbe)
    if as_model:
        probe.config = TelegramConfig.model_validate(cfg)
    else:
        probe.config = cfg
    try:
        return probe.is_allowed(sender)
    except Exception as exc:  # noqa: BLE001 - the reference can raise TypeError
        return f"{type(exc).__name__}: {exc}"


def _dump_commands() -> dict[str, Any]:
    return {
        "aliases": list(_TELEGRAM_COMMAND_ALIASES.items()),
        "display_re_pattern": _TELEGRAM_DISPLAY_COMMAND_RE.pattern,
        "display_re_findall": [
            _TELEGRAM_DISPLAY_COMMAND_RE.findall(text) for text in COMMAND_TEXT_CASES
        ],
        "normalize": [
            {"input": text, "out": _TelegramProbe._normalize_telegram_command(text)}
            for text in NORMALIZE_CASES
        ],
        "command_text": [
            {"input": text, "out": _telegram_command_text(text)} for text in COMMAND_TEXT_CASES
        ],
        # Single-LINE cases: the lookaround scanner is exercised without the
        # splitlines/fence machinery, so a failure points at one or the other.
        "display_lines": [
            {
                "input": text,
                "findall": _TELEGRAM_DISPLAY_COMMAND_RE.findall(text),
                "sub": _DISPLAY_NAMES_SUB(text),
            }
            for text in DISPLAY_LINE_CASES
        ],
        "bus_slash": [
            {"input": text, "match": _BUS_SLASH_RE.match(text) is not None}
            for text in BUS_SLASH_CASES
        ],
    }


def _dump_mentions() -> list[dict[str, Any]]:
    out = []
    for case in MENTION_CASES:
        entities = [
            SimpleNamespace(
                type=item["type"],
                offset=item.get("offset"),
                length=item.get("length"),
                user=(
                    SimpleNamespace(id=item["user_id"]) if item.get("user_id") is not None else None
                ),
            )
            for item in case["entities"]
        ]
        out.append(
            {
                "text": case["text"],
                "bot_username": case["bot_username"],
                "bot_id": case["bot_id"],
                "entities": case["entities"],
                "result": _TelegramProbe._has_mention_entity(
                    case["text"], entities, case["bot_username"], case["bot_id"]
                ),
            }
        )
    return out


def _dump_group_message() -> list[dict[str, Any]]:
    out = []
    for case in GROUP_MESSAGE_CASES:
        probe = _TelegramProbe.__new__(_TelegramProbe)
        probe.config = SimpleNamespace(group_policy=case["group_policy"])
        probe._PROBE_BOT_ID = case["bot_id"]
        probe._PROBE_BOT_USERNAME = case["bot_username"]
        message = SimpleNamespace(
            chat=SimpleNamespace(type=case["chat_type"]),
            text=case.get("text"),
            caption=case.get("caption"),
            entities=[
                SimpleNamespace(
                    type=item["type"],
                    offset=item.get("offset"),
                    length=item.get("length"),
                    user=(
                        SimpleNamespace(id=item["user_id"])
                        if item.get("user_id") is not None
                        else None
                    ),
                )
                for item in case.get("entities", [])
            ],
            caption_entities=[
                SimpleNamespace(
                    type=item["type"],
                    offset=item.get("offset"),
                    length=item.get("length"),
                    user=(
                        SimpleNamespace(id=item["user_id"])
                        if item.get("user_id") is not None
                        else None
                    ),
                )
                for item in case.get("caption_entities", [])
            ],
            reply_to_message=(
                SimpleNamespace(from_user=SimpleNamespace(id=case["reply_user_id"]))
                if case.get("reply_user_id") is not None
                else None
            ),
        )
        out.append({**case, "result": asyncio.run(probe._is_group_message_for_bot(message))})
    return out


def _dump_validate() -> list[dict[str, Any]]:
    out = []
    original_get_me = _validation_mod._get_me
    saved_env = {key: os.environ.get(key) for key in VALIDATE_ENV}
    os.environ.update(VALIDATE_ENV)
    try:
        for values in VALIDATE_CASES:
            for scenario in GETME_SCENARIOS:
                _install_get_me(scenario)
                try:
                    result = _validation_mod.validate(dict(values), None)
                except Exception as exc:  # noqa: BLE001
                    out.append(
                        {
                            "values": values,
                            "scenario": scenario["id"],
                            "ok": False,
                            "error": f"{type(exc).__name__}: {exc}",
                        }
                    )
                    continue
                payload = dict(result)
                # checked_at is datetime.now(UTC).isoformat(): nondeterministic by
                # construction, so both sides normalise it away.
                payload["checked_at"] = "CHECKED_AT"
                out.append(
                    {"values": values, "scenario": scenario["id"], "ok": True, "payload": payload}
                )
    finally:
        _validation_mod._get_me = original_get_me
        for key, value in saved_env.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value
    return out


def _install_get_me(scenario: dict[str, Any]) -> None:
    kind = scenario["kind"]

    def _get_me(token: str, proxy: str | None) -> dict[str, Any]:
        if kind == "ok":
            return scenario["data"]
        if kind == "http_status":
            request = httpx.Request("GET", "https://api.telegram.org/botX/getMe")
            response = httpx.Response(scenario["status"], request=request)
            raise httpx.HTTPStatusError("error", request=request, response=response)
        if kind == "transport":
            raise httpx.ConnectError("boom")
        raise RuntimeError("boom")

    _validation_mod._get_me = _get_me


def _dump_setup_spec() -> dict[str, Any]:
    spec = _manifest_mod.SETUP_SPEC
    return {
        "fields": [
            {
                "name": name,
                "kind": field.kind,
                "choices": sorted(field.choices),
                "default": field.default,
                "writable": field.writable,
                "snapshot": field.snapshot,
            }
            for name, field in spec.fields.items()
        ],
        "required": [
            {"alternatives": [list(alt) for alt in requirement.alternatives]}
            for requirement in spec.required
        ],
        "official_url": spec.official_url,
        "verifies_connection": spec.verifies_connection,
        "has_validator": spec.validator is not None,
        "secrets": sorted(spec.secrets),
        "simple_required_fields": list(spec.simple_required_fields),
        "group_policies": sorted(GROUP_POLICIES),
    }


def _dump_constants() -> dict[str, Any]:
    names = [
        "TELEGRAM_MAX_MESSAGE_LEN",
        "TELEGRAM_HTML_MAX_LEN",
        "TELEGRAM_RICH_MAX_LEN",
        "TELEGRAM_REPLY_CONTEXT_MAX_LEN",
        "TELEGRAM_RICH_DRAFT_MIN_INTERVAL",
        "COMPACTION_NOTICES_MAX",
        "POLL_STALE_SECONDS",
        "POLL_WATCH_INTERVAL",
        "RESTART_BACKOFF_INITIAL_SECONDS",
        "RESTART_BACKOFF_MAX_SECONDS",
        "APP_RESTART_SEND_WAIT_SECONDS",
        "_SEND_MAX_RETRIES",
        "_SEND_RETRY_BASE_DELAY",
        "_STREAM_EDIT_INTERVAL_DEFAULT",
    ]
    return {name: _PURE_NS[name] for name in names}


def _dump_unicodedata() -> dict[str, Any]:
    """Code-point predicates the Go port must reproduce without a Unicode table.

    `dw` uses unicodedata.east_asian_width; `is_allowed` uses str.isdigit. Both
    are enumerated from the reference interpreter so the Go tables can be
    checked against the exact same source of truth.
    """
    return {
        "unidata_version": unicodedata.unidata_version,
        "east_asian_width_wide": _ranges(
            lambda cp: unicodedata.east_asian_width(chr(cp)) in ("W", "F")
        ),
        "str_isdigit": _ranges(lambda cp: chr(cp).isdigit()),
        "str_isspace": _ranges(lambda cp: chr(cp).isspace()),
        "re_unicode_word": _ranges(lambda cp: re.match(r"\w", chr(cp)) is not None),
        "re_unicode_space": _ranges(lambda cp: re.match(r"\s", chr(cp)) is not None),
    }


def _ranges(predicate) -> list[list[int]]:
    out: list[list[int]] = []
    start = None
    for cp in range(0x110000):
        ok = bool(predicate(cp))
        if ok and start is None:
            start = cp
        elif not ok and start is not None:
            out.append([start, cp - 1])
            start = None
    if start is not None:
        out.append([start, 0x10FFFF])
    return out


def main() -> None:
    document = {
        "upstream_commit": "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9",
        "constants": _dump_constants(),
        "split": _dump_split(),
        "html": _dump_html(),
        "html_chunks": _dump_html_chunks(),
        "table_box": _dump_table_box(),
        "proxy": _dump_proxy(),
        "config_defaults": TelegramConfig().model_dump(by_alias=True),
        "config": _dump_config(),
        "allowed": _dump_allowed(),
        "commands": _dump_commands(),
        "mentions": _dump_mentions(),
        "group_message": _dump_group_message(),
        "validate": _dump_validate(),
        "setup_spec": _dump_setup_spec(),
        "unicode": _dump_unicodedata(),
    }
    json.dump(document, sys.stdout, ensure_ascii=False, sort_keys=False)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
