#!/usr/bin/env python3
"""Regenerate the embedded cl100k_base vocabulary blob and Unicode class tables.

This is a build-time code generator, NOT part of the Go build. It is committed
so the two generated artifacts in ../ can be reproduced byte-for-byte.

    .tools/venv/bin/python internal/bpe/gen/generate_vocab.py

Outputs (both written next to the package, i.e. internal/bpe/):

  cl100k_base.bin     the 100_256 mergeable ranks as a compact binary blob
  unicode_tables.go   the \\p{L} and \\p{N} code point ranges

WHY A GENERATOR AND NOT A CHECKED-IN DATA STRUCTURE
---------------------------------------------------
The upstream vocabulary lives at

    https://openaipublic.blob.core.windows.net/encodings/cl100k_base.tiktoken

which `tiktoken` downloads and caches under
`$TIKTOKEN_CACHE_DIR` (default `<tempdir>/data-gym-cache`) as
`sha1(blobpath).hexdigest()`. For cl100k_base that key is
`9b5ad71b2ce5302211f9c61530b329a4922fc6a4`. Pass that file as argv[1] to avoid
needing the network; otherwise this script asks `tiktoken` to fetch it.

The raw artifact is 1_681_126 bytes of base64 text ("<b64 token> <rank>\\n" per
line). The blob written here is the same information in binary form:

    offset 0   magic   "NBPE"                4 bytes
    offset 4   version uint32 LE  (= 1)      4 bytes
    offset 8   n       uint32 LE             4 bytes   number of ranks
    offset 12  maxLen  uint32 LE             4 bytes   longest token, in bytes
    offset 16  lens    n x uint8                       length of rank i
    offset ..  bytes   concatenation of rank 0..n-1 token bytes

744_102 bytes for cl100k_base (16 + 100_256 + 643_830). Embedding the base64
text instead would cost 1.68 MB of binary and force a base64 decode at startup;
a 2.0 MB JSON dump would cost 3x more still.

WHY THE UNICODE TABLES ARE ENUMERATED AND NOT TAKEN FROM Go's unicode PACKAGE
-----------------------------------------------------------------------------
The pre-tokenizer regex is compiled by tiktoken's Rust core, so its \\p{L} and
\\p{N} come from the Rust `regex` crate's Unicode tables, which are NEWER than
the ones Go 1.23.5 ships. Measured on this machine:

    Go 1.23.5 unicode.Version = 15.0.0
    \\p{L}: Go 136_104 code points, tiktoken 141_028   (Go is a strict subset)
    \\p{N}: Go   1_831 code points, tiktoken   1_911   (Go is a strict subset)

so Go's tables cannot be used directly. This script enumerates the reference's
own tables by probing tiktoken itself (see `enumerate_class`), never by reading
a Unicode data file, so the tables cannot drift from the reference.

`\\s` is NOT tabulated: it was enumerated the same way and found to be exactly
the 25 code points of Unicode's White_Space property, which is exactly what Go's
`unicode.IsSpace` reports. That equality is asserted at the end of this script
and re-asserted by the Go test suite (TestSpaceClassMatchesReference).

`(?i:...)` case folding is NOT tabulated either. The reference's `(?i)` uses
simple case folding; enumerating `(?i:[sdmt])`, `(?i:ll)`, `(?i:ve)` and
`(?i:re)` position by position yields the small sets hard-coded in
../scanner.go, and this script prints them so a future tiktoken change is
visible.
"""

from __future__ import annotations

import base64
import hashlib
import os
import struct
import sys

import tiktoken
from tiktoken import Encoding

HERE = os.path.dirname(os.path.abspath(__file__))
PKG = os.path.dirname(HERE)

CL100K_URL = "https://openaipublic.blob.core.windows.net/encodings/cl100k_base.tiktoken"
MAGIC = b"NBPE"
VERSION = 1

# All code points except the surrogate range. Python strings cannot hold lone
# surrogates in a way that tiktoken can encode, and no Unicode scalar value
# lives there anyway.
ALL_CODE_POINTS = [c for c in range(0x110000) if not (0xD800 <= c <= 0xDFFF)]


# ---------------------------------------------------------------------------
# Vocabulary
# ---------------------------------------------------------------------------


def read_raw_ranks(path: str) -> dict[int, bytes]:
    """Parse the upstream "<base64 token> <rank>" artifact into rank -> bytes."""
    out: dict[int, bytes] = {}
    with open(path, "rb") as fh:
        for lineno, line in enumerate(fh, 1):
            line = line.strip()
            if not line:
                continue
            b64, _, rank = line.partition(b" ")
            if not rank:
                sys.exit("malformed line %d in %s" % (lineno, path))
            out[int(rank)] = base64.b64decode(b64)
    return out


def load_ranks(argv: list[str]) -> dict[int, bytes]:
    if len(argv) > 1:
        return read_raw_ranks(argv[1])

    # Ask tiktoken for the encoding, which populates its own cache, then read
    # the cache file back so we are building from the same artifact tiktoken
    # itself used rather than from tiktoken's in-memory dict.
    import tempfile

    tiktoken.get_encoding("cl100k_base")
    cache_dir = os.environ.get("TIKTOKEN_CACHE_DIR") or os.environ.get(
        "DATA_GYM_CACHE_DIR"
    ) or os.path.join(tempfile.gettempdir(), "data-gym-cache")
    cache_path = os.path.join(cache_dir, hashlib.sha1(CL100K_URL.encode()).hexdigest())
    if not os.path.exists(cache_path):
        sys.exit(
            "no cached cl100k_base artifact at %s; download %s or pass its path "
            "as argv[1]" % (cache_path, CL100K_URL)
        )
    return read_raw_ranks(cache_path)


def build_blob(ranks: dict[int, bytes]) -> bytes:
    n = len(ranks)
    if sorted(ranks) != list(range(n)):
        sys.exit("ranks are not contiguous 0..n-1")
    lens = bytes(len(ranks[i]) for i in range(n))
    max_len = max(lens)
    if max_len > 255:
        sys.exit("token longer than 255 bytes; uint8 lengths would overflow")
    body = b"".join(ranks[i] for i in range(n))
    return (
        MAGIC
        + struct.pack("<III", VERSION, n, max_len)
        + lens
        + body
    )


# ---------------------------------------------------------------------------
# Unicode classes, enumerated from tiktoken's own regex engine
# ---------------------------------------------------------------------------


def probe_encoding(pattern: str) -> Encoding:
    """An Encoding whose BPE is the identity on single bytes.

    With `mergeable_ranks = {b: b for b in range(256)}` every matched piece
    encodes to exactly its own bytes, so `len(encode_ordinary(s))` is the total
    number of bytes of `s` that the pattern matched. That byte count is what
    `enumerate_class` bisects on.
    """
    return Encoding(
        name="probe",
        pat_str=pattern,
        mergeable_ranks={bytes([b]): b for b in range(256)},
        special_tokens={},
    )


def enumerate_class(
    pattern: str,
    wrap,
    width,
    label: str,
) -> list[tuple[int, int]]:
    """Return the code points matched by `pattern`, as sorted (lo, hi) ranges.

    `wrap(chunk)` must turn a list of code points into a probe string in which
    each code point's contribution to the match is independent of the others,
    and `width(c)` must be the maximum number of bytes that code point can
    contribute. A chunk is "all match" when the probe encodes to exactly
    `sum(width(c))` bytes, "none match" when it encodes to 0 bytes, and
    otherwise it is split in half and the two halves are probed again.

    This is exact rather than a sample: the recursion only descends into chunks
    that contain at least one matching and one non-matching code point, so every
    boundary in the class is found. It also costs O(transitions * log(0x110000))
    probes instead of one probe per code point -- ~9_000 probes for \\p{L}.
    """
    enc = probe_encoding(pattern)
    encode = enc.encode_ordinary
    matched: set[int] = set()
    stack = [ALL_CODE_POINTS]
    calls = 0
    while stack:
        chunk = stack.pop()
        got = len(encode(wrap(chunk)))
        calls += 1
        want = sum(width(c) for c in chunk)
        if got == 0:
            continue
        if got == want:
            matched.update(chunk)
            continue
        if len(chunk) == 1:
            raise AssertionError(
                "single code point %#x matched %d bytes, expected either 0 or %d"
                % (chunk[0], got, want)
            )
        mid = len(chunk) // 2
        stack.append(chunk[:mid])
        stack.append(chunk[mid:])

    ranges: list[tuple[int, int]] = []
    start = prev = None
    for c in ALL_CODE_POINTS:
        if c in matched:
            if start is None:
                start = c
            prev = c
        elif start is not None:
            ranges.append((start, prev))
            start = None
    if start is not None:
        ranges.append((start, prev))
    print(
        "  %-22s code points=%-7d ranges=%-4d probes=%d"
        % (label, len(matched), len(ranges), calls)
    )
    return ranges


def plain(chunk):
    return "".join(chr(c) for c in chunk)


def plain_width(c):
    return len(chr(c).encode("utf-8"))


def _paired(fmt):
    """Build (wrap, width) for a two-character pattern probe.

    The code points are separated by "\\n", which is in no case-fold class of
    any ASCII letter, so a match can never straddle two entries.
    """

    def wrap(chunk):
        return "\n".join(fmt(c) for c in chunk)

    def width(c):
        return len(fmt(c).encode("utf-8"))

    return wrap, width


def suffixed(suffix):
    """Probe `chr(c) + suffix`: enumerates the fold class of the FIRST letter."""
    return _paired(lambda c: chr(c) + suffix)


def prefixed(prefix):
    """Probe `prefix + chr(c)`: enumerates the fold class of the SECOND letter."""
    return _paired(lambda c: prefix + chr(c))


def members(ranges):
    return sorted(c for lo, hi in ranges for c in range(lo, hi + 1))


def go_ranges(name: str, ranges: list[tuple[int, int]], comment: str) -> str:
    lines = [
        "// %s holds the %d code point ranges of %s." % (name, len(ranges), comment),
        "//",
        "// Generated by gen/generate_vocab.py; do not edit by hand.",
        "var %s = [...]struct{ lo, hi rune }{" % name,
    ]
    for lo, hi in ranges:
        if lo == hi:
            lines.append("\t{0x%X, 0x%X}," % (lo, lo))
        else:
            lines.append("\t{0x%X, 0x%X}," % (lo, hi))
    lines.append("}")
    lines.append("")
    return "\n".join(lines)


def main() -> None:
    ranks = load_ranks(sys.argv)
    blob = build_blob(ranks)
    blob_path = os.path.join(PKG, "cl100k_base.bin")
    with open(blob_path, "wb") as fh:
        fh.write(blob)
    print(
        "wrote %s (%d bytes, n=%d, maxLen=%d, token bytes=%d)"
        % (
            blob_path,
            len(blob),
            len(ranks),
            max(len(v) for v in ranks.values()),
            sum(len(v) for v in ranks.values()),
        )
    )

    print("enumerating character classes from tiktoken's Rust regex:")
    space = enumerate_class(r"\s", plain, plain_width, r"\s")
    letters = enumerate_class(r"\p{L}", plain, plain_width, r"\p{L}")
    numbers = enumerate_class(r"\p{N}", plain, plain_width, r"\p{N}")

    print("enumerating (?i:) case-fold classes:")
    fold_sdmt = enumerate_class(r"(?i:[sdmt])", plain, plain_width, r"(?i:[sdmt])")
    fold_l = enumerate_class(r"(?i:ll)", *suffixed("l"), "fold('l')")
    fold_v = enumerate_class(r"(?i:ve)", *suffixed("e"), "fold('v')")
    fold_r = enumerate_class(r"(?i:re)", *suffixed("e"), "fold('r')")
    fold_e = enumerate_class(r"(?i:ve)", *prefixed("v"), "fold('e')")
    fold_e2 = enumerate_class(r"(?i:re)", *prefixed("r"), "fold('e') via re")
    folds = (
        ("fold('sdtm')", fold_sdmt, [0x44, 0x4D, 0x53, 0x54, 0x64, 0x6D, 0x73, 0x74, 0x17F]),
        ("fold('l')", fold_l, [0x4C, 0x6C]),
        ("fold('v')", fold_v, [0x56, 0x76]),
        ("fold('r')", fold_r, [0x52, 0x72]),
        ("fold('e')", fold_e, [0x45, 0x65]),
        ("fold('e') via re", fold_e2, [0x45, 0x65]),
    )
    for label, rng, want in folds:
        got = members(rng)
        print("    %s = %s" % (label, " ".join("U+%04X" % c for c in got)))
        assert got == want, "%s changed: %r != %r" % (label, got, want)

    assert space == [
        (0x9, 0xD),
        (0x20, 0x20),
        (0x85, 0x85),
        (0xA0, 0xA0),
        (0x1680, 0x1680),
        (0x2000, 0x200A),
        (0x2028, 0x2029),
        (0x202F, 0x202F),
        (0x205F, 0x205F),
        (0x3000, 0x3000),
    ], "\\s is no longer Unicode White_Space; scanner.go must stop using unicode.IsSpace"

    src = [
        "package bpe",
        "",
        "// Unicode character classes for the cl100k_base pre-tokenizer.",
        "//",
        "// The reference compiles its pre-tokenizer with tiktoken's Rust core, so",
        "// these are the Rust `regex` crate's tables, not Go's. Go 1.23.5 ships",
        "// Unicode 15.0.0, which is a strict SUBSET of what the reference matches",
        "// (see the package doc and gen/generate_vocab.py). They are therefore",
        "// embedded rather than taken from the unicode package.",
        "//",
        "// Do not edit: regenerate with gen/generate_vocab.py.",
        "//",
        "// Code generated by gen/generate_vocab.py. DO NOT EDIT.",
        "",
        go_ranges(
            "letterRanges",
            letters,
            r"\p{L} as implemented by tiktoken's Rust regex engine",
        ),
        go_ranges(
            "numberRanges",
            numbers,
            r"\p{N} as implemented by tiktoken's Rust regex engine",
        ),
    ]
    out = os.path.join(PKG, "unicode_tables.go")
    with open(out, "w") as fh:
        fh.write("\n".join(src))
    print("wrote %s (%d bytes)" % (out, os.path.getsize(out)))


if __name__ == "__main__":
    main()
