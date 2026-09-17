#!/usr/bin/env python3
"""Dump reference values for the legacy HISTORY.md -> history.jsonl migration.

This is a third, independent dumper alongside dump_reference.py and
dump_outbound_events.py. It exists for the same reason the second one does:
dump_reference.py is a shared file that several agents edit concurrently, and a
read-modify-write race there would destroy work. Keeping this section in its own
file and its own Go test file removes the race entirely.

Run with the project venv:
    .tools/venv/bin/python compat/python/dump_legacy_history.py

Covers nanobot/agent/memory.py:106-225 at
HKUDS/nanobot@1bb712d3488915ca4ed9ccc1a93067ff722f5ab9.

Environment
-----------
LEGACY_HISTORY_UTC_OFFSET_SECONDS
    Optional. When set, every timestamp this dumper formats is computed at that
    fixed UTC offset instead of the machine's local zone. _legacy_fallback_timestamp
    calls datetime.fromtimestamp() with no tz argument, so its result depends on
    the process's local zone; the Go test pins this variable to the offset Go's
    time.Local uses, which makes the comparison independent of the machine.
"""
from __future__ import annotations

import json
import os
import shutil
import sys
import tempfile
from datetime import datetime, timedelta, timezone
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

# Scratch directories live under .tools/ and carry the PID, because several
# agents run `go test ./compat/` concurrently and a fixed name would let one
# run delete another's scratch tree.
SCRATCH_ROOT = ROOT / ".tools"

# Every scenario pins this mtime on the legacy file so the fallback timestamp
# path is deterministic: 2020-09-13 12:27:00 UTC.
MTIME_EPOCH = 1_600_000_020.0

_OFFSET_ENV = "LEGACY_HISTORY_UTC_OFFSET_SECONDS"


def _store_class():
    from nanobot.agent.memory import MemoryStore

    return MemoryStore


def _scratch(prefix: str) -> Path:
    return Path(
        tempfile.mkdtemp(dir=SCRATCH_ROOT, prefix=f"{prefix}-ref-{os.getpid()}-")
    )


def _pin_mtime(path: Path, epoch: float = MTIME_EPOCH) -> int:
    """Set *path*'s mtime and return the nanosecond value the filesystem kept.

    The value is read back rather than assumed: os.utime takes a float, the
    filesystem stores nanoseconds, and the Go side must set the identical
    instant for the fallback timestamp to be comparable.
    """
    os.utime(path, (epoch, epoch))
    return os.stat(path).st_mtime_ns


def _make_store_without_legacy() -> tuple[Path, object]:
    """A real MemoryStore in a workspace that has no HISTORY.md.

    Constructing it here runs the migration, which is a no-op when the legacy
    file is absent — so the store is fully formed and the legacy file can then
    be created by hand for the individual helpers to read.
    """
    workspace = _scratch("legacy-history")
    return workspace, _store_class()(workspace)


# ---------------------------------------------------------------------------
# _parse_legacy_history / _split_legacy_history_chunks
# ---------------------------------------------------------------------------

PARSE_CASES: list[tuple[str, str]] = [
    ("empty", ""),
    ("whitespace_only", "   \n\t\n  "),
    ("whitespace_only_python_only", "\x1c\x1d\x1e\x1f"),
    ("single_entry", "[2024-01-01 10:00] hello"),
    ("several_entries", "[2024-01-01 10:00] a\n[2024-01-02 11:00] b"),
    ("blank_line_separated", "[2024-01-01 10:00] a\n\n[2024-01-02 11:00] b"),
    ("blank_lines_between_paragraphs", "paragraph one\n\nparagraph two"),
    ("raw_chunk", "[2024-01-01 10:00] [RAW] some raw text"),
    ("raw_chunk_multiline", "[2024-01-01 10:00] [RAW] line one\nline two\nline three"),
    ("raw_chunk_absorbs_entry_start",
     "[2024-01-01 10:00] [RAW] line one\n[2024-01-02 11:00] USER: still raw?"),
    ("raw_chunk_absorbs_tools_line",
     "[2024-01-01 10:00] [RAW] line one\n[2024-01-02 11:00] ASSISTANT [tools: read]: more"),
    ("raw_chunk_does_not_absorb_lowercase",
     "[2024-01-01 10:00] [RAW] line one\n[2024-01-02 11:00] user: lowercase role"),
    ("raw_chunk_does_not_absorb_tools_only",
     "[2024-01-01 10:00] [RAW] line one\n[2024-01-02 11:00] [tools: read] no role"),
    ("timestamp_without_time", "[2024-01-01] just a date"),
    ("timestamp_without_time_then_full",
     "[2024-01-01] just a date\n[2024-01-02 11:00] full"),
    ("timestamp_with_seconds", "[2024-01-01 10:00:30] has seconds"),
    ("crlf_line_endings", "[2024-01-01 10:00] a\r\n[2024-01-02 11:00] b\r\n"),
    ("crlf_only_blank_separator", "[2024-01-01 10:00] a\r\n\r\n[2024-01-02 11:00] b"),
    ("lone_cr_line_endings", "[2024-01-01 10:00] a\r[2024-01-02 11:00] b"),
    ("no_timestamp_at_all", "just some text\nmore text"),
    ("timestamp_plus_trailing_text", "[2024-01-01 10:00] hello trailing text"),
    ("leading_and_trailing_whitespace", "   [2024-01-01 10:00] hi   "),
    ("leading_whitespace_each_line", "  [2024-01-01 10:00] hi\n  [2024-01-02 11:00] ho"),
    ("tools_line", "[2024-01-01 10:00] ASSISTANT [tools: read, write]:\ndid stuff"),
    ("tools_line_without_role", "[2024-01-01 10:00] [tools: read]:\ndid stuff"),
    ("timestamp_only_chunk", "[2024-01-01 10:00]"),
    ("timestamp_only_then_entry", "[2024-01-01 10:00]\n[2024-01-02 11:00] b"),
    ("entry_start_inside_raw_chunk",
     "[2024-01-01 10:00] [RAW] first\n[2024-01-02 11:00] USER: second\nplain third"),
    ("entry_start_after_raw_chunk_ends",
     "[2024-01-01 10:00] [RAW] first\nplain second\n[2024-01-02 11:00] USER: third"),
    ("trailing_blank_lines", "[2024-01-01 10:00] a\n\n\n"),
    ("only_blank_lines_between", "[2024-01-01 10:00] a\n \n\t\n[2024-01-02 11:00] b"),
    ("nbsp_after_bracket", "[2024-01-01 10:00]\u00a0hello"),
    ("file_separator_after_bracket", "[2024-01-01 10:00]\x1chello"),
    ("unicode_digits_timestamp", "[\u0662\u0660\u0662\u0664-01-01 10:00] arabic digits"),
    ("timestamp_with_extra_bracket_text", "[2024-01-01 10:00 extra] tail"),
    ("unclosed_bracket", "[2024-01-01 10:00 hello"),
    ("nested_brackets", "[2024-01-01 10:00] a [b] c"),
    ("raw_uppercase_word", "[2024-01-01 10:00] [RAWX] not raw"),
    ("raw_lowercase", "[2024-01-01 10:00] [raw] lowercase raw marker"),
    ("raw_after_other_text", "[2024-01-01 10:00] prefix [RAW] later"),
    ("cr_inside_chunk", "[2024-01-01 10:00] a\rb"),
    ("blank_separator_before_plain_line", "one\n\ntwo\n[2024-01-01 10:00] three"),
    ("entry_after_plain_paragraph", "hello\n\n[2024-01-01 10:00] entry"),
]


def _char_class_cases() -> list[tuple[str, str]]:
    """Cases that only pass if the Go port widened `\\s` and `\\d` correctly.

    Go's `\\s` is ASCII-only and Go 1.23's `\\p{Nd}` predates Unicode 16.0, so
    both need the reference's exact sets. Comparing the final entries over these
    inputs is the only way the Go side can check that from outside the package:
    a narrow class changes the CHUNKING, so the difference is observable.
    """
    import re as _re

    space = _re.compile(r"\s")
    out: list[tuple[str, str]] = []

    # `\s*` after the closing bracket of a timestamp.
    for cp in range(0x110000):
        ch = chr(cp)
        if space.match(ch):
            out.append((
                f"ws_after_timestamp_u{cp:04x}",
                f"[2024-01-01 10:00]{ch}hello",
            ))

    # `\s+` between the bracket and the role, and `\s*` inside `[tools: ...]`,
    # in the pattern that decides whether a [RAW] chunk absorbs the next line.
    out.append((
        "ws_before_role_in_raw_chunk",
        "[2024-01-01 10:00] [RAW] first\n[2024-01-02 11:00]\x1cUSER: absorbed",
    ))
    out.append((
        "ws_before_role_ascii_in_raw_chunk",
        "[2024-01-01 10:00] [RAW] first\n[2024-01-02 11:00] USER: absorbed",
    ))
    out.append((
        "ws_inside_tools_marker_in_raw_chunk",
        "[2024-01-01 10:00] [RAW] first\n"
        "[2024-01-02 11:00] ASSISTANT\x1c[tools:\x1cread]: absorbed",
    ))
    out.append((
        "nbsp_before_role_in_raw_chunk",
        "[2024-01-01 10:00] [RAW] first\n[2024-01-02 11:00]\u00a0USER: absorbed",
    ))

    # `\d` must accept Unicode category Nd. The seven blocks below are Nd in the
    # reference interpreter's Unicode 16.0.0 and NOT in Go 1.23's 15.0.0, so
    # these are the cases that would regress if the class were left as \p{Nd}.
    for base in (
        0x10D40, 0x116D0, 0x11BF0, 0x16130, 0x16D70, 0x1CCF0, 0x1E5F1,
    ):
        digits = "".join(chr(base + i) for i in range(4))
        out.append((
            f"unicode16_digits_u{base:05x}",
            f"[2024-01-01 10:00] first\n[{digits}-01-02 11:00] second",
        ))
    out.append((
        "arabic_indic_digits",
        "[\u0662\u0660\u0662\u0664-01-01 10:00] first\n"
        "[\u0662\u0660\u0662\u0665-01-02 11:00] second",
    ))
    out.append((
        "ascii_digits_control",
        "[2024-01-01 10:00] first\n[2025-01-02 11:00] second",
    ))
    return out


PARSE_CASES.extend(_char_class_cases())


def dump_parse_cases() -> list[dict]:
    """_parse_legacy_history (memory.py:145) plus the chunk split it performs.

    The chunk list is captured through the REAL code path: the store's
    _split_legacy_history_chunks is wrapped, so `normalized` is the exact string
    _parse_legacy_history computed on line 146 and `chunks` is exactly what the
    reference handed to the entry loop. Reproducing the normalisation here by
    hand would have tested the dumper instead of the reference.
    """
    workspace, store = _make_store_without_legacy()
    try:
        legacy = workspace / "memory" / "HISTORY.md"
        legacy.write_text("placeholder", encoding="utf-8")
        _pin_mtime(legacy)

        out = []
        for name, text in PARSE_CASES:
            captured: dict = {}
            original = store._split_legacy_history_chunks

            def spy(normalized, _captured=captured, _original=original):
                chunks = _original(normalized)
                _captured["normalized"] = normalized
                _captured["chunks"] = list(chunks)
                return chunks

            store._split_legacy_history_chunks = spy
            try:
                entries = store._parse_legacy_history(text)
            finally:
                del store.__dict__["_split_legacy_history_chunks"]

            out.append({
                "name": name,
                "text": text,
                "normalized": captured.get("normalized"),
                # An empty or whitespace-only input returns before the split is
                # reached, so the spy is never called. Reporting [] rather than
                # null keeps "no chunks" a value the Go side can compare.
                "chunks": captured.get("chunks", []),
                "entries": entries,
            })
        return out
    finally:
        shutil.rmtree(workspace, ignore_errors=True)


# ---------------------------------------------------------------------------
# _legacy_fallback_timestamp
# ---------------------------------------------------------------------------

# Each entry is the float handed to os.utime. The value actually stored is read
# back and reported, because that is what the reference will stat.
#
# 1/128 and 127/128 are dyadic, so x*1e6 is an exact half-integer and CPython's
# round-half-even on the microseconds is exercised on both parities. Neither
# changes the formatted string, but they are the only reachable exact ties.
FALLBACK_MTIMES: list[tuple[str, float]] = [
    ("exact_second", MTIME_EPOCH),
    ("half_second", MTIME_EPOCH + 0.5),
    ("tie_rounds_up", 1_600_000_019.9999995),
    ("just_below_tie", 1_600_000_019.9999994),
    ("tie_crosses_minute", 1_600_000_019.9999995),
    ("near_tie_crosses_minute", 1_600_000_019.9999999),
    ("microsecond", MTIME_EPOCH + 0.000001),
    ("exact_tie_rounds_down_even", MTIME_EPOCH + 1 / 128),
    ("exact_tie_rounds_up_odd", MTIME_EPOCH + 127 / 128),
]


def dump_fallback_timestamp() -> list[dict]:
    """_legacy_fallback_timestamp (memory.py:211) for pinned mtimes.

    The method formats the mtime with the process's local zone, so the Go test
    pins LEGACY_HISTORY_UTC_OFFSET_SECONDS to the offset Go's time.Local uses.
    `offset_seconds` records what the interpreter actually applied, which lets
    the Go side assert the pinning worked instead of trusting it.
    """
    workspace, store = _make_store_without_legacy()
    try:
        legacy = workspace / "memory" / "HISTORY.md"
        legacy.write_text("placeholder", encoding="utf-8")

        out = []
        for name, epoch in FALLBACK_MTIMES:
            mtime_ns = _pin_mtime(legacy, epoch)
            expected = store._legacy_fallback_timestamp()
            offset = datetime.fromtimestamp(mtime_ns / 1e9).astimezone().utcoffset()
            out.append({
                "name": name,
                "mtime_ns": mtime_ns,
                "expected": expected,
                "offset_seconds": int(offset.total_seconds()),
            })
        return out
    finally:
        shutil.rmtree(workspace, ignore_errors=True)


# ---------------------------------------------------------------------------
# _next_legacy_backup_path
# ---------------------------------------------------------------------------

def dump_backup_path() -> list[dict]:
    """_next_legacy_backup_path (memory.py:219) with 0, 1 and 2 backups present."""
    workspace, store = _make_store_without_legacy()
    try:
        memory_dir = workspace / "memory"
        out = []
        for existing in range(3):
            for stale in list(memory_dir.glob("HISTORY.md.bak*")):
                stale.unlink()
            for i in range(existing):
                name = "HISTORY.md.bak" if i == 0 else f"HISTORY.md.bak.{i + 1}"
                (memory_dir / name).write_text(f"backup-{i}", encoding="utf-8")
            out.append({
                "existing": existing,
                "chosen": store._next_legacy_backup_path().name,
            })
        return out
    finally:
        shutil.rmtree(workspace, ignore_errors=True)


# ---------------------------------------------------------------------------
# _maybe_migrate_legacy_history
# ---------------------------------------------------------------------------

MIGRATION_CASES: list[dict] = [
    {"name": "legacy_missing"},
    {"name": "legacy_empty", "legacy_text": ""},
    {"name": "legacy_whitespace_only", "legacy_text": "  \n\t\n "},
    {"name": "single_entry", "legacy_text": "[2024-01-01 10:00] a\n"},
    {"name": "two_entries", "legacy_text": "[2024-01-01 10:00] a\n[2024-01-02 11:00] b\n"},
    {"name": "entry_without_timestamp", "legacy_text": "plain text with no stamp\n"},
    {"name": "mixed_timestamps_and_plain",
     "legacy_text": "[2024-01-01 10:00] a\nplain\n[2024-01-02 11:00] b\n"},
    {"name": "raw_chunk", "legacy_text": "[2024-01-01 10:00] [RAW] 2 messages\n[t] USER: hi\n"},
    {"name": "session_key_absent", "legacy_text": "[2024-01-01 10:00] a\n"},
    {"name": "history_already_nonempty",
     "legacy_text": "[2024-01-01 10:00] a\n", "pre_history": b'{"cursor": 9}\n'},
    {"name": "history_exists_but_empty",
     "legacy_text": "[2024-01-01 10:00] a\n", "pre_history": b""},
    {"name": "history_whitespace_only",
     "legacy_text": "[2024-01-01 10:00] a\n", "pre_history": b"\n"},
    {"name": "one_existing_backup", "legacy_text": "[2024-01-01 10:00] a\n", "backups": 1},
    {"name": "two_existing_backups", "legacy_text": "[2024-01-01 10:00] a\n", "backups": 2},
    {"name": "three_existing_backups", "legacy_text": "[2024-01-01 10:00] a\n", "backups": 3},
    {"name": "legacy_is_a_directory"},
    {"name": "invalid_utf8_lone_bytes",
     "legacy_bytes": b"[2024-01-01 10:00] a\xff\xffb\n"},
    {"name": "invalid_utf8_truncated_prefix",
     "legacy_bytes": b"[2024-01-01 10:00] a\xf0\x9fb\n"},
    {"name": "invalid_utf8_surrogate",
     "legacy_bytes": b"[2024-01-01 10:00] a\xed\xa0\x80b\n"},
    {"name": "invalid_utf8_overlong",
     "legacy_bytes": b"[2024-01-01 10:00] a\xc0\x80b\n"},
    {"name": "invalid_utf8_out_of_range",
     "legacy_bytes": b"[2024-01-01 10:00] a\xf4\x90\x80\x80b\n"},
    {"name": "invalid_utf8_before_timestamp",
     "legacy_bytes": b"\xff[2024-01-01 10:00] a\n"},
    {"name": "utf8_bom_prefix", "legacy_bytes": b"\xef\xbb\xbf[2024-01-01 10:00] a\n"},
    {"name": "non_ascii_content",
     "legacy_text": "[2024-01-01 10:00] ol\u00e1 \u2014 caf\u00e9 \u4f60\u597d\n"},
    {"name": "content_with_quotes_and_backslash",
     "legacy_text": '[2024-01-01 10:00] say "hi" \\ bye\n'},
    {"name": "content_with_control_chars",
     "legacy_text": "[2024-01-01 10:00] a\x01b\n"},
    {"name": "content_with_line_separator",
     "legacy_text": "[2024-01-01 10:00] a\u2028b\n"},
]


def dump_migration() -> list[dict]:
    """_maybe_migrate_legacy_history (memory.py:106) end to end.

    Every scenario records the exact INPUT it wrote and the whole memory/
    directory afterwards, plus the bytes of history.jsonl and the two cursor
    files. That is the only way to pin the parts that are easy to get wrong:
    which files exist at all (an empty legacy file still produces a backup but
    no history.jsonl), the exact JSON separators _write_entries emits, and the
    fact that the dream cursor is advanced to the last migrated cursor.
    """
    out = []
    for case in MIGRATION_CASES:
        workspace = _scratch("legacy-migrate")
        try:
            memory_dir = workspace / "memory"
            memory_dir.mkdir()

            legacy_bytes = case.get("legacy_bytes")
            if legacy_bytes is None and "legacy_text" in case:
                legacy_bytes = case["legacy_text"].encode("utf-8")
            legacy_is_dir = case["name"] == "legacy_is_a_directory"

            if legacy_is_dir:
                (memory_dir / "HISTORY.md").mkdir()
            elif legacy_bytes is not None:
                (memory_dir / "HISTORY.md").write_bytes(legacy_bytes)

            legacy_mtime_ns = None
            if (memory_dir / "HISTORY.md").is_file():
                legacy_mtime_ns = _pin_mtime(memory_dir / "HISTORY.md")

            if "pre_history" in case:
                (memory_dir / "history.jsonl").write_bytes(case["pre_history"])

            backups_written: dict[str, str] = {}
            for i in range(case.get("backups", 0)):
                name = "HISTORY.md.bak" if i == 0 else f"HISTORY.md.bak.{i + 1}"
                payload = f"backup-{i}".encode("utf-8")
                (memory_dir / name).write_bytes(payload)
                backups_written[name] = payload.hex()

            before = sorted(p.name for p in memory_dir.iterdir())
            _store_class()(workspace)
            after = sorted(p.name for p in memory_dir.iterdir())

            def read_hex(path: Path) -> str | None:
                return path.read_bytes().hex() if path.exists() else None

            backups_after = {}
            for path in sorted(memory_dir.glob("HISTORY.md*")):
                if path.is_file():
                    backups_after[path.name] = path.read_bytes().hex()

            out.append({
                "name": case["name"],
                "inputs": {
                    "legacy_bytes_hex": legacy_bytes.hex() if legacy_bytes is not None else None,
                    "legacy_is_dir": legacy_is_dir,
                    "legacy_mtime_ns": legacy_mtime_ns,
                    "pre_history_hex": case.get("pre_history", b"").hex()
                    if "pre_history" in case
                    else None,
                    "backups_hex": backups_written,
                },
                "before": before,
                "after": after,
                "history_bytes_hex": read_hex(memory_dir / "history.jsonl"),
                "cursor": read_hex(memory_dir / ".cursor"),
                "dream_cursor": read_hex(memory_dir / ".dream_cursor"),
                "backups_after": backups_after,
            })
        finally:
            shutil.rmtree(workspace, ignore_errors=True)
    return out


# ---------------------------------------------------------------------------
# Whitespace and digit classes the three regexes depend on
# ---------------------------------------------------------------------------

def dump_char_classes() -> dict:
    """The exact code points Python's `\\s` and `\\d` match in a str pattern.

    Go's `\\s` is ASCII-only and its `\\p{Nd}` follows Go's own Unicode tables,
    so neither is a drop-in replacement. Enumerating the reference interpreter's
    sets here is what lets the Go side hard-code them instead of guessing.
    """
    import re as _re

    space = _re.compile(r"\s")
    digit = _re.compile(r"\d")

    def ranges(pred) -> list[list[int]]:
        out: list[list[int]] = []
        for cp in range(0x110000):
            if pred(chr(cp)):
                if out and cp == out[-1][1] + 1:
                    out[-1][1] = cp
                else:
                    out.append([cp, cp])
        return out

    space_ranges = ranges(lambda ch: bool(space.match(ch)))
    digit_ranges = ranges(lambda ch: bool(digit.match(ch)))
    return {
        "space_ranges": space_ranges,
        "digit_ranges": digit_ranges,
        "space_equals_str_isspace": space_ranges == ranges(lambda ch: ch.isspace()),
    }


def main() -> int:
    doc = {
        "upstream_commit": "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9",
        "mtime_epoch": MTIME_EPOCH,
        # The same instant as an exact integer. MTIME_EPOCH is integral, so this
        # conversion is lossless; the Go side needs an integer because a JSON
        # float64 cannot hold a nanosecond timestamp near 1.6e18.
        "mtime_ns": int(MTIME_EPOCH * 1e9),
        "char_classes": dump_char_classes(),
        "parse_cases": dump_parse_cases(),
        "fallback_timestamp": dump_fallback_timestamp(),
        "backup_path": dump_backup_path(),
        "migration": dump_migration(),
    }
    json.dump(doc, sys.stdout, indent=2, sort_keys=True, ensure_ascii=False)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
