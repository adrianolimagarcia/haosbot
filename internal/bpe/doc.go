// Package bpe implements OpenAI's cl100k_base byte-pair encoding.
//
// It exists because nanobot's context governance decides when to compact a
// conversation by counting prompt tokens, and the reference counts them with
// tiktoken's cl100k_base encoder. The port previously substituted a UTF-8 byte
// count, which over-estimates English prose by roughly 5x and therefore
// compacted roughly 5x too early.
//
// The package has no dependencies outside the standard library. The 100_256
// mergeable ranks are embedded in the binary as a 744_102-byte blob and are
// decoded lazily, so a process that never estimates a prompt never builds the
// encoder and never touches those pages.
//
// Symbol map (Go -> reference)
//
//	Go                                  Python / tiktoken / Rust
//	----------------------------------  --------------------------------------------
//	Encode                              Encoding.encode(text)   (defaults)
//	EncodeOrdinary                      Encoding.encode_ordinary(text)
//	Decode                              Encoding.decode(tokens)
//	DecodeBytes                         Encoding.decode_bytes(tokens)
//	SpecialTokenError                   ValueError from raise_disallowed_special_token
//	ScanPieces                          regex.find_iter(text) in _encode_ordinary_native
//	encodePiece / mergeParts            _byte_pair_encode / _byte_pair_merge
//	TokenCount                          len(enc.encode(text))
//	TruncateTextToTokens                nanobot.utils.helpers.truncate_text_to_tokens
//	TruncatedSuffix                     nanobot.utils.helpers._TRUNCATED_SUFFIX
//	vocabulary.rank                     CoreBPE.encoder: HashMap<Vec<u8>, Rank>
//	vocabulary.token                    CoreBPE.decoder: HashMap<Rank, Vec<u8>>
//	isLetter / isNumber                 \p{L} / \p{N} in the Rust `regex` crate
//	isSpace                             \s in the Rust `regex` crate
//
// What is reproduced exactly, and how that was established
//
//   - The pre-tokenizer is a hand-written scanner, not a regexp. The reference
//     pattern uses possessive quantifiers (`?+`, `++`, `*+`, `{1,3}+`), a
//     negative lookahead `(?!\S)`, and `\p{L}`/`\p{N}`. Go's RE2-based regexp
//     supports none of those, so scanner.go reimplements the alternation
//     branch by branch, in the reference's order.
//
//   - The character classes are NOT Go's unicode tables. tiktoken compiles the
//     pattern in its Rust core, whose Unicode tables are newer than Go 1.23.5's
//     Unicode 15.0.0. Measured: \p{L} has 141_028 code points in the reference
//     and 136_104 in Go (Go is a strict subset), \p{N} 1_911 vs 1_831. The
//     reference's tables are embedded in unicode_tables.go, enumerated from
//     tiktoken itself by gen/generate_vocab.py. \s turned out to be exactly the
//     25 code points of Unicode's White_Space property, i.e. exactly Go's
//     unicode.IsSpace, and is used directly.
//
//   - `(?i:...)` uses Unicode simple case folding, and the folded class of 's'
//     contains U+017F (LATIN SMALL LETTER LONG S). So "ſs" pre-tokenizes as
//     ["ſ", "s"] and not as one piece. The four fold classes are enumerated
//     from the reference and hard-coded in scanner.go.
//
//   - Decode performs Python's `bytes.decode("utf-8", errors="replace")`, whose
//     "maximal subpart" rule differs from Go's utf8.DecodeRune for a truncated
//     but well-formed prefix: b"\xe4\xb8" is one U+FFFD in Python and two in
//     Go. utf8replace.go implements the WHATWG/Python rule.
//
//   - Encode reproduces tiktoken's default `disallowed_special="all"`: it
//     returns a *SpecialTokenError if the text contains any of the five special
//     token strings. nanobot relies on that exception: every call site uses the
//     default, and the `except Exception` in helpers.py sends such text to the
//     byte heuristic instead. EncodeOrdinary is the variant that does not
//     check, matching `encode(text, disallowed_special=())`.
//
// Deliberate extensions (states the reference cannot reach)
//
//   - Go strings may hold invalid UTF-8; Python `str` cannot. The scanner
//     classifies an invalid byte as U+FFFD (not a letter, not a number, not
//     whitespace, so it lands in the `[^\s\p{L}\p{N}]++` branch) and emits the
//     original byte unchanged. Token IDs for invalid input therefore have no
//     reference counterpart, but DecodeBytes(EncodeOrdinary(s)) == []byte(s)
//     still holds.
//
//   - Decode ignores token IDs outside the vocabulary instead of panicking the
//     way the Rust `unwrap()` does. No encoder path can produce such an ID.
package bpe
