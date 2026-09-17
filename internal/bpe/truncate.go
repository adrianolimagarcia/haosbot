package bpe

// TruncatedSuffix is nanobot.utils.helpers._TRUNCATED_SUFFIX
// (helpers.py:371). It is a token-costed suffix, not a plain marker: the
// reference measures it with the encoder before spending budget on the body.
const TruncatedSuffix = "\n... (truncated)"

// TruncateTextToTokens is nanobot.utils.helpers.truncate_text_to_tokens
// (helpers.py:409).
//
// It is NOT textutil.TruncateText, which is the CHARACTER-based
// truncate_text (helpers.py:402). This one counts real tokens, so the cap holds
// for CJK and code as well as for ASCII prose.
//
// The search is the reference's: it decodes successively shorter prefixes of
// the token sequence until the prefix plus the suffix fits, because decoding a
// token prefix can produce fewer characters than expected (a token boundary is
// not a character boundary) and the re-encoded result is what actually has to
// fit.
//
// When the encoder cannot be used — a special token appears in the input, which
// makes tiktoken's default `disallowed_special="all"` raise — the reference
// falls back to a UTF-8 byte budget. That fallback is kept, because dropping it
// would turn a conservative truncation into an unbounded one.
func TruncateTextToTokens(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return text
	}
	v, err := getVocab()
	if err != nil {
		return truncateByBytes(text, maxTokens)
	}

	tokens := v.encodeOrdinary(text)
	if findSpecialToken(text) != "" {
		// enc.encode(text) raises before any of the work below happens.
		return truncateByBytes(text, maxTokens)
	}
	if len(tokens) <= maxTokens {
		return text
	}
	if findSpecialToken(TruncatedSuffix) != "" {
		return truncateByBytes(text, maxTokens)
	}
	suffixTokens := v.encodeOrdinary(TruncatedSuffix)
	bodyBudget := maxTokens - len(suffixTokens)
	if bodyBudget <= 0 {
		return Decode(tokens[:maxTokens])
	}
	for candidateBudget := bodyBudget; candidateBudget >= 0; candidateBudget-- {
		result := Decode(tokens[:candidateBudget]) + TruncatedSuffix
		// enc.encode(result) can itself raise, and in the reference that
		// exception escapes the whole try block into the byte fallback.
		if findSpecialToken(result) != "" {
			return truncateByBytes(text, maxTokens)
		}
		if n := len(v.encodeOrdinary(result)); n <= maxTokens {
			return result
		}
	}
	return Decode(tokens[:maxTokens])
}

// truncateByBytes is the reference's `except Exception` branch.
func truncateByBytes(text string, maxTokens int) string {
	if len(text) <= maxTokens {
		return text
	}
	suffixBytes := len(TruncatedSuffix)
	if maxTokens <= suffixBytes {
		return truncateToUTF8Bytes(text, maxTokens)
	}
	return truncateToUTF8Bytes(text, maxTokens-suffixBytes) + TruncatedSuffix
}

// truncateToUTF8Bytes is _truncate_text_to_utf8_bytes (helpers.py:442): the
// longest code-point prefix of text within maxBytes bytes, decoded with
// errors="ignore".
//
// Python encodes the str, slices the BYTES, then decodes ignoring errors. For
// valid UTF-8 that is the same as backing up to the last character boundary;
// for a Go string holding invalid bytes it also drops the ill-formed tail, so
// the WHATWG error-advance rule is reused from decodeUTF8Replace.
func truncateToUTF8Bytes(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	return decodeUTF8Ignore([]byte(text)[:maxBytes])
}
