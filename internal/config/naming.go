package config

// Naming helpers ported from pydantic's alias generators.
//
// The Python reference builds every configuration model on `nanobot.config_base.Base`,
// which declares:
//
//	model_config = ConfigDict(alias_generator=to_camel, populate_by_name=True)
//	upstream/nanobot/nanobot/config_base.py:12-15
//
// `alias_generator=to_camel` means every field's *validation* alias and (absent an
// explicit serialization alias) its *serialization* alias is `to_camel(field_name)`;
// `populate_by_name=True` additionally accepts the raw snake_case field name.
//
// Both functions below are byte-for-byte ports of pydantic 2.13.5's
// `pydantic.alias_generators.to_camel` / `to_snake`.

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	// pydantic: re.match('^[a-z]+[A-Za-z0-9]*$', snake)
	reAlreadyCamel = regexp.MustCompile(`^[a-z]+[A-Za-z0-9]*$`)
	// pydantic: re.search(r'\d[a-z]', snake)
	reDigitThenLower = regexp.MustCompile(`\d[a-z]`)

	// pydantic.alias_generators.to_snake substitutions, applied in order.
	reSnakeAcronym    = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
	reSnakeLowerUpper = regexp.MustCompile(`([a-z])([A-Z])`)
	reSnakeDigitUpper = regexp.MustCompile(`([0-9])([A-Z])`)
	reSnakeLowerDigit = regexp.MustCompile(`([a-z])([0-9])`)
)

// toCamel ports pydantic.alias_generators.to_camel (pydantic 2.13.5):
//
//	if re.match('^[a-z]+[A-Za-z0-9]*$', snake) and not re.search(r'\d[a-z]', snake):
//	    return snake
//	camel = to_pascal(snake)
//	return re.sub('(^_*[A-Z])', lambda m: m.group(1).lower(), camel)
func toCamel(snake string) string {
	if reAlreadyCamel.MatchString(snake) && !reDigitThenLower.MatchString(snake) {
		return snake
	}
	return lowerFirstUpper(toPascal(snake))
}

// toPascal ports pydantic.alias_generators.to_pascal:
//
//	camel = snake.title()
//	return re.sub('([0-9A-Za-z])_(?=[0-9A-Z])', lambda m: m.group(1), camel)
//
// Go's regexp (RE2) has no lookahead, so the substitution is implemented as a
// single non-overlapping left-to-right scan, which is what re.sub does.
func toPascal(snake string) string {
	camel := pythonTitle(snake)
	rs := []rune(camel)
	var b strings.Builder
	b.Grow(len(camel))
	for i := 0; i < len(rs); i++ {
		// Drop the underscore when it is preceded by an alphanumeric and
		// followed by [0-9A-Z]. The lookahead does not consume the next rune.
		if rs[i] == '_' && i > 0 && isASCIIAlnum(rs[i-1]) &&
			i+1 < len(rs) && (isASCIIDigit(rs[i+1]) || isASCIIUpper(rs[i+1])) {
			continue
		}
		b.WriteRune(rs[i])
	}
	return b.String()
}

// lowerFirstUpper ports re.sub('(^_*[A-Z])', lower, s): it lowercases the first
// uppercase ASCII letter of the string, skipping any leading underscores.
func lowerFirstUpper(s string) string {
	rs := []rune(s)
	i := 0
	for i < len(rs) && rs[i] == '_' {
		i++
	}
	if i < len(rs) && isASCIIUpper(rs[i]) {
		rs[i] = rs[i] - 'A' + 'a'
	}
	return string(rs)
}

// pythonTitle replicates Python's str.title(): a maximal run of *cased*
// characters is a word; the first cased character of a word is uppercased (title
// cased) and the remaining cased characters are lowercased. Uncased characters
// (digits, underscore, punctuation) terminate the current word.
//
// Mirrors CPython's do_title()/unicode_title, which tracks `previous_is_cased`.
func pythonTitle(s string) string {
	rs := []rune(s)
	prevCased := false
	for i, r := range rs {
		if unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) {
			if prevCased {
				rs[i] = unicode.ToLower(r)
			} else {
				rs[i] = unicode.ToTitle(r)
			}
			prevCased = true
			continue
		}
		prevCased = false
	}
	return string(rs)
}

// toSnake ports pydantic.alias_generators.to_snake (pydantic 2.13.5).
func toSnake(camel string) string {
	snake := reSnakeAcronym.ReplaceAllString(camel, `${1}_${2}`)
	snake = reSnakeLowerUpper.ReplaceAllString(snake, `${1}_${2}`)
	snake = reSnakeDigitUpper.ReplaceAllString(snake, `${1}_${2}`)
	snake = reSnakeLowerDigit.ReplaceAllString(snake, `${1}_${2}`)
	snake = strings.ReplaceAll(snake, "-", "_")
	return strings.ToLower(snake)
}

func isASCIIUpper(r rune) bool { return r >= 'A' && r <= 'Z' }
func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }
func isASCIIAlnum(r rune) bool {
	return isASCIIUpper(r) || (r >= 'a' && r <= 'z') || isASCIIDigit(r)
}
