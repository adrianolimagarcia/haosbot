#!/usr/bin/env python3
"""Dump reference values for apply_patch and the file-diff machinery.

This is a third, independent dumper alongside dump_reference.py and
dump_outbound_events.py. It exists because dump_reference.py is a shared file
that several agents edit concurrently, and a read-modify-write race there would
destroy work. Keeping this section in its own file and its own Go test file
removes the race entirely.

Run with the project venv:
    .tools/venv/bin/python compat/python/dump_apply_patch.py

Covers, at HKUDS/nanobot@1bb712d3488915ca4ed9ccc1a93067ff722f5ab9:

  nanobot/agent/tools/apply_patch.py        ApplyPatchTool
  nanobot/utils/file_edit_events.py         FileDiff, Indel.opcodes,
                                            _append_text's callers,
                                            build_unified_diff_payload
  nanobot/agent/tools/filesystem.py         EditFileTool._format_summary

Every expectation the Go differential test compares against comes from
executing the real code here. Nothing is transcribed from documentation.
"""
from __future__ import annotations

import asyncio
import base64
import itertools
import json
import os
import random
import shutil
import sys
import tempfile
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

# A PID-suffixed scratch directory under .tools/. Parallel `go test ./compat/`
# runs by other agents have deleted each other's scratch dirs, so this name must
# not be shared with any other harness.
SCRATCH = ROOT / ".tools" / f"apply-patch-ref-{os.getpid()}"


def b64(data: bytes) -> str:
    return base64.b64encode(data).decode("ascii")


def opcode_json(op) -> list:
    return [op.tag, op.src_start, op.src_end, op.dest_start, op.dest_end]


# ---------------------------------------------------------------------------
# Indel.opcodes
# ---------------------------------------------------------------------------

ADVERSARIAL_SEQUENCES: list[tuple[list[str], list[str]]] = [
    ([], []),
    ([], ["a"]),
    (["a"], []),
    (["a"], ["a"]),
    (["a"], ["b"]),
    (["a"], ["a", "a"]),
    (["a"], ["a", "a", "a"]),
    (["a"], ["b", "a"]),
    (["a"], ["b", "a", "a"]),
    (["a"], ["a", "b"]),
    (["a"], ["a", "b", "a"]),
    (["a", "a"], ["a"]),
    (["a", "a"], ["a", "a"]),
    (["a", "a"], ["a", "a", "a"]),
    (["a", "b"], ["b", "a"]),
    (["a", "b"], ["c", "d"]),
    (["a", "b"], ["b", "b", "b", "c"]),
    (["a", "b"], ["c", "b", "b", "c"]),
    (["a", "b", "a"], ["a", "a"]),
    (["a", "b", "c"], ["a", "x", "c"]),
    (["a", "b", "c", "d"], ["a", "d"]),
    (["x", "a", "y"], ["a"]),
    (["b", "a"], ["a", "a", "c"]),
    (["1", "2", "3", "4"], ["1", "2", "3", "4"]),
    (["1", "2", "3", "4"], ["1", "3", "4"]),
    (["1", "2", "3", "4"], ["1", "2", "4"]),
    (["1", "2", "3", "4"], ["2", "3", "4"]),
    (["one", "two", "three"], ["one", "2", "three", "four"]),
    (["same"] * 12, ["same"] * 12),
    (["same"] * 12 + ["x"], ["same"] * 12),
    (["x"] + ["same"] * 12, ["same"] * 12),
    (["same"] * 8 + ["x"] + ["same"] * 8, ["same"] * 16),
    (["pre"], ["prefix"]),
    (["prefix"], ["pre"]),
    (["a", "ab"], ["ab", "a"]),
    (["", ""], [""]),
    (["", "a", ""], ["a", ""]),
    (["line"] * 3 + ["mid"] + ["line"] * 3, ["line"] * 6),
    (["a"] * 5, ["b"] * 5),
    (["dup", "dup", "uniq"], ["dup", "uniq", "dup"]),
]

EXHAUSTIVE_ALPHABET = ["a", "b", "c"]
EXHAUSTIVE_MAX_LEN = 4
RANDOM_CASES = 3000
RANDOM_SEED = 20240916


def dump_indel() -> list[dict]:
    """Indel.opcodes plus the added/deleted derivation FileDiff.from_text uses
    (utils/file_edit_events.py:58-69).

    The FULL opcode sequence is dumped, not just the counts: the sequence feeds
    unified_lines/_groups, so matching only the totals would not prove parity.
    """
    from rapidfuzz.distance import Indel

    out: list[dict] = []
    seen: set[tuple] = set()

    def add(before: list[str], after: list[str]) -> None:
        key = (tuple(before), tuple(after))
        if key in seen:
            return
        seen.add(key)
        codes = Indel.opcodes(before, after)
        added = deleted = 0
        for code in codes:
            if code.tag in ("replace", "delete"):
                deleted += code.src_end - code.src_start
            if code.tag in ("replace", "insert"):
                added += code.dest_end - code.dest_start
        out.append({
            "before": list(before),
            "after": list(after),
            "opcodes": [opcode_json(c) for c in codes],
            "added": added,
            "deleted": deleted,
        })

    for before, after in ADVERSARIAL_SEQUENCES:
        add(before, after)

    for n in range(0, EXHAUSTIVE_MAX_LEN + 1):
        for m in range(0, EXHAUSTIVE_MAX_LEN + 1):
            for before in itertools.product(EXHAUSTIVE_ALPHABET, repeat=n):
                for after in itertools.product(EXHAUSTIVE_ALPHABET, repeat=m):
                    add(list(before), list(after))

    rng = random.Random(RANDOM_SEED)
    for _ in range(RANDOM_CASES):
        k = rng.randint(1, 6)
        alphabet = "abcdefghij"[:k]
        n = rng.randint(0, 24)
        m = rng.randint(0, 24)
        add([rng.choice(alphabet) for _ in range(n)],
            [rng.choice(alphabet) for _ in range(m)])

    return out


# ---------------------------------------------------------------------------
# FileDiff / unified_lines / build_unified_diff_payload
# ---------------------------------------------------------------------------

FILE_DIFF_TEXTS: list[tuple[str, str]] = [
    ("", ""),
    ("", "a\n"),
    ("a\n", ""),
    ("a\n", "a\n"),
    ("a\n", "b\n"),
    ("hello world\n", "hello there\n"),
    ("one\ntwo\nthree\n", "one\n2\nthree\nfour\n"),
    ("one\r\ntwo\r\n", "one\r\ntwo\r\nthree\r\n"),
    ("one\r\ntwo\r\n", "one\r\n2\r\n"),
    ("a\r\nb", "a\nb\n"),
    ("no trailing", "no trailing\n"),
    ("a\n" * 20, "a\n" * 10 + "b\n" + "a\n" * 10),
    ("x\vy\n", "x\vy\nz\n"),
    ("x\x0cy\n", "x\x0cy\n"),
    ("x\x1cy\n", "x\x1cy\n"),
    ("x\u0085y\n", "x\u0085y\n"),
    ("x\u2028y\n", "x\u2028y\n"),
    ("x\u2029y\n", "x\u2029y\n"),
    ("caf\u00e9\n", "cafe\n"),
    ("a\nb\nc\nd\ne\nf\ng\nh\n", "a\nb\nX\nd\ne\nf\ng\nh\n"),
    ("a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n", "a\nb\nc\nd\ne\nf\ng\nh\ni\nZ\n"),
    ("\n\n\n", "\n\n"),
    ("trailing\n\n", "trailing\n"),
    ("dup\ndup\ndup\n", "dup\n"),
    ("line" * 1300 + "\n", "other\n"),
    ("\u00e9" * 1300 + "\n", "e\n"),
]


def dump_file_diff() -> list[dict]:
    """FileDiff.from_text + unified_lines over the same inputs."""
    from nanobot.utils.file_edit_events import FileDiff

    out = []
    for before, after in FILE_DIFF_TEXTS:
        diff = FileDiff.from_text(before, after)
        out.append({
            "before": before,
            "after": after,
            "before_lines": list(diff.before_lines),
            "after_lines": list(diff.after_lines),
            "opcodes": [opcode_json(c) for c in diff.opcodes],
            "added": diff.added,
            "deleted": diff.deleted,
            "matches_self": diff.matches(before, after),
            "groups_0": [[opcode_json(c) for c in g] for g in diff._groups(0)],
            "groups_1": [[opcode_json(c) for c in g] for g in diff._groups(1)],
            "groups_3": [[opcode_json(c) for c in g] for g in diff._groups(3)],
            "unified_context_3": list(diff.unified_lines("before", "after", 3)),
            "unified_context_0": list(diff.unified_lines("before", "after", 0)),
            "unified_context_1": list(diff.unified_lines("before", "after", 1)),
        })
    return out


def dump_unified_payload() -> list[dict]:
    """build_unified_diff_payload over the interesting option combinations."""
    from nanobot.utils.file_edit_events import build_unified_diff_payload

    pairs = [
        ("one\ntwo\nthree\n", "one\n2\nthree\nfour\n"),
        ("a\n", "a\n"),
        ("a\n", "b\n"),
        ("x\n" * 300 + "y\n" + "x\n" * 300, "x\n" * 300 + "z\n" + "x\n" * 300),
        ("l" * 1300 + "\n", "m\n"),
        ("caf\u00e9" * 1300 + "\n", "c\n"),
    ]
    arms = [
        ("defaults", {}),
        ("context_0", {"context_lines": 0}),
        ("context_neg", {"context_lines": -1}),
        ("context_1", {"context_lines": 1}),
        ("max_lines_1", {"max_lines": 1}),
        ("max_lines_2", {"max_lines": 2}),
        ("max_lines_0", {"max_lines": 0}),
        ("max_line_chars_0", {"max_line_chars": 0}),
        ("max_line_chars_3", {"max_line_chars": 3}),
        ("max_line_chars_1", {"max_line_chars": 1}),
        ("labels", {"fromfile": "a.txt", "tofile": "a.txt"}),
        ("none_before", {"_before_none": True}),
        ("none_after", {"_after_none": True}),
    ]
    out = []
    for before, after in pairs:
        for name, kwargs in arms:
            kwargs = dict(kwargs)
            b = None if kwargs.pop("_before_none", False) else before
            a = None if kwargs.pop("_after_none", False) else after
            payload = build_unified_diff_payload(b, a, **kwargs)
            out.append({
                "name": name,
                "before": before,
                "after": after,
                "payload": payload,
            })
    return out


# ---------------------------------------------------------------------------
# apply_patch internals
# ---------------------------------------------------------------------------

APPEND_TEXT_CASES: list[tuple[str, str]] = [
    ("", ""),
    ("", "x"),
    ("", "x\n"),
    ("", "\n"),
    ("x", ""),
    ("x\n", ""),
    ("x", "y"),
    ("x\n", "y"),
    ("x", "y\n"),
    ("x\n", "y\n"),
    ("x", "\ny"),
    ("x\n", "\ny"),
    ("x\r\n", "y\r\n"),
    ("x\r\n", "y"),
    ("x", "\r\ny"),
    ("x\r\ny", "z"),
    ("a\r\nb", "c"),
    ("\r\n", "x"),
    ("x", "\r\n"),
    ("x\n\n", "y"),
]

VALIDATE_PATH_CASES: list[str] = [
    "a.txt",
    " a.txt ",
    "\ta.txt\n",
    "",
    "   ",
    "\t\n ",
    "\u00a0",
    "\u00a0a\u00a0",
    "\u001c",
    "a\u001cb",
    "a\x00b",
    "\x00",
    " a\x00b ",
    "a'b\x00c",
    'a"b\x00c',
    "a'\"b\x00c",
    "caf\u00e9\x00x",
    "a\\b\x00c",
    "a\nb\x00c",
    "a\tb\x00c",
    "\x7f\x00",
]

LINE_DIFF_STATS_CASES: list[tuple[str | None, str | None]] = [
    (None, None),
    (None, "a\n"),
    ("a\n", None),
    ("a\n", "a\n"),
    ("a\n", "b\n"),
    ("one\ntwo\n", "one\n2\n"),
    ("one\ntwo\n", "one\n2\ntwo\n"),
    ("one\ntwo\n", "two\n"),
    ("", "a\n"),
    ("a\n", ""),
]


def dump_append_text() -> list[dict]:
    """_append_text's raw results.

    The Go side does not compare this section directly — appendText is
    unexported — but every pair below is also driven end to end through the
    "append_text_*" apply_patch cases, where the written file bytes and the
    returned summary are compared. This section is kept as the record of which
    inputs the tool-level cases cover.
    """
    from nanobot.agent.tools.apply_patch import _append_text

    return [{"content": c, "addition": a, "result": _append_text(c, a)}
            for c, a in APPEND_TEXT_CASES]


def dump_validate_path() -> list[dict]:
    """_validate_patch_path's raw results; see dump_append_text for why the Go
    side compares the "validate_path_*" apply_patch cases instead."""
    from nanobot.agent.tools.apply_patch import _PatchError, _validate_patch_path

    out = []
    for path in VALIDATE_PATH_CASES:
        try:
            out.append({"path": path, "ok": True, "value": _validate_patch_path(path)})
        except _PatchError as exc:
            out.append({"path": path, "ok": False, "error": str(exc)})
    return out


def dump_line_diff_stats() -> list[dict]:
    from nanobot.utils.file_edit_events import line_diff_stats

    return [{"before": b, "after": a, "added": line_diff_stats(b, a)[0],
             "deleted": line_diff_stats(b, a)[1]}
            for b, a in LINE_DIFF_STATS_CASES]


def dump_tracked() -> dict:
    from nanobot.utils.file_edit_events import TRACKED_FILE_EDIT_TOOLS, is_file_edit_tool

    names = sorted(TRACKED_FILE_EDIT_TOOLS)
    probes = ["write_file", "edit_file", "apply_patch", "read_file", "", "exec", "APPLY_PATCH"]
    return {
        "tools": names,
        "is_file_edit_tool": {p: is_file_edit_tool(p) for p in probes},
    }


def dump_display_path() -> list[dict]:
    """display_file_edit_path (utils/file_edit_events.py:154-160).

    Paths are reported relative to SCRATCH so the Go side can rebuild the same
    shape under its own temporary directory; the SCRATCH prefix inside a
    returned absolute path is replaced with <ROOT>.
    """
    from nanobot.utils.file_edit_events import display_file_edit_path

    ws = SCRATCH / "display"
    ws.mkdir(parents=True, exist_ok=True)
    inside = ws / "sub" / "file.txt"
    inside.parent.mkdir(parents=True, exist_ok=True)
    inside.write_text("x", encoding="utf-8")
    outside = SCRATCH / "outside.txt"
    outside.write_text("x", encoding="utf-8")

    out = []
    for path, workspace in [
        (inside, ws),
        (ws, ws),
        (outside, ws),
        (inside, None),
        (outside, None),
        (ws / "sub", ws),
    ]:
        value = display_file_edit_path(path, workspace)
        out.append({
            "input_rel": os.path.relpath(str(path), str(SCRATCH)),
            "workspace_rel": None if workspace is None else os.path.relpath(str(workspace), str(SCRATCH)),
            "display": value.replace(str(SCRATCH), "<ROOT>"),
        })
    return out


# ---------------------------------------------------------------------------
# apply_patch executions
# ---------------------------------------------------------------------------

TEXT = "text"
RAW = "raw"


def setup(entries: list[tuple[str, str, str]]) -> list[dict]:
    return [{"path": p, "kind": k, "data": v} for p, k, v in entries]


APPLY_PATCH_CASES: list[dict] = [
    # --- add ---------------------------------------------------------------
    {
        "name": "add_new_file",
        "setup": setup([]),
        "edits": [{"path": "new.txt", "action": "add", "new_text": "hello"}],
    },
    {
        "name": "add_new_file_trailing_newline",
        "setup": setup([]),
        "edits": [{"path": "new.txt", "action": "add", "new_text": "hello\n"}],
    },
    {
        "name": "add_new_file_empty_text",
        "setup": setup([]),
        "edits": [{"path": "new.txt", "action": "add", "new_text": ""}],
    },
    {
        "name": "add_new_file_crlf_text",
        "setup": setup([]),
        "edits": [{"path": "new.txt", "action": "add", "new_text": "a\r\nb\r\n"}],
    },
    {
        "name": "add_new_file_deep_path",
        "setup": setup([]),
        "edits": [{"path": "deep/nested/new.txt", "action": "add", "new_text": "x\n"}],
    },
    {
        "name": "add_existing_file_appends",
        "setup": setup([("a.txt", TEXT, "one\ntwo\n")]),
        "edits": [{"path": "a.txt", "action": "add", "new_text": "three\n"}],
    },
    {
        "name": "add_existing_file_no_trailing_newline",
        "setup": setup([("a.txt", TEXT, "one\ntwo")]),
        "edits": [{"path": "a.txt", "action": "add", "new_text": "three\n"}],
    },
    {
        "name": "add_existing_file_both_unterminated",
        "setup": setup([("a.txt", TEXT, "one\ntwo")]),
        "edits": [{"path": "a.txt", "action": "add", "new_text": "three"}],
    },
    {
        "name": "add_existing_file_addition_starts_with_newline",
        "setup": setup([("a.txt", TEXT, "one\ntwo")]),
        "edits": [{"path": "a.txt", "action": "add", "new_text": "\nthree"}],
    },
    {
        "name": "add_existing_crlf_file",
        "setup": setup([("a.txt", TEXT, "one\r\ntwo\r\n")]),
        "edits": [{"path": "a.txt", "action": "add", "new_text": "three\n"}],
    },
    {
        "name": "add_existing_crlf_file_no_trailing",
        "setup": setup([("a.txt", TEXT, "one\r\ntwo")]),
        "edits": [{"path": "a.txt", "action": "add", "new_text": "three\n"}],
    },
    {
        "name": "add_existing_empty_file",
        "setup": setup([("a.txt", TEXT, "")]),
        "edits": [{"path": "a.txt", "action": "add", "new_text": "x"}],
    },
    {
        "name": "add_to_same_file_twice",
        "setup": setup([("a.txt", TEXT, "one\n")]),
        "edits": [
            {"path": "a.txt", "action": "add", "new_text": "two\n"},
            {"path": "a.txt", "action": "add", "new_text": "three\n"},
        ],
    },
    {
        "name": "add_to_new_file_twice",
        "setup": setup([]),
        "edits": [
            {"path": "a.txt", "action": "add", "new_text": "one\n"},
            {"path": "a.txt", "action": "add", "new_text": "two\n"},
        ],
    },
    # --- replace -----------------------------------------------------------
    {
        "name": "replace_unique",
        "setup": setup([("a.txt", TEXT, "hello world\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "world", "new_text": "there"}],
    },
    {
        "name": "replace_not_found",
        "setup": setup([("a.txt", TEXT, "hello world\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "nope", "new_text": "x"}],
    },
    {
        "name": "replace_ambiguous",
        "setup": setup([("a.txt", TEXT, "x\nx\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "x", "new_text": "y"}],
    },
    {
        "name": "replace_ambiguous_overlapping",
        "setup": setup([("a.txt", TEXT, "aaa\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "aa", "new_text": "b"}],
    },
    {
        "name": "replace_ambiguous_multibyte",
        "setup": setup([("a.txt", TEXT, "\u00e9\u00e9\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "\u00e9", "new_text": "e"}],
    },
    {
        "name": "replace_empty_new_text",
        "setup": setup([("a.txt", TEXT, "one\ntwo\nthree\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "two\n", "new_text": ""}],
    },
    {
        "name": "replace_empty_new_text_inline",
        "setup": setup([("a.txt", TEXT, "one two three\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": " two", "new_text": ""}],
    },
    {
        "name": "replace_multiline",
        "setup": setup([("a.txt", TEXT, "a\nb\nc\nd\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "b\nc", "new_text": "X\nY\nZ"}],
    },
    {
        "name": "replace_crlf_file",
        "setup": setup([("a.txt", TEXT, "one\r\ntwo\r\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "two", "new_text": "2"}],
    },
    {
        "name": "replace_crlf_old_text",
        "setup": setup([("a.txt", TEXT, "one\ntwo\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "one\r\ntwo", "new_text": "x"}],
    },
    {
        "name": "replace_crlf_new_text",
        "setup": setup([("a.txt", TEXT, "one\ntwo\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "two", "new_text": "2\r\n3"}],
    },
    {
        "name": "replace_no_trailing_newline_file",
        "setup": setup([("a.txt", TEXT, "one\ntwo")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "two", "new_text": "2"}],
    },
    {
        "name": "replace_drops_trailing_newline",
        "setup": setup([("a.txt", TEXT, "one\ntwo\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "\ntwo\n", "new_text": ""}],
    },
    {
        "name": "replace_missing_file",
        "setup": setup([]),
        "edits": [{"path": "nope.txt", "action": "replace", "old_text": "a", "new_text": "b"}],
    },
    {
        "name": "replace_whole_file",
        "setup": setup([("a.txt", TEXT, "old\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "old\n", "new_text": "new\n"}],
    },
    # --- multi-edit / multi-file -------------------------------------------
    {
        "name": "multi_edit_same_file",
        "setup": setup([("a.txt", TEXT, "one\ntwo\n")]),
        "edits": [
            {"path": "a.txt", "action": "replace", "old_text": "one", "new_text": "1"},
            {"path": "a.txt", "action": "replace", "old_text": "two", "new_text": "2"},
        ],
    },
    {
        "name": "multi_edit_same_file_second_sees_pending",
        "setup": setup([("a.txt", TEXT, "one\n")]),
        "edits": [
            {"path": "a.txt", "action": "replace", "old_text": "one", "new_text": "two"},
            {"path": "a.txt", "action": "replace", "old_text": "two", "new_text": "three"},
        ],
    },
    {
        "name": "multi_file",
        "setup": setup([("a.txt", TEXT, "a1\n"), ("b.txt", TEXT, "b1\n")]),
        "edits": [
            {"path": "a.txt", "action": "replace", "old_text": "a1", "new_text": "a2"},
            {"path": "b.txt", "action": "add", "new_text": "b2\n"},
            {"path": "c.txt", "action": "add", "new_text": "c1\n"},
        ],
    },
    {
        "name": "multi_edit_mixed_actions",
        "setup": setup([("a.txt", TEXT, "a\n")]),
        "edits": [
            {"path": "a.txt", "action": "add", "new_text": "b\n"},
            {"path": "a.txt", "action": "replace", "old_text": "a", "new_text": "A"},
        ],
    },
    # --- errors ------------------------------------------------------------
    {
        "name": "unknown_action",
        "setup": setup([("a.txt", TEXT, "a\n")]),
        "edits": [{"path": "a.txt", "action": "delete"}],
    },
    {
        "name": "missing_path",
        "setup": setup([]),
        "edits": [{"action": "add", "new_text": "x"}],
    },
    {
        "name": "path_wrong_type",
        "setup": setup([]),
        "edits": [{"path": 5, "action": "add", "new_text": "x"}],
    },
    {
        "name": "path_empty",
        "setup": setup([]),
        "edits": [{"path": "   ", "action": "add", "new_text": "x"}],
    },
    {
        "name": "path_nbsp_only",
        "setup": setup([]),
        "edits": [{"path": "\u00a0", "action": "add", "new_text": "x"}],
    },
    {
        "name": "path_trimmed_nbsp",
        "setup": setup([]),
        "edits": [{"path": "\u00a0a.txt\u00a0", "action": "add", "new_text": "x"}],
    },
    {
        "name": "path_null_byte",
        "setup": setup([]),
        "edits": [{"path": "a\u0000b", "action": "add", "new_text": "x"}],
    },
    {
        "name": "path_null_byte_only",
        "setup": setup([]),
        "edits": [{"path": "\u0000", "action": "add", "new_text": "x"}],
    },
    {
        "name": "path_null_byte_quotes",
        "setup": setup([]),
        "edits": [{"path": "a'b\u0000c", "action": "add", "new_text": "x"}],
    },
    {
        "name": "missing_action",
        "setup": setup([("a.txt", TEXT, "a\n")]),
        "edits": [{"path": "a.txt"}],
    },
    {
        "name": "action_null",
        "setup": setup([("a.txt", TEXT, "a\n")]),
        "edits": [{"path": "a.txt", "action": None}],
    },
    {
        "name": "action_wrong_type",
        "setup": setup([("a.txt", TEXT, "a\n")]),
        "edits": [{"path": "a.txt", "action": 3}],
    },
    {
        "name": "missing_new_text_add",
        "setup": setup([("a.txt", TEXT, "a\n")]),
        "edits": [{"path": "a.txt", "action": "add"}],
    },
    {
        "name": "new_text_null_add",
        "setup": setup([("a.txt", TEXT, "a\n")]),
        "edits": [{"path": "a.txt", "action": "add", "new_text": None}],
    },
    {
        "name": "new_text_wrong_type_add",
        "setup": setup([("a.txt", TEXT, "a\n")]),
        "edits": [{"path": "a.txt", "action": "add", "new_text": 5}],
    },
    {
        "name": "new_text_wrong_type_add_new_file",
        "setup": setup([]),
        "edits": [{"path": "a.txt", "action": "add", "new_text": 5}],
    },
    {
        "name": "missing_old_text",
        "setup": setup([("a.txt", TEXT, "a\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "new_text": "b"}],
    },
    {
        "name": "old_text_zero",
        "setup": setup([("a.txt", TEXT, "a\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": 0, "new_text": "b"}],
    },
    {
        "name": "old_text_empty",
        "setup": setup([("a.txt", TEXT, "a\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "", "new_text": "b"}],
    },
    {
        "name": "old_text_wrong_type",
        "setup": setup([("a.txt", TEXT, "a\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": 5, "new_text": "b"}],
    },
    {
        "name": "missing_new_text_replace",
        "setup": setup([("a.txt", TEXT, "a\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "a"}],
    },
    {
        "name": "new_text_null_replace",
        "setup": setup([("a.txt", TEXT, "a\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "a", "new_text": None}],
    },
    {
        "name": "new_text_wrong_type_replace",
        "setup": setup([("a.txt", TEXT, "a\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "a", "new_text": 5}],
    },
    {
        "name": "empty_edits",
        "setup": setup([]),
        "edits": [],
    },
    {
        "name": "edits_missing",
        "setup": setup([]),
        "edits": "__OMIT__",
    },
    {
        "name": "edits_null",
        "setup": setup([]),
        "edits": None,
    },
    {
        "name": "edits_string",
        "setup": setup([]),
        "edits": "abc",
    },
    {
        "name": "edits_empty_string",
        "setup": setup([]),
        "edits": "",
    },
    {
        "name": "edits_dict",
        "setup": setup([]),
        "edits": {"a": 1},
    },
    {
        "name": "edits_empty_dict",
        "setup": setup([]),
        "edits": {},
    },
    {
        "name": "edits_int",
        "setup": setup([]),
        "edits": 5,
    },
    {
        "name": "edits_zero",
        "setup": setup([]),
        "edits": 0,
    },
    {
        "name": "edits_true",
        "setup": setup([]),
        "edits": True,
    },
    {
        "name": "edits_false",
        "setup": setup([]),
        "edits": False,
    },
    {
        "name": "non_dict_edit_int",
        "setup": setup([]),
        "edits": [1],
    },
    {
        "name": "non_dict_edit_string",
        "setup": setup([]),
        "edits": ["x"],
    },
    {
        "name": "non_dict_edit_null",
        "setup": setup([]),
        "edits": [None],
    },
    {
        "name": "non_dict_edit_list",
        "setup": setup([]),
        "edits": [[{"path": "a.txt", "action": "add", "new_text": "x"}]],
    },
    {
        "name": "non_utf8_add",
        "setup": setup([("a.txt", RAW, b"caf\xe9\n")]),
        "edits": [{"path": "a.txt", "action": "add", "new_text": "x\n"}],
    },
    {
        "name": "non_utf8_replace",
        "setup": setup([("a.txt", RAW, b"caf\xe9\n")]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "caf", "new_text": "x"}],
    },
    {
        "name": "outside_workspace",
        "setup": setup([]),
        "edits": [{"path": "../escape.txt", "action": "add", "new_text": "x"}],
    },
    # --- dry run -----------------------------------------------------------
    {
        "name": "dry_run_no_write",
        "setup": setup([("a.txt", TEXT, "one\n")]),
        "edits": [
            {"path": "a.txt", "action": "replace", "old_text": "one", "new_text": "1"},
            {"path": "b.txt", "action": "add", "new_text": "new\n"},
        ],
        "dry_run": True,
    },
    {
        "name": "dry_run_failure_still_fails",
        "setup": setup([]),
        "edits": [{"path": "a.txt", "action": "replace", "old_text": "x", "new_text": "y"}],
        "dry_run": True,
    },
    # --- rollback ----------------------------------------------------------
    # compare_text is False because the failure that triggers the rollback is an
    # OS error, and Go and Python word those differently by design (the port
    # reports Go's error strings everywhere, e.g. edit_file's
    # permissionOrGenericEdit). The file-state comparison below is what proves
    # the rollback happened.
    {
        "name": "rollback_restores_and_deletes",
        "setup": setup([("a.txt", TEXT, "original\n"), ("blocker", TEXT, "not a directory\n")]),
        "edits": [
            {"path": "a.txt", "action": "replace", "old_text": "original", "new_text": "changed"},
            {"path": "b.txt", "action": "add", "new_text": "brand new\n"},
            {"path": "blocker/x.txt", "action": "add", "new_text": "doomed\n"},
        ],
        "compare_text": False,
    },
]

# _append_text and _validate_patch_path are module-private upstream, so the Go
# side cannot call the ported equivalents directly. Both are instead driven end
# to end through the real tool, one case per input: the written file bytes prove
# _append_text, and the returned text plus the created file name prove
# _validate_patch_path.
for _i, (_content, _addition) in enumerate(APPEND_TEXT_CASES):
    APPLY_PATCH_CASES.append({
        "name": f"append_text_{_i:02d}",
        "setup": setup([("a.txt", TEXT, _content)]),
        "edits": [{"path": "a.txt", "action": "add", "new_text": _addition}],
    })

for _i, _path in enumerate(VALIDATE_PATH_CASES):
    APPLY_PATCH_CASES.append({
        "name": f"validate_path_{_i:02d}",
        "setup": setup([]),
        "edits": [{"path": _path, "action": "add", "new_text": "x\n"}],
    })


def _setup_bytes(entry: dict) -> bytes:
    if entry["kind"] == TEXT:
        return entry["data"].encode("utf-8")
    data = entry["data"]
    return data if isinstance(data, bytes) else base64.b64decode(data)


def _setup_json(entries: list[dict]) -> list[dict]:
    return [{"path": e["path"], "b64": b64(_setup_bytes(e))} for e in entries]


def _files_json(ws: Path) -> list[dict]:
    out = []
    for path in sorted(ws.rglob("*")):
        if path.is_file():
            out.append({
                "path": path.relative_to(ws).as_posix(),
                "b64": b64(path.read_bytes()),
            })
    return out


def _diffs_json(diffs, ws: Path):
    if diffs is None:
        return None
    out = {}
    for key, diff in diffs.items():
        rel = os.path.relpath(str(key), str(ws))
        out[rel] = {
            "added": diff.added,
            "deleted": diff.deleted,
            "before_lines": list(diff.before_lines),
            "after_lines": list(diff.after_lines),
            "opcodes": [opcode_json(c) for c in diff.opcodes],
        }
    return out


def run_apply_patch_case(case: dict) -> dict:
    from nanobot.agent.tools.apply_patch import ApplyPatchTool

    ws = SCRATCH / "cases" / case["name"]
    if ws.exists():
        shutil.rmtree(ws)
    ws.mkdir(parents=True, exist_ok=True)
    for entry in case["setup"]:
        target = ws / entry["path"]
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(_setup_bytes(entry))

    tool = ApplyPatchTool(workspace=ws, allowed_dir=ws, restrict_to_workspace=True)

    omit_edits = case["edits"] == "__OMIT__"
    dry_run = case.get("dry_run", False)
    kwargs = {"dry_run": dry_run}
    if not omit_edits:
        kwargs["edits"] = case["edits"]

    async def call():
        return await tool.execute(**kwargs)

    result = asyncio.run(call())

    return {
        "name": case["name"],
        "setup": _setup_json(case["setup"]),
        "omit_edits": omit_edits,
        "edits": None if omit_edits else case["edits"],
        "dry_run": dry_run,
        "text": str(result).replace(str(ws), "<WS>"),
        "compare_text": case.get("compare_text", True),
        "is_error": bool(getattr(result, "is_error", False)),
        "result_type": type(result).__name__,
        "files": _files_json(ws),
        "file_diffs": _diffs_json(getattr(result, "file_diffs", None), ws),
    }


# ---------------------------------------------------------------------------
# edit_file summaries (the "(+N/-M)" suffix)
# ---------------------------------------------------------------------------

EDIT_FILE_CASES: list[dict] = [
    {
        "name": "replace_single_line",
        "setup": setup([("a.txt", TEXT, "hello world\n")]),
        "args": {"path": "a.txt", "old_text": "world", "new_text": "there"},
    },
    {
        "name": "replace_same_line_count",
        "setup": setup([("a.txt", TEXT, "one\ntwo\nthree\n")]),
        "args": {"path": "a.txt", "old_text": "two", "new_text": "2"},
    },
    {
        "name": "replace_adds_lines",
        "setup": setup([("a.txt", TEXT, "one\nthree\n")]),
        "args": {"path": "a.txt", "old_text": "one\n", "new_text": "one\ntwo\n"},
    },
    {
        "name": "replace_removes_lines",
        "setup": setup([("a.txt", TEXT, "one\ntwo\nthree\n")]),
        "args": {"path": "a.txt", "old_text": "one\ntwo\n", "new_text": "one\n"},
    },
    {
        "name": "create_new_file",
        "setup": setup([]),
        "args": {"path": "new/deep.txt", "old_text": "", "new_text": "hi"},
    },
    {
        "name": "create_new_file_multiline",
        "setup": setup([]),
        "args": {"path": "new.txt", "old_text": "", "new_text": "a\nb\nc"},
    },
    {
        "name": "overwrite_empty_file",
        "setup": setup([("a.txt", TEXT, "")]),
        "args": {"path": "a.txt", "old_text": "", "new_text": "x\ny\n"},
    },
    {
        "name": "overwrite_whitespace_file",
        "setup": setup([("a.txt", TEXT, "   \n")]),
        "args": {"path": "a.txt", "old_text": "", "new_text": "x\n"},
    },
    {
        "name": "replace_crlf_file",
        "setup": setup([("a.txt", TEXT, "one\r\ntwo\r\n")]),
        "args": {"path": "a.txt", "old_text": "two", "new_text": "2"},
    },
    {
        "name": "replace_all_occurrences",
        "setup": setup([("a.txt", TEXT, "x\nx\nx\n")]),
        "args": {"path": "a.txt", "old_text": "x", "new_text": "y", "replace_all": True},
    },
    # A workspace escape raises WorkspaceBoundaryError, which is a
    # PermissionError, so the reference's except arm emits "Error: ..." rather
    # than "Error editing file: ...". This case is why permissionOrGenericEdit
    # has to special-case boundaryError.
    {
        "name": "boundary_escape",
        "setup": setup([]),
        "args": {"path": "../escape.txt", "old_text": "", "new_text": "x"},
    },
]


def run_edit_file_case(case: dict) -> dict:
    from nanobot.agent.tools.filesystem import EditFileTool

    ws = SCRATCH / "edit" / case["name"]
    if ws.exists():
        shutil.rmtree(ws)
    ws.mkdir(parents=True, exist_ok=True)
    for entry in case["setup"]:
        target = ws / entry["path"]
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(_setup_bytes(entry))

    tool = EditFileTool(workspace=ws, allowed_dir=ws, restrict_to_workspace=True)

    async def call():
        return await tool.execute(**case["args"])

    result = asyncio.run(call())

    return {
        "name": case["name"],
        "setup": _setup_json(case["setup"]),
        "args": case["args"],
        "text": str(result).replace(str(ws), "<WS>"),
        "compare_text": True,
        "is_error": bool(getattr(result, "is_error", False)),
        "files": _files_json(ws),
        "file_diffs": _diffs_json(getattr(result, "file_diffs", None), ws),
    }


def main() -> int:
    SCRATCH.mkdir(parents=True, exist_ok=True)
    try:
        doc = {
            "upstream_commit": "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9",
            "indel": dump_indel(),
            "file_diff": dump_file_diff(),
            "unified_payload": dump_unified_payload(),
            "append_text": dump_append_text(),
            "validate_path": dump_validate_path(),
            "line_diff_stats": dump_line_diff_stats(),
            "tracked": dump_tracked(),
            "display_path": dump_display_path(),
            "apply_patch": [run_apply_patch_case(c) for c in APPLY_PATCH_CASES],
            "edit_file": [run_edit_file_case(c) for c in EDIT_FILE_CASES],
        }
    finally:
        shutil.rmtree(SCRATCH, ignore_errors=True)

    json.dump(doc, sys.stdout, sort_keys=True, ensure_ascii=False)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
