package filediff

// Port of the Python string primitives the diff machinery depends on.
//
// internal/gitstore/pytext.go contains an equivalent implementation, but that
// package is owned by another workstream and importing it would couple the diff
// machinery to the git object store, so the handful of functions needed here is
// written out again.

// pySplitLines mirrors str.splitlines() with no argument.
//
// Python splits on ten code points, with "\r\n" counting as a single break:
// "\n" U+000A, "\r" U+000D, "\v" U+000B, "\f" U+000C, U+001C, U+001D, U+001E,
// U+0085 NEL, U+2028 LINE SEPARATOR and U+2029 PARAGRAPH SEPARATOR.
// strings.Split(s, "\n") is a different function: it keeps "\r", splits on
// nothing else, and produces a trailing empty element for a trailing newline.
//
// This is load-bearing for FileDiff.from_text: the added/deleted counts are
// line counts, so a file using U+2028 as a line break must count the same way
// in both implementations.
func pySplitLines(s string) []string {
	var out []string
	start := 0
	i := 0
	for i < len(s) {
		width := 0
		switch c := s[i]; {
		case c == '\n' || c == '\v' || c == '\f' || c == 0x1c || c == 0x1d || c == 0x1e:
			width = 1
		case c == '\r':
			width = 1
			if i+1 < len(s) && s[i+1] == '\n' {
				width = 2
			}
		case c == 0xc2 && i+1 < len(s) && s[i+1] == 0x85:
			// UTF-8 encoding of U+0085 NEL.
			width = 2
		case c == 0xe2 && i+2 < len(s) && s[i+1] == 0x80 && (s[i+2] == 0xa8 || s[i+2] == 0xa9):
			// UTF-8 encoding of U+2028 / U+2029.
			width = 3
		}
		if width == 0 {
			i++
			continue
		}
		out = append(out, s[start:i])
		i += width
		start = i
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
