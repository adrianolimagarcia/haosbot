package skills

// A focused YAML subset parser.
//
// WHY THIS EXISTS
//
// The reference parses skill frontmatter with `yaml.safe_load`
// (agent/skills.py:31). The port has zero external Go dependencies, so PyYAML
// cannot be reused, and the task explicitly forbids pulling in a YAML library.
// This file implements the subset of YAML that skill frontmatter actually uses
// — block mappings, block sequences, flow collections, quoted and plain
// scalars, block scalars, comments and PyYAML's implicit scalar typing — plus
// enough strictness that constructs it does not model are reported as ERRORS
// rather than silently mis-parsed. That direction matters: an error makes
// parse_skill_metadata return None, which is what the reference does for
// malformed frontmatter, whereas a wrong parse would produce a wrong
// description or a wrong availability verdict in the prompt.
//
// WHAT IT DELIBERATELY DOES NOT SUPPORT (each is an error, never a guess):
// tags (`!!str`), anchors/aliases (`&a`, `*a`), merge keys (`<<`), complex keys
// (`? `), explicit document end markers with trailing content, and multi-line
// quoted scalars. All of these are unreachable from the bundled skills and from
// any frontmatter the project's fixtures exercise; see
// compat/skills_differential_test.go, which probes a corpus of adversarial
// frontmatter against the real yaml.safe_load and FAILS on any disagreement.

import (
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// yamlError is a frontmatter body that yaml.safe_load would reject with a
// yaml.YAMLError. parse_skill_metadata turns it into None.
type yamlError struct{ msg string }

func (e *yamlError) Error() string { return "yaml: " + e.msg }

func yamlErr(format string, args ...any) error {
	return &yamlError{msg: fmt.Sprintf(format, args...)}
}

// --------------------------------------------------------------------------
// scalar resolution (PyYAML's implicit resolvers)
// --------------------------------------------------------------------------

// pyYAML resolves an untagged scalar by testing these patterns in order:
// bool, float, int, null (resolver.py: Resolver.add_implicit_resolver calls).
// The first-character sets are disjoint enough that the order only matters for
// the empty scalar, which is null.

func isYAMLNull(s string) bool {
	switch s {
	case "", "~", "null", "Null", "NULL":
		return true
	}
	return false
}

func isYAMLBool(s string) bool {
	switch s {
	case "yes", "Yes", "YES", "no", "No", "NO",
		"true", "True", "TRUE", "false", "False", "FALSE",
		"on", "On", "ON", "off", "Off", "OFF":
		return true
	}
	return false
}

// isYAMLInt implements PyYAML's int resolver, including the base-2/base-8/
// base-16 forms and the sexagesimal (base-60) form, which is YAML 1.1 and still
// accepted by PyYAML.
func isYAMLInt(s string) bool {
	body, ok := stripYAMLSign(s)
	if !ok || body == "" {
		return false
	}
	switch {
	case strings.HasPrefix(body, "0b"):
		return allOf(body[2:], isBinaryDigit) && len(body) > 2
	case strings.HasPrefix(body, "0x"):
		return allOf(body[2:], isHexDigit) && len(body) > 2
	case body[0] == '0' && len(body) == 1:
		// PyYAML's decimal branch is `(?:0|[1-9][0-9_]*)`, so a lone "0" — and
		// its signed forms "+0"/"-0", which stripYAMLSign has already reduced to
		// "0" — is a decimal int. Without this case the value fell through to the
		// string branch and "zero: 0" parsed as the string "0".
		return true
	case body[0] == '0' && len(body) > 1:
		if allOf(body[1:], isOctalDigit) {
			return true
		}
		// PyYAML's decimal int pattern `[0-9_]+` also matches "0_", "0_7".
		return allOf(body, isDigitOrUnderscore)
	case body[0] >= '1' && body[0] <= '9':
		// Either a plain decimal, or sexagesimal "1:30:00".
		if i := strings.IndexByte(body, ':'); i >= 0 {
			return isSexagesimal(body)
		}
		return allOf(body, isDigitOrUnderscore)
	}
	return false
}

// isYAMLFloat implements PyYAML's float resolver.
func isYAMLFloat(s string) bool {
	if s == "" {
		return false
	}
	switch s {
	case ".inf", ".Inf", ".INF", "+.inf", "+.Inf", "+.INF", "-.inf", "-.Inf", "-.INF",
		".nan", ".NaN", ".NAN":
		return true
	}
	body, ok := stripYAMLSign(s)
	if !ok || body == "" {
		return false
	}
	// Split off an exponent.
	mantissa := body
	exp := ""
	if i := strings.IndexAny(body, "eE"); i >= 0 {
		exp = body[i+1:]
		mantissa = body[:i]
		// PyYAML's float exponent pattern requires an explicit sign:
		// `[eE][-+][0-9]+`. "1e5" is therefore NOT a float to PyYAML; it is a
		// string.
		if exp == "" || (exp[0] != '+' && exp[0] != '-') {
			return false
		}
		if !allOf(exp[1:], isDecimalDigit) || len(exp) < 2 {
			return false
		}
	}
	if strings.HasPrefix(mantissa, ".") {
		return len(mantissa) > 1 && allOf(mantissa[1:], isDigitOrUnderscore)
	}
	if i := strings.IndexByte(mantissa, ':'); i >= 0 {
		// Sexagesimal float: PyYAML's
		// `[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*`. The colon groups come FIRST
		// and the '.' separates the fraction, so "1:30.5" is a float while
		// "1:30" is an INT — which isYAMLInt resolves to 90. Classifying
		// "1:30" as a float here made it 90.0 where the reference reports 90.
		dot := strings.IndexByte(mantissa, '.')
		if dot < 0 || dot < i {
			return false
		}
		if !isSexagesimalFloat(mantissa[:dot]) {
			return false
		}
		return allOf(mantissa[dot+1:], isDigitOrUnderscore)
	}
	dot := strings.IndexByte(mantissa, '.')
	if dot < 0 {
		return false
	}
	intPart, fracPart := mantissa[:dot], mantissa[dot+1:]
	if intPart == "" {
		return false
	}
	if !allOf(intPart, isDigitOrUnderscore) {
		return false
	}
	return allOf(fracPart, isDigitOrUnderscore)
}

// isSexagesimal checks PyYAML's INT sexagesimal shape
// `[1-9][0-9_]*(:[0-5]?[0-9])+`: the leading component may not start with '0',
// so "0:30" stays a string.
func isSexagesimal(s string) bool { return sexagesimalShape(s, false) }

// isSexagesimalFloat checks PyYAML's FLOAT sexagesimal shape
// `[0-9][0-9_]*(:[0-5]?[0-9])+`, which does allow a leading zero.
func isSexagesimalFloat(s string) bool { return sexagesimalShape(s, true) }

func sexagesimalShape(s string, allowLeadingZero bool) bool {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || parts[0] == "" {
		return false
	}
	if !allowLeadingZero && (parts[0][0] < '1' || parts[0][0] > '9') {
		return false
	}
	if !allOf(parts[0], isDigitOrUnderscore) {
		return false
	}
	for _, p := range parts[1:] {
		if p == "" || len(p) > 2 || !allOf(p, isDecimalDigit) {
			return false
		}
		// Each group is `[0-5]?[0-9]`, so "60" and "99" are not components.
		if len(p) == 2 && (p[0] < '0' || p[0] > '5') {
			return false
		}
	}
	return true
}

func stripYAMLSign(s string) (string, bool) {
	if s == "" {
		return s, true
	}
	if s[0] == '+' || s[0] == '-' {
		return s[1:], true
	}
	return s, true
}

func allOf(s string, pred func(byte) bool) bool {
	for i := 0; i < len(s); i++ {
		if !pred(s[i]) {
			return false
		}
	}
	return true
}

func isDecimalDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isDigitOrUnderscore(c byte) bool { return isDecimalDigit(c) || c == '_' }
func isOctalDigit(c byte) bool        { return c >= '0' && c <= '7' }
func isBinaryDigit(c byte) bool       { return c == '0' || c == '1' }
func isHexDigit(c byte) bool {
	return isDecimalDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// resolveScalar applies PyYAML's implicit typing to an untagged plain scalar.
func resolveScalar(s string) any {
	switch {
	case isYAMLBool(s):
		return s == "yes" || s == "Yes" || s == "YES" ||
			s == "true" || s == "True" || s == "TRUE" ||
			s == "on" || s == "On" || s == "ON"
	case isYAMLFloat(s):
		return parseYAMLFloat(s)
	case isYAMLInt(s):
		return parseYAMLInt(s)
	case isYAMLNull(s):
		return nil
	}
	return s
}

func parseYAMLFloat(s string) any {
	switch s {
	case ".inf", ".Inf", ".INF", "+.inf", "+.Inf", "+.INF":
		return math.Inf(1)
	case "-.inf", "-.Inf", "-.INF":
		return math.Inf(-1)
	case ".nan", ".NaN", ".NAN":
		return math.NaN()
	}
	// Sexagesimal floats are converted to a plain decimal first.
	if i := strings.IndexByte(s, ':'); i >= 0 {
		neg := strings.HasPrefix(s, "-")
		body := strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
		parts := strings.Split(body, ":")
		total := 0.0
		for _, p := range parts {
			v, err := strconv.ParseFloat(strings.ReplaceAll(p, "_", ""), 64)
			if err != nil {
				return s
			}
			total = total*60 + v
		}
		if neg {
			total = -total
		}
		return total
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
	if err != nil {
		return s
	}
	return v
}

func parseYAMLInt(s string) any {
	neg := strings.HasPrefix(s, "-")
	body := strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	body = strings.ReplaceAll(body, "_", "")
	var v int64
	var err error
	switch {
	case strings.HasPrefix(body, "0b"):
		v, err = strconv.ParseInt(body[2:], 2, 64)
	case strings.HasPrefix(body, "0x"):
		v, err = strconv.ParseInt(body[2:], 16, 64)
	case len(body) > 1 && body[0] == '0':
		v, err = strconv.ParseInt(body[1:], 8, 64)
	case strings.Contains(body, ":"):
		var total int64
		for _, p := range strings.Split(body, ":") {
			n, e := strconv.ParseInt(p, 10, 64)
			if e != nil {
				return s
			}
			total = total*60 + n
		}
		v = total
	default:
		v, err = strconv.ParseInt(body, 10, 64)
	}
	if err != nil {
		return s
	}
	if neg {
		v = -v
	}
	return v
}

// --------------------------------------------------------------------------
// line model
// --------------------------------------------------------------------------

type yamlLine struct {
	num         int    // 1-based, for diagnostics
	indent      int    // number of leading spaces
	text        string // content after the indentation
	blank       bool   // empty or comment-only
	tabbed      bool   // leading whitespace contains a tab (fatal to PyYAML)
	trailingTab bool   // a tab was trimmed from the end (also fatal to PyYAML)
	// breakAfter reports whether a line break terminated this line in the
	// source. It is false only for the final line of a source that does not end
	// with "\n", which is exactly the case block-scalar chomping needs: PyYAML
	// clips to a single trailing newline only when the block's last content line
	// was actually followed by a break.
	breakAfter bool
}

type yamlParser struct {
	lines   []yamlLine
	pos     int
	anchors map[string]any
}

// setAnchor records an anchor definition. PyYAML's composer registers the
// anchor BEFORE the node is constructed, so a recursive structure would be
// possible in principle; the subset parser only anchors complete scalar /
// collection values, which is what skill frontmatter can express.
func (p *yamlParser) setAnchor(name string, value any) {
	if p.anchors == nil {
		p.anchors = map[string]any{}
	}
	p.anchors[name] = value
}

// anchorValue resolves an alias. An undefined alias is a ComposerError to
// PyYAML, which makes parse_skill_metadata return None.
func (p *yamlParser) anchorValue(name string) (any, error) {
	if value, ok := p.anchors[name]; ok {
		return value, nil
	}
	return nil, yamlErr("found undefined alias %q", name)
}

// splitYAMLLines normalises line breaks the way Python's text-mode IO does
// (\r\n and lone \r both become \n) and splits into lines.
func splitYAMLLines(src string) []string {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	if src == "" {
		return nil
	}
	lines := strings.Split(src, "\n")
	// A trailing newline yields a final empty element; PyYAML does not treat it
	// as a content line, and dropping it keeps indentation checks simple.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

func newYAMLParser(src string) (*yamlParser, error) {
	normalized := strings.ReplaceAll(strings.ReplaceAll(src, "\r\n", "\n"), "\r", "\n")
	// splitYAMLLines drops the empty element a trailing newline produces, so the
	// fact that the source ended with a break has to be captured here or it is
	// lost — and block-scalar chomping depends on it.
	srcEndsWithBreak := strings.HasSuffix(normalized, "\n")
	raw := splitYAMLLines(src)
	p := &yamlParser{lines: make([]yamlLine, 0, len(raw))}
	for i, line := range raw {
		indent := 0
		for indent < len(line) && line[indent] == ' ' {
			indent++
		}
		text := line[indent:]
		trimmed := strings.TrimRight(text, " \t")
		// A tab in a line's LEADING whitespace is fatal to PyYAML ("found
		// character '\\t' that cannot start any token"), but a tab after the
		// value's colon ("description:\tTabbed value") is ordinary whitespace
		// and the value parses. The line is kept with tabbed set so the
		// decision can be made where the line is actually consumed: a tab-only
		// line inside a block scalar is content, not an error.
		tabbed := indent < len(line) && line[indent] == '\t'
		// TrimRight below removes a trailing tab, which would hide the scanner
		// error PyYAML raises for "a: 1\t" and 'a: "v"\t'.
		trailingTab := strings.TrimRight(text, " \t") != strings.TrimRight(text, " ")
		p.lines = append(p.lines, yamlLine{
			num:         i + 1,
			indent:      indent,
			text:        trimmed,
			blank:       trimmed == "" || strings.HasPrefix(trimmed, "#"),
			tabbed:      tabbed,
			trailingTab: trailingTab,
			breakAfter:  i < len(raw)-1 || srcEndsWithBreak,
		})
	}
	return p, nil
}

// tabIndentationError reports the tab-in-indentation scanner error for a line
// whose leading whitespace contains a tab. PyYAML rejects such a document with
// "found character '\\t' that cannot start any token".
func tabIndentationError(lineNum int) error {
	return yamlErr("line %d: found character '\\t' that cannot start any token", lineNum)
}

// tabOutsideQuotes reports whether text holds a tab PyYAML's scanner would
// reject.
//
// A tab is NOT ordinary separation whitespace in YAML: it is fatal everywhere
// except inside a quoted scalar or a comment. Verified against PyYAML 6.0.3 —
// "description:\tvalue", "description: a\tb", "a: [1,\t2]", "a: 1\t" and
// "-\tvalue" all raise ScannerError, while 'a: "x\ty"' and "a: 1 # x\ty" parse.
// Block-scalar CONTENT is exempt, but those lines are consumed by
// parseBlockScalar and never reach this check.
func tabOutsideQuotes(text string) bool {
	inSingle := false
	inDouble := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case inDouble:
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				inDouble = false
			}
		case inSingle:
			if c == '\'' {
				if i+1 < len(text) && text[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
		default:
			switch c {
			case '"':
				inDouble = true
			case '\'':
				inSingle = true
			case '#':
				// A '#' opens a comment at the start of the line or after
				// separation whitespace; the rest of the line is not scanned.
				if i == 0 || text[i-1] == ' ' || text[i-1] == '\t' {
					return false
				}
			case '\t':
				return true
			}
		}
	}
	return false
}

// checkLineTabs rejects the scanner error for a line that is about to be read as
// YAML tokens.
func checkLineTabs(ln yamlLine) error {
	if ln.tabbed || ln.trailingTab || tabOutsideQuotes(ln.text) {
		return tabIndentationError(ln.num)
	}
	return nil
}

func (p *yamlParser) skipBlank() {
	for p.pos < len(p.lines) && p.lines[p.pos].blank {
		p.pos++
	}
}

func (p *yamlParser) atEnd() bool {
	p.skipBlank()
	return p.pos >= len(p.lines)
}

// --------------------------------------------------------------------------
// entry point
// --------------------------------------------------------------------------

// yamlLoad parses a single YAML document. It mirrors yaml.safe_load: an empty
// stream yields nil (Python None), and a stream holding more than one document
// is an error.
func yamlLoad(src string) (any, error) {
	p, err := newYAMLParser(src)
	if err != nil {
		return nil, err
	}
	p.skipBlank()
	if p.atEnd() {
		return nil, nil
	}
	// An explicit document start marker is allowed.
	if p.lines[p.pos].text == "---" {
		p.pos++
	}
	if p.atEnd() {
		return nil, nil
	}
	indent := p.lines[p.pos].indent
	v, err := p.parseNode(indent)
	if err != nil {
		return nil, err
	}
	p.skipBlank()
	if !p.atEnd() && p.lines[p.pos].text == "..." {
		p.pos++
	}
	if !p.atEnd() {
		return nil, yamlErr("line %d: expected a single document in the stream", p.lines[p.pos].num)
	}
	return v, nil
}

// parseNode parses the block node that begins at the current line, which must
// be indented by exactly indent.
func (p *yamlParser) parseNode(indent int) (any, error) {
	p.skipBlank()
	if p.pos >= len(p.lines) {
		return nil, nil
	}
	ln := p.lines[p.pos]
	if err := checkLineTabs(ln); err != nil {
		return nil, err
	}
	if ln.indent < indent {
		return nil, nil
	}
	if ln.indent > indent {
		return nil, yamlErr("line %d: bad indentation", ln.num)
	}
	if isSequenceEntry(ln.text) {
		return p.parseSequence(indent)
	}
	// A bare scalar document ("null", "just a string"): no mapping key, no
	// sequence entry. PyYAML resolves it with the implicit resolvers
	// (yaml.safe_load("null") is None, "just a string" stays a string).
	if _, _, ok := splitMappingKey(ln.text); !ok {
		p.pos++
		return p.parseInlineScalar(ln.text, indent, ln.num)
	}
	return p.parseMapping(indent)
}

func isSequenceEntry(text string) bool {
	return text == "-" || strings.HasPrefix(text, "- ") || strings.HasPrefix(text, "-\t")
}

// --------------------------------------------------------------------------
// block mapping
// --------------------------------------------------------------------------

func (p *yamlParser) parseMapping(indent int) (any, error) {
	out := map[string]any{}
	// Merge-key bookkeeping. PyYAML's SafeConstructor.flatten_mapping applies
	// `<<` entries AFTER the explicit keys of the SAME mapping, so an explicit
	// key always wins over a merged one, and a later `<<` wins over an earlier
	// one (each merge is applied in document order onto the mapping built so
	// far, overwriting only keys no explicit entry has set since).
	merges := []map[string]any{}
	for {
		p.skipBlank()
		if p.pos >= len(p.lines) {
			break
		}
		ln := p.lines[p.pos]
		if err := checkLineTabs(ln); err != nil {
			return nil, err
		}
		if ln.indent < indent {
			break
		}
		if ln.indent > indent {
			return nil, yamlErr("line %d: bad indentation of a mapping entry", ln.num)
		}
		if isSequenceEntry(ln.text) {
			// A block sequence at the same indentation as its parent mapping is
			// legal only as the value of the immediately preceding key, which
			// parseValue already consumed.
			return nil, yamlErr("line %d: unexpected block sequence entry", ln.num)
		}
		key, rest, ok := splitMappingKey(ln.text)
		if !ok {
			return nil, yamlErr("line %d: could not find expected ':'", ln.num)
		}
		p.pos++
		value, err := p.parseValue(rest, indent, ln.num)
		if err != nil {
			return nil, err
		}
		if key == "<<" {
			merged, ok := value.(map[string]any)
			if !ok {
				return nil, yamlErr("line %d: the merge value must be a mapping", ln.num)
			}
			merges = append(merges, merged)
			continue
		}
		// PyYAML's SafeLoader silently keeps the last of duplicate keys.
		out[key] = value
	}
	applyYAMLMerges(out, merges)
	return out, nil
}

// applyYAMLMerges folds `<<` merge sources into the mapping. Explicit keys set
// BEFORE or AFTER the merge entry win; among the merge sources themselves the
// LAST one wins (PyYAML applies them in order onto the same mapping).
func applyYAMLMerges(out map[string]any, merges []map[string]any) {
	for _, merged := range merges {
		for key, value := range merged {
			if _, explicit := out[key]; !explicit {
				out[key] = value
			}
		}
	}
}

// splitMappingKey splits "key: value" into ("key", "value", true). Quoted keys
// and plain keys are both handled; a plain key cannot contain ": ".
func splitMappingKey(text string) (string, string, bool) {
	if text == "" {
		return "", "", false
	}
	if text[0] == '"' || text[0] == '\'' {
		key, n, err := scanQuoted(text, 0)
		if err != nil {
			return "", "", false
		}
		rest := strings.TrimLeft(text[n:], " ")
		if !strings.HasPrefix(rest, ":") {
			return "", "", false
		}
		return key, strings.TrimLeft(rest[1:], " "), true
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '#' && i > 0 && (text[i-1] == ' ' || text[i-1] == '\t') {
			return "", "", false
		}
		if c != ':' {
			continue
		}
		if i+1 == len(text) {
			return resolvePlainKey(text[:i]), "", true
		}
		if text[i+1] == ' ' || text[i+1] == '\t' {
			return resolvePlainKey(text[:i]), strings.TrimLeft(text[i+1:], " \t"), true
		}
	}
	return "", "", false
}

// resolvePlainKey applies YAML implicit typing to an unquoted mapping key and
// then Python's str(), mirroring parse_skill_metadata's
// `{str(key): value for key, value in parsed.items()}`. `true: x` therefore
// becomes the key "True" and `1: x` the key "1", not the raw source text.
func resolvePlainKey(raw string) string {
	return pyStr(resolveScalar(strings.TrimRight(raw, " \t")))
}

// parseValue parses the value part of a mapping entry. rest is the text after
// the key's colon (possibly empty); keyIndent is the indentation of the key
// line, used to discover a nested block node.
func (p *yamlParser) parseValue(rest string, keyIndent, lineNum int) (any, error) {
	rest = stripTrailingComment(rest)
	if rest == "" {
		// Nested block node, or nothing at all.
		p.skipBlank()
		if p.pos >= len(p.lines) {
			return nil, nil
		}
		next := p.lines[p.pos]
		if next.indent > keyIndent {
			return p.parseNode(next.indent)
		}
		if next.indent == keyIndent && isSequenceEntry(next.text) {
			return p.parseSequence(keyIndent)
		}
		return nil, nil
	}

	if rest[0] == '|' || rest[0] == '>' {
		return p.parseBlockScalar(rest, keyIndent)
	}
	if rest[0] == '{' || rest[0] == '[' {
		return p.parseFlowValue(rest, lineNum)
	}
	if rest[0] == '&' || rest[0] == '*' {
		return p.parseAnchoredValue(rest, keyIndent, lineNum)
	}
	if rest[0] == '!' {
		return p.parseTaggedValue(rest, keyIndent, lineNum)
	}
	if rest[0] == '"' || rest[0] == '\'' {
		s, n, err := scanQuoted(rest, 0)
		if err != nil {
			return nil, yamlErr("line %d: %s", lineNum, err.Error())
		}
		if tail := strings.TrimSpace(stripTrailingComment(rest[n:])); tail != "" {
			return nil, yamlErr("line %d: unexpected content after a quoted scalar", lineNum)
		}
		return s, nil
	}

	// Plain scalar, possibly folded over continuation lines.
	return p.parsePlainScalar(rest, keyIndent)
}

// parseAnchoredValue parses "&name value" (an anchored node) or "*name" (an
// alias). The anchor property may be followed by any node, including a block
// node on the following lines.
func (p *yamlParser) parseAnchoredValue(rest string, keyIndent, lineNum int) (any, error) {
	if rest[0] == '*' {
		name := strings.TrimSpace(stripTrailingComment(rest[1:]))
		if strings.ContainsAny(name, " \t") || name == "" {
			return nil, yamlErr("line %d: invalid alias %q", lineNum, rest)
		}
		return p.anchorValue(name)
	}
	// "&name ..." — split off the anchor name.
	body := rest[1:]
	end := 0
	for end < len(body) && body[end] != ' ' && body[end] != '\t' {
		end++
	}
	name := body[:end]
	if name == "" {
		return nil, yamlErr("line %d: an anchor requires a name", lineNum)
	}
	tail := strings.TrimLeft(body[end:], " \t")
	tail = stripTrailingComment(tail)
	var value any
	var err error
	if tail == "" {
		// The anchored node is a block node on the following lines.
		p.skipBlank()
		if p.pos < len(p.lines) && p.lines[p.pos].indent > keyIndent {
			value, err = p.parseNode(p.lines[p.pos].indent)
		} else {
			value = nil
		}
	} else {
		value, err = p.parseValue(tail, keyIndent, lineNum)
	}
	if err != nil {
		return nil, err
	}
	p.setAnchor(name, value)
	return value, nil
}

// parseTaggedValue parses "!!tag value". Only the tags PyYAML's SafeLoader
// resolves are accepted; an unknown tag is a ConstructorError to PyYAML, which
// makes parse_skill_metadata return None.
func (p *yamlParser) parseTaggedValue(rest string, keyIndent, lineNum int) (any, error) {
	end := 0
	for end < len(rest) && rest[end] != ' ' && rest[end] != '\t' {
		end++
	}
	tag := rest[:end]
	tail := strings.TrimLeft(rest[end:], " \t")
	tail = stripTrailingComment(tail)
	value, err := resolveTag(tag, tail, p, keyIndent, lineNum)
	if err != nil {
		return nil, err
	}
	return value, nil
}

// resolveTag applies a PyYAML SafeLoader tag to a scalar. Collection tags
// (!!seq/!!map) fall through to the ordinary value parser, which already
// produces lists and maps.
func resolveTag(tag, tail string, p *yamlParser, keyIndent, lineNum int) (any, error) {
	// The value part: a quoted scalar keeps its string-ness before the tag is
	// applied, a plain scalar is resolved with the implicit resolvers first
	// (PyYAML resolves the SCALAR, then the tag constructor converts it).
	var scalar any
	if tail == "" {
		scalar = nil
	} else if tail[0] == '"' || tail[0] == '\'' {
		s, n, err := scanQuoted(tail, 0)
		if err != nil {
			return nil, yamlErr("line %d: %s", lineNum, err.Error())
		}
		if t := strings.TrimSpace(stripTrailingComment(tail[n:])); t != "" {
			return nil, yamlErr("line %d: unexpected content after a quoted scalar", lineNum)
		}
		scalar = s
	} else if tail[0] == '{' || tail[0] == '[' {
		return p.parseFlowValue(tail, lineNum)
	} else {
		scalar = resolveScalar(tail)
	}
	switch tag {
	case "!!str":
		if scalar == nil {
			return "", nil
		}
		return pyStr(scalar), nil
	case "!!int":
		if s, ok := scalar.(string); ok {
			if v, err := strconv.ParseInt(strings.ReplaceAll(strings.TrimSpace(s), "_", ""), 0, 64); err == nil {
				return v, nil
			}
			return nil, yamlErr("line %d: !!int cannot resolve %q", lineNum, tail)
		}
		return scalar, nil
	case "!!float":
		if s, ok := scalar.(string); ok {
			if v, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(s), "_", ""), 64); err == nil {
				return v, nil
			}
			return nil, yamlErr("line %d: !!float cannot resolve %q", lineNum, tail)
		}
		if v, ok := scalar.(int64); ok {
			return float64(v), nil
		}
		return scalar, nil
	case "!!bool":
		if s, ok := scalar.(string); ok {
			if isYAMLBool(s) {
				return s == "yes" || s == "Yes" || s == "YES" ||
					s == "true" || s == "True" || s == "TRUE" ||
					s == "on" || s == "On" || s == "ON", nil
			}
			return nil, yamlErr("line %d: !!bool cannot resolve %q", lineNum, tail)
		}
		return scalar, nil
	case "!!null":
		return nil, nil
	case "!!binary":
		if s, ok := scalar.(string); ok {
			return decodeYAMLBinary(s, lineNum)
		}
		return nil, yamlErr("line %d: !!binary cannot resolve %q", lineNum, tail)
	case "!!timestamp":
		if s, ok := scalar.(string); ok {
			return parseYAMLTimestamp(s, lineNum)
		}
		return scalar, nil
	case "!!seq", "!!map":
		// The ordinary parser already produced the right shape; a scalar under
		// a collection tag is a type error to PyYAML.
		if tail == "" {
			if tag == "!!seq" {
				return []any{}, nil
			}
			return map[string]any{}, nil
		}
		return scalar, nil
	}
	return nil, yamlErr("line %d: could not determine a constructor for the tag %q", lineNum, tag)
}

// decodeYAMLBinary ports PyYAML's !!binary constructor: base64 decode, and the
// result is a Python bytes object, which the port represents as a string
// (parse_skill_metadata only str()s the KEYS, so the value keeps its type).
func decodeYAMLBinary(s string, lineNum int) (any, error) {
	clean := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
			return -1
		}
		return r
	}, s)
	data, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil, yamlErr("line %d: !!binary is not valid base64", lineNum)
	}
	return string(data), nil
}

// parseYAMLTimestamp ports PyYAML's !!timestamp constructor for the forms its
// regex accepts. The result is a datetime.date or datetime.datetime; the port
// represents both as their ISO strings, which is what the differential canon
// compares.
func parseYAMLTimestamp(s string, lineNum int) (any, error) {
	layouts := []string{
		"2006-01-02",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05.999999999",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			if layout == "2006-01-02" {
				return t.Format("2006-01-02"), nil
			}
			return t.Format("2006-01-02T15:04:05Z07:00"), nil
		}
	}
	return nil, yamlErr("line %d: !!timestamp cannot resolve %q", lineNum, s)
}

// parsePlainScalar consumes a plain scalar and any more-indented continuation
// lines, which YAML folds into the same scalar.
func (p *yamlParser) parsePlainScalar(first string, keyIndent int) (any, error) {
	var sb strings.Builder
	sb.WriteString(first)
	for {
		save := p.pos
		// Blank lines must be counted BEFORE skipping them: skipBlank swallows
		// them, and a blank line folds into a newline rather than a space
		// (verified against PyYAML: "a: first\n\n  second" -> "first\nsecond").
		blanks := 0
		for p.pos < len(p.lines) && p.lines[p.pos].blank {
			p.pos++
			blanks++
		}
		if p.pos >= len(p.lines) {
			p.pos = save
			break
		}
		ln := p.lines[p.pos]
		if ln.indent <= keyIndent {
			p.pos = save
			break
		}
		text := stripTrailingComment(ln.text)
		// A plain scalar cannot contain a mapping indicator, so a more-indented
		// "key: value" line is not a continuation. PyYAML rejects the document
		// ("mapping values are not allowed here", verified); leaving the line
		// unconsumed makes the enclosing parseMapping report the indentation
		// instead of silently folding it into the scalar.
		if _, _, isEntry := splitMappingKey(text); isEntry {
			p.pos = save
			break
		}
		if blanks > 0 {
			sb.WriteString(strings.Repeat("\n", blanks))
		} else {
			sb.WriteString(" ")
		}
		sb.WriteString(text)
		// Consume the continuation line. Without this the loop re-read the same
		// line forever, growing sb without bound (observed as a 22 GB
		// allocation on "description: first part\n  second part").
		p.pos++
	}
	// A multi-line plain scalar is a STRING to PyYAML: its implicit resolvers
	// only ever see single-line scalars, because a plain scalar that continues
	// onto the next line contains a line break, and none of the int/float/bool/
	// null patterns match text with a break in it. "always: 0\n  1" is
	// therefore "0 1", not the int 0 (verified against PyYAML).
	if sb.Len() > len(first) {
		return strings.TrimRight(sb.String(), " \t"), nil
	}
	return resolveScalar(strings.TrimRight(sb.String(), " \t")), nil
}

// --------------------------------------------------------------------------
// block sequence
// --------------------------------------------------------------------------

func (p *yamlParser) parseSequence(indent int) (any, error) {
	out := []any{}
	for {
		p.skipBlank()
		if p.pos >= len(p.lines) {
			break
		}
		ln := p.lines[p.pos]
		if err := checkLineTabs(ln); err != nil {
			return nil, err
		}
		if ln.indent != indent || !isSequenceEntry(ln.text) {
			if ln.indent > indent {
				return nil, yamlErr("line %d: bad indentation of a sequence entry", ln.num)
			}
			break
		}
		after := ln.text[1:]
		rest := strings.TrimLeft(after, " \t")
		if rest == "" || strings.HasPrefix(rest, "#") {
			p.pos++
			p.skipBlank()
			if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
				v, err := p.parseNode(p.lines[p.pos].indent)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
				continue
			}
			out = append(out, nil)
			continue
		}
		// Compact notation: "- key: value" starts a mapping whose indentation
		// is the column at which the item text begins. Rewriting the line in
		// place lets the ordinary mapping parser take over.
		if key, _, ok := splitMappingKey(rest); ok && key != "" {
			column := ln.indent + 1 + (len(after) - len(rest))
			p.lines[p.pos].indent = column
			p.lines[p.pos].text = rest
			v, err := p.parseMapping(column)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			continue
		}
		p.pos++
		v, err := p.parseInlineScalar(rest, indent, ln.num)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// parseInlineScalar parses a sequence item written on the dash line.
func (p *yamlParser) parseInlineScalar(rest string, itemIndent, lineNum int) (any, error) {
	rest = stripTrailingComment(rest)
	if rest == "" {
		return nil, nil
	}
	if rest[0] == '|' || rest[0] == '>' {
		return p.parseBlockScalar(rest, itemIndent)
	}
	if rest[0] == '{' || rest[0] == '[' {
		return p.parseFlowValue(rest, lineNum)
	}
	if rest[0] == '"' || rest[0] == '\'' {
		s, n, err := scanQuoted(rest, 0)
		if err != nil {
			return nil, yamlErr("line %d: %s", lineNum, err.Error())
		}
		if tail := strings.TrimSpace(stripTrailingComment(rest[n:])); tail != "" {
			return nil, yamlErr("line %d: unexpected content after a quoted scalar", lineNum)
		}
		return s, nil
	}
	if rest[0] == '&' || rest[0] == '*' {
		return p.parseAnchoredValue(rest, itemIndent, lineNum)
	}
	if rest[0] == '!' {
		return p.parseTaggedValue(rest, itemIndent, lineNum)
	}
	return p.parsePlainScalar(rest, itemIndent)
}

// --------------------------------------------------------------------------
// block scalars
// --------------------------------------------------------------------------

func (p *yamlParser) parseBlockScalar(header string, keyIndent int) (any, error) {
	style := header[0]
	chomp := byte(0)
	explicitIndent := 0
	for i := 1; i < len(header); i++ {
		c := header[i]
		switch {
		case c == '-' || c == '+':
			chomp = c
		case c >= '1' && c <= '9':
			explicitIndent = int(c - '0')
		case c == ' ' || c == '\t':
			i = len(header)
		default:
			return nil, yamlErr("invalid block scalar header %q", header)
		}
	}

	var body []string
	contentIndent := explicitIndent
	if contentIndent > 0 {
		contentIndent += keyIndent
	}
	lastBreakAfter := false
	for p.pos < len(p.lines) {
		ln := p.lines[p.pos]
		// Only a genuinely EMPTY line is blank inside a block scalar. A
		// comment-only line indented past the key is CONTENT: PyYAML returns
		// 'one\n# not a comment\ntwo\n' for "d: |\n  one\n  # not a comment\n
		// two\n", so treating '#' as a comment here would silently drop text.
		if strings.TrimSpace(ln.text) == "" {
			body = append(body, "")
			p.pos++
			continue
		}
		if ln.indent <= keyIndent {
			break
		}
		if contentIndent == 0 {
			contentIndent = ln.indent
		}
		if ln.indent < contentIndent {
			break
		}
		raw := strings.Repeat(" ", ln.indent-contentIndent) + ln.text
		body = append(body, raw)
		lastBreakAfter = ln.breakAfter
		p.pos++
	}
	// Trim trailing empty lines, remembering how many there were.
	trailing := 0
	for len(body) > 0 && body[len(body)-1] == "" {
		body = body[:len(body)-1]
		trailing++
	}

	// totalBreaks is the number of line breaks that follow the block's last
	// content character, which is what the chomping indicator acts on. Every
	// trailing empty line contributes one break, and the break that terminated
	// the last content line contributes one more.
	totalBreaks := trailing
	if trailing > 0 || lastBreakAfter {
		totalBreaks++
	}

	var text string
	if style == '|' {
		text = strings.Join(body, "\n")
	} else {
		text = foldBlockLines(body)
	}
	switch chomp {
	case '-':
		// strip: no trailing newline
	case '+':
		// keep: every trailing break is preserved
		text += strings.Repeat("\n", totalBreaks)
	default:
		// clip: at most one trailing newline, and only when the block actually
		// ended with a break. Verified against PyYAML 6.0.3:
		//   "d: |\n  one\n  two"  -> 'one\ntwo'    (no break after "two")
		//   "d: |\n  one\n  two\n" -> 'one\ntwo\n' (break after "two")
		if totalBreaks > 0 {
			text += "\n"
		}
	}
	return text, nil
}

// foldBlockLines implements folded (">") style: single line breaks become
// spaces, and a run of n blank lines becomes n newlines.
func foldBlockLines(lines []string) string {
	var sb strings.Builder
	i := 0
	for i < len(lines) {
		if lines[i] == "" {
			n := 0
			for i < len(lines) && lines[i] == "" {
				n++
				i++
			}
			sb.WriteString(strings.Repeat("\n", n))
			continue
		}
		if sb.Len() > 0 {
			last := sb.String()[sb.Len()-1]
			if last != '\n' {
				sb.WriteString(" ")
			}
		}
		sb.WriteString(lines[i])
		i++
	}
	return sb.String()
}

// --------------------------------------------------------------------------
// flow collections
// --------------------------------------------------------------------------

// parseFlowValue parses a flow collection that begins on the current line and
// may continue over following, more-indented lines (YAML allows a flow node to
// span lines).
func (p *yamlParser) parseFlowValue(first string, lineNum int) (any, error) {
	// Every caller has ALREADY advanced p.pos past the line that opened the flow
	// collection, so the line at p.pos is the first CONTINUATION line and must be
	// appended before advancing again. Advancing first silently dropped it: the
	// collection lost its first entry AND p.pos ended up inside the collection,
	// so the caller's indentation checks then failed on a line it should never
	// have seen ("bad indentation of a mapping entry").
	buf := first
	for !flowBalanced(buf) {
		p.skipBlank()
		if p.pos >= len(p.lines) {
			return nil, yamlErr("line %d: unexpected end of stream inside a flow collection", lineNum)
		}
		buf += " " + p.lines[p.pos].text
		p.pos++
	}
	v, n, err := p.parseFlow(buf, 0)
	if err != nil {
		return nil, yamlErr("line %d: %s", lineNum, err.Error())
	}
	if tail := strings.TrimSpace(stripTrailingComment(buf[n:])); tail != "" {
		return nil, yamlErr("line %d: unexpected content after a flow collection", lineNum)
	}
	return v, nil
}

func flowBalanced(s string) bool {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\'':
			_, n, err := scanQuoted(s, i)
			if err != nil {
				return true // let the parser report the real error
			}
			i = n - 1
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case '#':
			if i > 0 && (s[i-1] == ' ' || s[i-1] == '\t') {
				return depth <= 0
			}
		}
	}
	return depth <= 0
}

// parseFlow parses one flow node starting at index i and returns the value and
// the index just past it. It takes the parser so anchors and aliases resolve
// inside flow collections too.
func (p *yamlParser) parseFlow(s string, i int) (any, int, error) {
	i = skipFlowSpace(s, i)
	if i >= len(s) {
		return nil, i, fmt.Errorf("unexpected end of flow collection")
	}
	switch s[i] {
	case '{':
		return p.parseFlowMapping(s, i)
	case '[':
		return p.parseFlowSequence(s, i)
	case '"', '\'':
		v, n, err := scanQuoted(s, i)
		return v, n, err
	case '&':
		// "&name value" inside a flow collection.
		end := i + 1
		for end < len(s) && s[end] != ' ' && s[end] != '\t' && s[end] != ',' && s[end] != '}' && s[end] != ']' {
			end++
		}
		name := s[i+1 : end]
		if name == "" {
			return nil, i, fmt.Errorf("an anchor requires a name")
		}
		v, n, err := p.parseFlow(s, end)
		if err != nil {
			return nil, i, err
		}
		p.setAnchor(name, v)
		return v, n, nil
	case '*':
		end := i + 1
		for end < len(s) && s[end] != ' ' && s[end] != '\t' && s[end] != ',' && s[end] != '}' && s[end] != ']' {
			end++
		}
		v, err := p.anchorValue(s[i+1 : end])
		if err != nil {
			return nil, i, err
		}
		return v, end, nil
	case '!':
		end := i
		for end < len(s) && s[end] != ' ' && s[end] != '\t' && s[end] != ',' && s[end] != '}' && s[end] != ']' {
			end++
		}
		tag := s[i:end]
		v, n, err := p.parseFlow(s, end)
		if err != nil {
			return nil, i, err
		}
		converted, err := applyFlowTag(tag, v)
		if err != nil {
			return nil, i, err
		}
		return converted, n, nil
	}
	return parseFlowPlain(s, i)
}

// applyFlowTag converts an already-parsed flow node according to a tag.
func applyFlowTag(tag string, v any) (any, error) {
	switch tag {
	case "!!str":
		if v == nil {
			return "", nil
		}
		return pyStr(v), nil
	case "!!int":
		if s, ok := v.(string); ok {
			if n, err := strconv.ParseInt(strings.ReplaceAll(strings.TrimSpace(s), "_", ""), 0, 64); err == nil {
				return n, nil
			}
			return nil, fmt.Errorf("!!int cannot resolve %q", s)
		}
		return v, nil
	case "!!float":
		if s, ok := v.(string); ok {
			if f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(s), "_", ""), 64); err == nil {
				return f, nil
			}
			return nil, fmt.Errorf("!!float cannot resolve %q", s)
		}
		if n, ok := v.(int64); ok {
			return float64(n), nil
		}
		return v, nil
	case "!!bool":
		if s, ok := v.(string); ok {
			if isYAMLBool(s) {
				return s == "yes" || s == "Yes" || s == "YES" ||
					s == "true" || s == "True" || s == "TRUE" ||
					s == "on" || s == "On" || s == "ON", nil
			}
			return nil, fmt.Errorf("!!bool cannot resolve %q", s)
		}
		return v, nil
	case "!!null":
		return nil, nil
	}
	return nil, fmt.Errorf("could not determine a constructor for the tag %q", tag)
}

func skipFlowSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}

func (p *yamlParser) parseFlowMapping(s string, i int) (any, int, error) {
	out := map[string]any{}
	merges := []map[string]any{}
	i++ // consume '{'
	i = skipFlowSpace(s, i)
	if i < len(s) && s[i] == '}' {
		return out, i + 1, nil
	}
	for {
		i = skipFlowSpace(s, i)
		if i >= len(s) {
			return nil, i, fmt.Errorf("unexpected end of flow mapping")
		}
		if s[i] == '?' {
			return nil, i, fmt.Errorf("complex mapping keys are not supported")
		}
		var key string
		if s[i] == '"' || s[i] == '\'' {
			k, n, err := scanQuoted(s, i)
			if err != nil {
				return nil, i, err
			}
			key = k
			i = n
		} else {
			start := i
			for i < len(s) && s[i] != ':' && s[i] != ',' && s[i] != '}' {
				i++
			}
			key = strings.TrimRight(s[start:i], " \t")
		}
		i = skipFlowSpace(s, i)
		var value any
		if i < len(s) && s[i] == ':' {
			i++
			i = skipFlowSpace(s, i)
			if i < len(s) && s[i] != ',' && s[i] != '}' {
				v, n, err := p.parseFlow(s, i)
				if err != nil {
					return nil, i, err
				}
				value = v
				i = n
			}
		}
		if key == "<<" {
			merged, ok := value.(map[string]any)
			if !ok {
				return nil, i, fmt.Errorf("the merge value must be a mapping")
			}
			merges = append(merges, merged)
		} else {
			out[pyStr(resolveScalar(key))] = value
		}
		i = skipFlowSpace(s, i)
		if i < len(s) && s[i] == ',' {
			i++
			i = skipFlowSpace(s, i)
			if i < len(s) && s[i] == '}' {
				applyYAMLMerges(out, merges)
				return out, i + 1, nil
			}
			continue
		}
		if i < len(s) && s[i] == '}' {
			applyYAMLMerges(out, merges)
			return out, i + 1, nil
		}
		return nil, i, fmt.Errorf("expected ',' or '}' in a flow mapping")
	}
}

func (p *yamlParser) parseFlowSequence(s string, i int) (any, int, error) {
	out := []any{}
	i++ // consume '['
	i = skipFlowSpace(s, i)
	if i < len(s) && s[i] == ']' {
		return out, i + 1, nil
	}
	for {
		v, n, err := p.parseFlow(s, i)
		if err != nil {
			return nil, i, err
		}
		out = append(out, v)
		i = skipFlowSpace(s, n)
		if i < len(s) && s[i] == ',' {
			i++
			i = skipFlowSpace(s, i)
			if i < len(s) && s[i] == ']' {
				return out, i + 1, nil
			}
			continue
		}
		if i < len(s) && s[i] == ']' {
			return out, i + 1, nil
		}
		return nil, i, fmt.Errorf("expected ',' or ']' in a flow sequence")
	}
}

// parseFlowPlain reads an untagged plain scalar inside a flow collection.
func parseFlowPlain(s string, i int) (any, int, error) {
	start := i
	for i < len(s) {
		c := s[i]
		if c == ',' || c == '}' || c == ']' {
			break
		}
		if c == ':' && (i+1 == len(s) || s[i+1] == ' ' || s[i+1] == ',' || s[i+1] == '}' || s[i+1] == ']') {
			break
		}
		if c == '#' && i > start && (s[i-1] == ' ' || s[i-1] == '\t') {
			break
		}
		i++
	}
	raw := strings.TrimRight(s[start:i], " \t")
	if raw == "" {
		return nil, i, nil
	}
	return resolveScalar(raw), i, nil
}

// --------------------------------------------------------------------------
// quoted scalars
// --------------------------------------------------------------------------

// scanQuoted reads the quoted scalar that starts at s[i] and returns its value
// and the index just past the closing quote.
func scanQuoted(s string, i int) (string, int, error) {
	quote := s[i]
	i++
	var sb strings.Builder
	for i < len(s) {
		c := s[i]
		if c == quote {
			if quote == '\'' && i+1 < len(s) && s[i+1] == '\'' {
				sb.WriteByte('\'')
				i += 2
				continue
			}
			return sb.String(), i + 1, nil
		}
		if quote == '"' && c == '\\' {
			if i+1 >= len(s) {
				break
			}
			esc := s[i+1]
			switch esc {
			case '0':
				sb.WriteByte(0)
			case 'a':
				sb.WriteByte(7)
			case 'b':
				sb.WriteByte(8)
			case 't', '\t':
				sb.WriteByte('\t')
			case 'n':
				sb.WriteByte('\n')
			case 'v':
				sb.WriteByte(11)
			case 'f':
				sb.WriteByte(12)
			case 'r':
				sb.WriteByte('\r')
			case 'e':
				sb.WriteByte(27)
			case ' ':
				sb.WriteByte(' ')
			case '"':
				sb.WriteByte('"')
			case '/':
				sb.WriteByte('/')
			case '\\':
				sb.WriteByte('\\')
			case 'N':
				sb.WriteString("\u0085")
			case '_':
				sb.WriteString("\u00a0")
			case 'L':
				sb.WriteString("\u2028")
			case 'P':
				sb.WriteString("\u2029")
			case 'x', 'u', 'U':
				width := map[byte]int{'x': 2, 'u': 4, 'U': 8}[esc]
				if i+2+width > len(s) {
					return "", i, fmt.Errorf("invalid escape sequence in a double-quoted scalar")
				}
				code, err := strconv.ParseUint(s[i+2:i+2+width], 16, 32)
				if err != nil {
					return "", i, fmt.Errorf("invalid escape sequence in a double-quoted scalar")
				}
				sb.WriteRune(rune(code))
				i += 2 + width
				continue
			default:
				return "", i, fmt.Errorf("invalid escape sequence \\%c in a double-quoted scalar", esc)
			}
			i += 2
			continue
		}
		sb.WriteByte(c)
		i++
	}
	return "", i, fmt.Errorf("unexpected end of a quoted scalar")
}

// --------------------------------------------------------------------------
// comments
// --------------------------------------------------------------------------

// stripTrailingComment removes a YAML comment from an already-tokenised scalar
// fragment. A '#' starts a comment only at the start of the fragment or when
// preceded by whitespace, and never inside a quoted scalar.
func stripTrailingComment(s string) string {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\'':
			_, n, err := scanQuoted(s, i)
			if err != nil {
				return s
			}
			i = n - 1
		case '#':
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
				return strings.TrimRight(s[:i], " \t")
			}
		}
	}
	return strings.TrimRight(s, " \t")
}
