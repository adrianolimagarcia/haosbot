#!/usr/bin/env python3
"""Dump cl100k_base reference values for the Go BPE port.

This is one half of a differential harness. It executes the REAL tiktoken
(0.14.0) in the project venv and emits a single JSON document on stdout; the Go
half (compat/bpe_differential_test.go) reads it and compares against
internal/bpe. Nothing here is transcribed from documentation or from a spec —
every value is what the installed tiktoken actually produces.

Run with the project venv:
    .tools/venv/bin/python compat/python/dump_bpe.py

The corpus has five parts:

  cases              curated + seeded-fuzz strings, with the FULL token ID
                     sequence for encode_ordinary and for encode()
  decode_cases       token prefixes of the above, through Encoding.decode
  byte_decode_cases  arbitrary byte strings expressed as single-byte tokens,
                     which pins Python's utf-8 errors="replace" rule
  class_cases        per-code-point membership in \\s, \\p{L}, \\p{N}
  sweeps             a sha256 over the token IDs of four strings that between
                     them contain EVERY Unicode scalar value, so the whole code
                     point space is covered without shipping 1.1M cases
  truncate_cases     nanobot.utils.helpers.truncate_text_to_tokens
  estimate_cases     nanobot.utils.helpers._estimate_prompt_tokens_with_source
                     and estimate_message_tokens

The sweeps matter because Go 1.23.5 ships Unicode 15.0.0 while tiktoken's Rust
regex crate ships a newer set: \\p{L} has 141_028 code points in the reference
and 136_104 in Go. A sample would not prove the embedded tables complete; the
sweeps do.
"""
from __future__ import annotations

import base64
import hashlib
import json
import random
import struct
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
UPSTREAM = ROOT / "upstream" / "nanobot"
sys.path.insert(0, str(UPSTREAM))

# nanobot's helpers module imports loguru and friends; silence loguru so the
# harness emits exactly one JSON document on stdout.
try:
    from loguru import logger as _loguru_logger

    _loguru_logger.remove()
except Exception:
    pass

import tiktoken  # noqa: E402
from nanobot.utils.helpers import (  # noqa: E402
    _estimate_prompt_tokens_with_source,
    estimate_message_tokens,
    truncate_text_to_tokens,
)

ENC = tiktoken.get_encoding("cl100k_base")

# Every Unicode scalar value. Surrogates are excluded because no Python str can
# hold one in a form tiktoken accepts.
ALL_CODE_POINTS = [c for c in range(0x110000) if not (0xD800 <= c <= 0xDFFF)]

FUZZ_SEED = 49408  # 0xC100; fixed so runs are reproducible
FUZZ_CASES = 3000


# ---------------------------------------------------------------------------
# Corpus
# ---------------------------------------------------------------------------

CURATED = [
    # --- the pre-tokenizer's branch-1 ground truth -------------------------
    "don't",
    "don",
    "'t",
    "we've",
    "I'll",
    "he'd",
    "she's",
    "they're",
    "it's",
    "I'm",
    "can't",
    "won't",
    "DON'T",
    "D'ON'T",
    "'s",
    "'S",
    "'ll",
    "'LL",
    "'lL",
    "'ve",
    "'VE",
    "'re",
    "'RE",
    "'d",
    "'D",
    "'m",
    "'M",
    "'t",
    "'T",
    "'x",
    "'",
    "''",
    "'''",
    "'l",
    "'v",
    "'r",
    "\u017f",
    "\u017fs",
    "s\u017f",
    "\u017f\u017f",
    "'\u017f",
    "'\u017f\u017f",
    # --- whitespace runs and end-of-input ----------------------------------
    "a   ",
    "a   b",
    "  \n",
    "  hello",
    " ",
    "  ",
    "   ",
    "a ",
    "a  ",
    "a \n",
    "a\n",
    "a\r",
    "a\r\n",
    "a\r\nb",
    "a\n\n",
    "a\n\n\n",
    "\n",
    "\n\n",
    "\r\n",
    "\r\n\r\n",
    " \n ",
    "  \n  ",
    " \t ",
    "\t\t\t",
    "\v\f",
    "a\vb",
    "a\fb",
    "a\x1cb",
    "a\x1db",
    "a\x1eb",
    "a\x1fb",
    "\x1c",
    "\x1d",
    "\x1e",
    "\x1f",
    "\x85",
    "a\x85b",
    "\xa0",
    "x\u00a0y",
    "\xa0y",
    "x\xa0",
    "\u1680",
    "\u2000\u2001",
    "\u2028",
    "\u2029",
    "\u202f",
    "\u205f",
    "\u3000",
    "a\u3000b",
    "\u3000a",
    "a\u3000",
    "\u200b",
    "\ufeff",
    "\u180e",
    # --- digits ------------------------------------------------------------
    "1",
    "12",
    "123",
    "1234",
    "12345",
    "1234567",
    "0",
    "007",
    "\u0660\u0661\u0662\u0663",
    "\u00b2\u00b3",
    "\u00bc",
    "\u2160",
    "\u3007",
    "a1",
    "1a",
    "a1b",
    "1a2b3",
    "1 2 3",
    "x\u00b2",
    "x\u2460",
    # --- mixed punctuation -------------------------------------------------
    "hello world",
    "hello, world!",
    "{}",
    "[]",
    "(a)",
    "a-b",
    "a_b",
    "a.b",
    "!!!",
    "!!!a",
    " a!",
    "  a!",
    "!a",
    " ?",
    "  ?",
    "? ",
    "?  ",
    "\r\n!",
    "!\r\n",
    "!  \r\n",
    "http://example.com/a?b=c&d=e",
    "<|endoftext|>",
    # --- code --------------------------------------------------------------
    "def f(x):\n    return x*2\n",
    "func main() {\n\tfmt.Println(\"hi\")\n}\n",
    "SELECT * FROM t WHERE a = 1;",
    '{"a": 1, "b": [true, null], "c": "\\u00e9"}',
    "  if (a && b) { return; }",
    "x" * 7 + " = " + "y" * 11,
    "a" * 99,
    "a" * 100,
    "a" * 101,
    "a" * 200,
    "a" * 1000,
    " " * 99,
    " " * 100,
    " " * 101,
    " " * 300,
    "\n" * 200,
    "\t" * 150,
    "ab" * 60,
    "the " * 40,
    "x" * 100 + "'t",
    # --- CJK, emoji, combining, RTL ----------------------------------------
    "\u4f60\u597d",
    "\u4f60\u597d\u4e16\u754c",
    "\u3053\u3093\u306b\u3061\u306f",
    "\ud55c\uad6d\uc5b4",
    "\U0001F600",
    "\U0001F600\U0001F601",
    "\U0001F469\u200d\U0001F4BB",
    "\U0001F468\u200d\U0001F469\u200d\U0001F467",
    "\U0001F44D\U0001F3FD",
    "\U0001F1E7\U0001F1F7",
    "e\u0301",
    "a\u0300\u0301\u0302",
    "\u05e9\u05dc\u05d5\u05dd",
    "\u0645\u0631\u062d\u0628\u0627",
    "\u0627\u0644\u0639\u0631\u0628\u064a\u0629",
    "\u0e01\u0e33",
    "\u0915\u094d\u0937",
    "a\u200d",
    "\u200d",
    "\u0301",
    "caf\u00e9",
    "\u00e9\u00e8\u00ea",
    "\U0001D400\U0001D401",
    "\u2764\ufe0f",
    "1\ufe0f\u20e3",
    # --- control / latin-1 sweep -------------------------------------------
    "".join(chr(c) for c in range(0x20)),
    "".join(chr(c) for c in range(0xA0, 0x100)),
    "".join(chr(c) for c in range(0x2000, 0x2070)),
]

FUZZ_ALPHABET = (
    "abcdefghijklmnopqrstuvwxyz"
    "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
    "0123456789"
    " \t\n\r\v\f"
    "'\".,;:!?-_()[]{}<>/\\|@#$%^&*+=~`"
    "\u00a0\u3000\u2028\u2029\u0085\u001c\u001d\u001e\u001f\u200b\ufeff"
    "\u4f60\u597d\u4e16\u754c\u3042\u30a2\uac00"
    "\u00e9\u00fc\u00df\u0301\u0302"
    "\U0001F600\U0001F469\u200d\U0001F4BB\U0001F3FD"
    "\u017f"
)


def fuzz_corpus() -> list[str]:
    rng = random.Random(FUZZ_SEED)
    out: list[str] = []
    for _ in range(FUZZ_CASES):
        n = rng.randint(1, 48)
        out.append("".join(rng.choice(FUZZ_ALPHABET) for _ in range(n)))
    # A second family biased towards whitespace and boundaries, where the
    # branch-5/6/7 interaction lives.
    boundary = " \t\n\r\v\f\u00a0\u3000'abcdefg0112345.,!"
    for _ in range(FUZZ_CASES // 2):
        n = rng.randint(1, 24)
        out.append("".join(rng.choice(boundary) for _ in range(n)))
    # Random scalar values, so the \p{L}/\p{N} tables are exercised away from
    # the sweeps as well.
    for _ in range(FUZZ_CASES // 4):
        n = rng.randint(1, 12)
        out.append(
            "".join(chr(rng.choice(ALL_CODE_POINTS)) for _ in range(n))
        )
    return out


def corpus() -> list[str]:
    seen: dict[str, None] = {}
    for s in CURATED:
        seen.setdefault(s, None)
    for s in fuzz_corpus():
        seen.setdefault(s, None)
    return list(seen)


# ---------------------------------------------------------------------------
# Sweeps
# ---------------------------------------------------------------------------


def sweep_inputs() -> list[tuple[str, str]]:
    """Four strings that between them contain every Unicode scalar value."""
    joined = "".join(chr(c) for c in ALL_CODE_POINTS)
    return [
        ("bare", joined),
        ("space-prefixed", "".join(" " + chr(c) for c in ALL_CODE_POINTS)),
        ("letter-prefixed", "".join("a" + chr(c) for c in ALL_CODE_POINTS)),
        ("digit-suffixed", "".join(chr(c) + "1" for c in ALL_CODE_POINTS)),
    ]


def token_digest(tokens: list[int]) -> str:
    h = hashlib.sha256()
    h.update(struct.pack("<Q", len(tokens)))
    for t in tokens:
        h.update(struct.pack("<I", t))
    return h.hexdigest()


# ---------------------------------------------------------------------------
# Assembly
# ---------------------------------------------------------------------------


def build_cases(texts: list[str]) -> list[dict]:
    out = []
    for s in texts:
        ordinary = ENC.encode_ordinary(s)
        entry = {"text": s, "ordinary": ordinary}
        try:
            entry["encode"] = ENC.encode(s)
            entry["encode_error"] = None
        except Exception as exc:  # noqa: BLE001 - the reference raises ValueError
            entry["encode"] = None
            entry["encode_error"] = type(exc).__name__ + ": " + str(exc)
        out.append(entry)
    return out


def build_decode_cases(texts: list[str]) -> list[dict]:
    out = []
    for s in texts:
        tokens = ENC.encode_ordinary(s)
        if not tokens:
            continue
        # Every cut point, capped: a split character boundary is exactly what
        # the "replace" rule is about.
        cuts = sorted({0, len(tokens), 1, len(tokens) // 3, len(tokens) // 2,
                       2 * len(tokens) // 3, len(tokens) - 1})
        for k in cuts:
            if 0 <= k <= len(tokens):
                prefix = tokens[:k]
                out.append({"tokens": prefix, "text": ENC.decode(prefix)})
    return out


BYTE_DECODE_SAMPLES = [
    b"",
    b"a",
    b"\xe4\xb8",
    b"\xe4",
    b"\xf0\x9f\x98",
    b"\xf0\x9f",
    b"\xf0",
    b"\xc3",
    b"\x80",
    b"\x80\x80",
    b"a\xe4\xb8",
    b"\xed\xa0\x80",
    b"\xf4\x90\x80\x80",
    b"\xe4\xb8\xad",
    b"\xff\xfe",
    b"\xc0\x80",
    b"\xe4\xb8\x41",
    b"\xe0\x80\x80",
    b"\xe0\xa0\x80",
    b"\xf0\x80\x80\x80",
    b"\xf0\x90\x80\x80",
    b"\xf5\x80\x80\x80",
    b"\xfe",
    b"\xbf",
    b"\xc2\xa0",
    b"a\xc2\xa0b",
    b"\xe4\xb8\xad\xe6\x96\x87",
    b"\xe4\xb8\xad\xe6",
    b"\xf0\x9f\x98\x80\xf0\x9f",
    bytes(range(0x80, 0x100)),
    bytes(range(0x00, 0x80)),
    bytes(range(256)),
]


def build_byte_decode_cases() -> list[dict]:
    out = []
    for b in BYTE_DECODE_SAMPLES:
        # encode_single_token is tiktoken's bytes-valued entry point; every
        # single byte is a rank in cl100k_base, so this is total.
        tokens = [ENC.encode_single_token(bytes([x])) for x in b]
        out.append(
            {
                "bytes_b64": base64.b64encode(b).decode(),
                "tokens": tokens,
                "text": ENC.decode(tokens),
            }
        )
    return out


CLASS_SAMPLE_STEP = 97  # coprime with the code point count, so it walks widely


def build_class_cases() -> list[dict]:
    cps = set(ALL_CODE_POINTS[::CLASS_SAMPLE_STEP])
    cps.update(range(0, 0x300))
    cps.update(range(0x2000, 0x2100))
    cps.update(range(0xFF00, 0xFFF0))
    cps.update(range(0x1F300, 0x1F400))
    cps.update({0x30000, 0x31350, 0x323AF, 0x2EBF0, 0x2EE5D, 0x1E4D0, 0x11F00})
    out = []
    for cp in sorted(cps):
        ch = chr(cp)
        out.append(
            {
                "cp": cp,
                "space": _is_single_class(ch, r"\s"),
                "L": _is_single_class(ch, r"\p{L}"),
                "N": _is_single_class(ch, r"\p{N}"),
            }
        )
    return out


_PROBE_CACHE: dict[str, "tiktoken.Encoding"] = {}


def _probe(pattern: str) -> "tiktoken.Encoding":
    """An encoding whose BPE is the identity on single bytes.

    With these ranks a matched piece contributes exactly its own bytes, so the
    encoded length is the number of matched BYTES. Comparing that against the
    code point's UTF-8 length decides membership in a single-character class.
    """
    if pattern not in _PROBE_CACHE:
        _PROBE_CACHE[pattern] = tiktoken.Encoding(
            name="probe",
            pat_str=pattern,
            mergeable_ranks={bytes([b]): b for b in range(256)},
            special_tokens={},
        )
    return _PROBE_CACHE[pattern]


def _is_single_class(ch: str, pattern: str) -> bool:
    return len(_probe(pattern).encode_ordinary(ch)) == len(ch.encode("utf-8"))


TRUNCATE_TEXTS = [
    "",
    "a",
    "hello world",
    "hello world, this is a longer sentence that will need truncating",
    "\u4f60\u597d\u4e16\u754c" * 40,
    "def f(x):\n    return x * 2\n" * 30,
    " " * 200,
    "a   " * 60,
    "don't " * 50,
    "\U0001F600" * 100,
    "\u00e9" * 200,
    "<|endoftext|>",
    "prefix <|endoftext|> suffix",
    "x" * 500,
]
TRUNCATE_LIMITS = [0, 1, 2, 3, 4, 5, 8, 16, 32, 64, 100, 256, -1]


def build_truncate_cases() -> list[dict]:
    out = []
    for text in TRUNCATE_TEXTS:
        for limit in TRUNCATE_LIMITS:
            out.append(
                {
                    "text": text,
                    "max_tokens": limit,
                    "out": truncate_text_to_tokens(text, limit),
                }
            )
    return out


ESTIMATE_MESSAGES = [
    [],
    [{"role": "user", "content": "hi"}],
    [{"role": "user", "content": "hello world"}],
    [{"role": "system", "content": "You are a helpful assistant."},
     {"role": "user", "content": "What is 2+2?"}],
    [{"role": "user", "content": "a" * 500}],
    [{"role": "user", "content": [{"type": "text", "text": "part one"},
                                  {"type": "text", "text": "part two"}]}],
    [{"role": "user", "content": [{"type": "image_url", "image_url": {"url": "x"}}]}],
    [{"role": "tool", "content": "result", "tool_call_id": "call_1"}],
    [{"role": "assistant", "content": "", "reasoning_content": "thinking..."}],
    [{"role": "user", "content": "x", "name": "alice"}],
    [{"role": "user", "content": "x" * 100, "name": "bob", "tool_call_id": "call_2",
      "reasoning_content": "y" * 50}],
    [{"role": "user", "content": "line1\nline2\nline3"}],
    [{"role": "user", "content": "m%d" % i} for i in range(12)],
    [{"role": "user", "content": "<|endoftext|>"}],
    [{"role": "user", "content": "1"}],
    [{"role": "user", "content": "don't"}],
]


def build_estimate_cases() -> list[dict]:
    out = []
    for messages in ESTIMATE_MESSAGES:
        estimated, source = _estimate_prompt_tokens_with_source(messages, None)
        out.append(
            {
                "messages": messages,
                "tools": None,
                "tokens": estimated,
                "source": source,
                "message_tokens": [estimate_message_tokens(m) for m in messages],
            }
        )
    return out


def main() -> None:
    texts = corpus()
    doc = {
        "upstream_commit": "1bb712d3488915ca4ed9ccc1a93067ff722f5ab9",
        "tiktoken_version": tiktoken.__version__,
        "pat_str": ENC._pat_str,
        "n_vocab": ENC.n_vocab,
        "n_mergeable_ranks": len(ENC._mergeable_ranks),
        "special_tokens": ENC._special_tokens,
        "fuzz_seed": FUZZ_SEED,
        "n_corpus_texts": len(texts),
        "cases": build_cases(texts),
        "decode_cases": build_decode_cases(texts),
        "byte_decode_cases": build_byte_decode_cases(),
        "class_cases": build_class_cases(),
        "sweeps": [
            {
                "name": name,
                "input_sha256": hashlib.sha256(s.encode()).hexdigest(),
                "input_bytes": len(s.encode("utf-8")),
                "n_tokens": len(toks),
                "tokens_sha256": token_digest(toks),
            }
            for name, s in sweep_inputs()
            for toks in [ENC.encode_ordinary(s)]
        ],
        "truncate_cases": build_truncate_cases(),
        "estimate_cases": build_estimate_cases(),
    }
    json.dump(doc, sys.stdout, ensure_ascii=False)


if __name__ == "__main__":
    main()
