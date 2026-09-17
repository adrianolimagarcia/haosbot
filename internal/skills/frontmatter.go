package skills

// Skill frontmatter: the regexes and the two module-level predicates.
//
// Port of nanobot/agent/skills.py:15-48 plus SkillsLoader._strip_frontmatter
// (:309) and SkillsLoader._parse_nanobot_metadata (:318).

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// pySpaceClass is Python's `\s` for str patterns.
//
// Go's regexp `\s` is ASCII-only and would silently change the meaning of the
// reference's `\s*` runs. Python's `\s` is exactly str.isspace(), the same
// 29-code-point set textutil.PyIsSpace implements, so the class is written out
// explicitly here. The four U+001C..U+001F code points are the ones that differ
// from Go's own notion of whitespace.
const pySpaceClass = `[\x{0009}-\x{000D}\x{001C}-\x{0020}\x{0085}\x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}]`

// stripSkillFrontmatter mirrors _STRIP_SKILL_FRONTMATTER (skills.py:18-21):
//
//	r"^---\s*\r?\n(.*?)\r?\n---\s*\r?\n?"  with re.DOTALL
//
// Group 1 is the YAML body. `\s` is Python's, and `(?s)` supplies re.DOTALL.
var stripSkillFrontmatter = regexp.MustCompile(
	`(?s)\A---` + pySpaceClass + `*\r?\n(.*?)\r?\n---` + pySpaceClass + `*\r?\n?`)

// skillNamePattern mirrors _SKILL_NAME (skills.py:22) minus the leading
// negative lookahead, which RE2 cannot express and isCheckedName reproduces.
//
//	^(?!.*--)[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$
var skillNamePattern = regexp.MustCompile(`\A[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\z`)

// ParseSkillMetadata ports parse_skill_metadata (skills.py:26-36).
//
// Returns nil for "None": no frontmatter block, malformed YAML, or a YAML
// document whose root is not a mapping. All three are the same observable
// outcome in the reference, and callers rely on it (`meta.get("description") if
// meta else None`).
func ParseSkillMetadata(content string) map[string]any {
	match := stripSkillFrontmatter.FindStringSubmatch(content)
	if match == nil {
		return nil
	}
	parsed, err := yamlLoad(match[1])
	if err != nil {
		return nil
	}
	dict, ok := parsed.(map[string]any)
	if !ok {
		return nil
	}
	return dict
}

// ValidSkillMetadata ports valid_skill_metadata (skills.py:39-48): the Agent
// Skills identity contract.
//
// `len(name) <= 64` and the description bounds are CODE POINT counts, not byte
// counts (Python len() on str), so both use utf8.RuneCountInString. The
// description is stripped with Python's str.strip(), not strings.TrimSpace.
func ValidSkillMetadata(metadata map[string]any, name string) bool {
	// `metadata.get("name") == name`. A non-string value can never equal a Go
	// string, which is exactly Python's rule for `True == "True"` (False). The
	// type assertion also keeps an uncomparable value (list, mapping) from
	// panicking a direct interface comparison.
	if s, ok := metadata["name"].(string); !ok || s != name {
		return false
	}
	description := metadata["description"]
	if utf8.RuneCountInString(name) > 64 {
		return false
	}
	if !isValidSkillName(name) {
		return false
	}
	if !pyIsInstanceStr(description) {
		return false
	}
	stripped := textutil.PyStrip(description.(string))
	n := utf8.RuneCountInString(stripped)
	return n >= 1 && n <= 1024
}

// isValidSkillName reproduces `_SKILL_NAME.fullmatch(name)`.
//
// RE2 has no lookahead, so the `(?!.*--)` half is checked directly. `.` in
// Python does not match a newline, so the lookahead only inspects the first
// line — a name whose SECOND line contains "--" still matches.
func isValidSkillName(name string) bool {
	firstLine := name
	if i := strings.IndexByte(name, '\n'); i >= 0 {
		firstLine = name[:i]
	}
	if strings.Contains(firstLine, "--") {
		return false
	}
	return skillNamePattern.MatchString(name)
}

// StripFrontmatter ports SkillsLoader._strip_frontmatter (skills.py:309-316).
//
// The guard is `content.startswith("---")` and the result is
// `content[match.end():].strip()` — Python's strip, so U+001C..U+001F are
// trimmed too.
func StripFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---") {
		return content
	}
	loc := stripSkillFrontmatter.FindStringSubmatchIndex(content)
	if loc == nil {
		return content
	}
	return textutil.PyStrip(content[loc[1]:])
}

// ParseNanobotMetadata ports SkillsLoader._parse_nanobot_metadata
// (skills.py:318-336).
//
// raw is either an already-parsed mapping (yaml.safe_load turned the flow
// mapping into one) or a JSON string. The `nanobot` key wins over `openclaw`,
// and the winner must itself be a mapping. nil is Python's {}.
func ParseNanobotMetadata(raw any) map[string]any {
	var data any
	switch typed := raw.(type) {
	case map[string]any:
		data = typed
	case string:
		var parsed any
		if err := json.Unmarshal([]byte(typed), &parsed); err != nil {
			return nil
		}
		data = parsed
	default:
		return nil
	}
	obj, ok := data.(map[string]any)
	if !ok {
		return nil
	}
	payload, present := obj["nanobot"]
	if !present {
		payload = obj["openclaw"]
	}
	if dict, ok := payload.(map[string]any); ok {
		return dict
	}
	return nil
}
