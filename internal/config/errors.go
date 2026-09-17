package config

// User-safe configuration diagnostics.
//
// Port of upstream/nanobot/nanobot/config/errors.py. The redaction rule in
// Issue.Location is security-relevant: pydantic error locations can contain
// user-controlled mapping keys (MCP server names, custom provider names), so
// only conventional config identifiers are rendered.

import (
	"regexp"
	"strconv"
	"strings"
)

// ErrorKind classifies a configuration loading failure.
type ErrorKind string

// Error kinds, mirroring ConfigErrorKind in config/errors.py:9-15.
const (
	KindInvalidJSON   ErrorKind = "invalid_json"
	KindInvalidRoot   ErrorKind = "invalid_root"
	KindInvalidSchema ErrorKind = "invalid_schema"
	KindMissingEnv    ErrorKind = "missing_env"
	KindIOError       ErrorKind = "io_error"
)

// PathPart is one component of an issue location: a field name or a list index.
type PathPart struct {
	Name  string
	Index int
	IsIdx bool
}

// Field builds a named path part.
func Field(name string) PathPart { return PathPart{Name: name} }

// Index builds a positional path part.
func Index(i int) PathPart { return PathPart{Index: i, IsIdx: true} }

func (p PathPart) String() string {
	if p.IsIdx {
		return strconv.Itoa(p.Index)
	}
	return p.Name
}

// Issue is one actionable configuration problem.
type Issue struct {
	Path    []PathPart
	Message string
}

// _safeLocationPart mirrors _SAFE_LOCATION_PART in config/errors.py:17.
var _safeLocationPart = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)

// Location renders the issue path exactly like ConfigIssue.location
// (config/errors.py:37-45).
func (i Issue) Location() string {
	if len(i.Path) == 0 {
		return "<root>"
	}
	parts := make([]string, 0, len(i.Path))
	for _, p := range i.Path {
		if p.IsIdx {
			parts = append(parts, strconv.Itoa(p.Index))
			continue
		}
		if _safeLocationPart.MatchString(p.Name) {
			parts = append(parts, p.Name)
		} else {
			parts = append(parts, "<redacted>")
		}
	}
	return strings.Join(parts, ".")
}

// LoadError is a structured, user-safe configuration loading failure.
// Port of ConfigLoadError (config/errors.py:48-81).
type LoadError struct {
	Path    string
	Kind    ErrorKind
	Summary string
	Issues  []Issue
}

func (e *LoadError) Error() string {
	var b strings.Builder
	b.WriteString("Invalid configuration: ")
	b.WriteString(e.Path)
	b.WriteString("\n\n")
	b.WriteString(e.Summary)
	limit := len(e.Issues)
	if limit > 10 {
		limit = 10
	}
	for _, issue := range e.Issues[:limit] {
		b.WriteString("\n\n  ")
		b.WriteString(issue.Location())
		b.WriteString("\n    ")
		b.WriteString(issue.Message)
	}
	if remaining := len(e.Issues) - 10; remaining > 0 {
		b.WriteString("\n\n  … and ")
		b.WriteString(strconv.Itoa(remaining))
		b.WriteString(" more issue(s)")
	}
	return b.String()
}

// FriendlyMessage ports _friendly_validation_message (config/errors.py:98-112).
// `code` is a pydantic error type string such as "int_type" or "greater_than_equal".
func FriendlyMessage(message, code string) string {
	switch code {
	case "extra_forbidden":
		return "Unknown setting."
	case "missing":
		return "This setting is required."
	case "assertion_error", "value_error":
		// Custom validators control these messages and may interpolate the
		// rejected value. Keep the field location, never the text.
		return "Value does not satisfy this setting's requirements."
	}
	message = strings.TrimPrefix(message, "Value error, ")
	if strings.HasPrefix(message, "Input should be ") {
		message = "Must be " + strings.TrimPrefix(message, "Input should be ")
	} else if strings.HasPrefix(message, "Input should have ") {
		message = "Must have " + strings.TrimPrefix(message, "Input should have ")
	}
	if message != "" {
		message = strings.ToUpper(message[:1]) + message[1:]
	}
	if message != "" && !strings.ContainsAny(message[len(message)-1:], ".!?") {
		message += "."
	}
	if message == "" {
		return "Invalid value."
	}
	return message
}

// sentence ports _sentence (config/loader.py:385-389).
func sentence(message string) string {
	message = strings.TrimSpace(message)
	if message != "" && !strings.ContainsAny(message[len(message)-1:], ".!?") {
		message += "."
	}
	return message
}
