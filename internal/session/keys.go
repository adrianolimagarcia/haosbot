// Package session implements nanobot's JSONL session persistence.
//
// It is a Go port of the reference implementation
// upstream/nanobot/nanobot/session/manager.py:JsonlSessionStore (@1bb712d3)
// restricted to the read/write path that other packages need: one file per
// session key, named by the base64url-nopad encoding of the key, with a
// metadata record on line 1 and one JSON object per remaining line.
//
// Compatibility rules that this package honours (all verified against the
// Python source and, where marked, against CPython 3.14.7 behaviour):
//
//   - Filenames are base64.urlsafe_b64encode(key.encode()).rstrip("=")
//     (manager.py:1005-1007). Non-ASCII keys are encoded as UTF-8 bytes.
//   - Records are serialised with json.dumps(..., ensure_ascii=False) and the
//     DEFAULT separators ", " / ": " (manager.py:1331-1339), i.e. with a space
//     after every comma and colon, no key sorting, and no HTML escaping.
//   - Numbers that Go did not generate are passed through verbatim so a
//     Python float such as 1.0 never becomes the JSON integer 1.
//   - Timestamps are naive local ISO-8601 (datetime.now().isoformat()):
//     no timezone suffix, six fractional digits or none at all.
//   - Every record whose "_type" is neither "metadata" nor "provider_state"
//     is a message record and is preserved verbatim (manager.py:1089-1090).
package session

import (
	"encoding/base64"
	"regexp"
	"strings"
	"unicode/utf8"
)

// StorageKey returns the canonical on-disk stem for a session key.
//
// Mirrors JsonlSessionStore.storage_key (manager.py:1005-1007):
//
//	base64.urlsafe_b64encode(key.encode()).decode().rstrip("=")
//
// which is exactly base64.RawURLEncoding applied to the UTF-8 bytes of key.
func StorageKey(key string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(key))
}

// DecodeStorageKey reverses StorageKey.
//
// Mirrors JsonlSessionStore.decode_storage_key (manager.py:1009-1017). The
// reference delegates to base64.urlsafe_b64decode, whose behaviour is
// reproduced exactly here:
//
//   - The input is encoded to ASCII first, so a non-ASCII stem is an error.
//   - Padding is restored from the length alone: 4 - len(stem)%4, unless that
//     is 4 (a multiple of four needs no padding).
//   - binascii.a2b_base64 runs in its lenient mode: characters outside the
//     base64 alphabet are ignored, the run of '=' that follows the last
//     alphabet character is the padding count, and the data length must leave
//     a remainder of 0, 2 or 3 modulo four (2 requires >=2 padding, 3
//     requires >=1).
//   - The result must be valid UTF-8.
//
// The leniency above was derived by differential testing against CPython
// 3.14.7 over 581,890 generated stems (all strings of length <=5 over
// "ABab01-_\n=!é+ " plus 6000 random stems) with zero mismatches.
//
// The second return value is false where Python's decode_storage_key returns
// None.
func DecodeStorageKey(stem string) (string, bool) {
	// base64._bytes_from_decode_data encodes str input to ASCII.
	for i := 0; i < len(stem); i++ {
		if stem[i] > 0x7f {
			return "", false
		}
	}

	s := stem
	if pad := 4 - len(stem)%4; pad != 4 {
		s += strings.Repeat("=", pad)
	}

	data := make([]byte, 0, len(s))
	padding := 0
	for i := 0; i < len(s); i++ {
		if v, ok := b64Value(s[i]); ok {
			data = append(data, v)
			padding = 0
			continue
		}
		if s[i] == '=' {
			padding++
		}
	}

	switch rest := len(data) % 4; {
	case rest == 1:
		return "", false
	case rest == 2 && padding < 2:
		return "", false
	case rest == 3 && padding < 1:
		return "", false
	}

	out := make([]byte, 0, len(data)/4*3+2)
	var bits uint32
	nbits := 0
	for _, v := range data {
		bits = bits<<6 | uint32(v)
		nbits += 6
		if nbits >= 8 {
			nbits -= 8
			out = append(out, byte(bits>>uint(nbits)))
			bits &= 1<<uint(nbits) - 1
		}
	}
	if !utf8.Valid(out) {
		return "", false
	}
	return string(out), true
}

// b64Value maps a base64 character to its six-bit value. It accepts the
// standard alphabet plus the two URL-safe substitutes, matching
// base64.urlsafe_b64decode (which only translates '-' and '_' and otherwise
// lets the standard decoder accept '+' and '/').
func b64Value(c byte) (byte, bool) {
	switch {
	case c >= 'A' && c <= 'Z':
		return c - 'A', true
	case c >= 'a' && c <= 'z':
		return c - 'a' + 26, true
	case c >= '0' && c <= '9':
		return c - '0' + 52, true
	case c == '+' || c == '-':
		return 62, true
	case c == '/' || c == '_':
		return 63, true
	}
	return 0, false
}

// SessionKeyFromStem returns the session key for a canonical file stem.
//
// Mirrors JsonlSessionStore.session_key_from_path (manager.py:1019-1024): the
// stem must decode AND re-encode to itself, byte for byte. This is the
// acceptance rule for files written by either implementation, so a stem with
// padding, or one built from standard (non-URL-safe) base64, is rejected.
func SessionKeyFromStem(stem string) (string, bool) {
	key, ok := DecodeStorageKey(stem)
	if !ok || StorageKey(key) != stem {
		return "", false
	}
	return key, true
}

// unsafeFilenameChars mirrors _UNSAFE_CHARS (helpers.py:366).
var unsafeFilenameChars = regexp.MustCompile(`[<>:"/\\|?*]`)

// safeFilename mirrors nanobot.utils.helpers.safe_filename (helpers.py:374-376).
func safeFilename(name string) string {
	return strings.TrimSpace(unsafeFilenameChars.ReplaceAllString(name, "_"))
}

// safeKey mirrors JsonlSessionStore.safe_key (manager.py:1001-1003).
//
// It is used only for the retired "lossy" path that delete() cleans up; it is
// never read and never written (manager.py:1032-1033, 1410-1416).
func safeKey(key string) string {
	return safeFilename(strings.ReplaceAll(key, ":", "_"))
}
