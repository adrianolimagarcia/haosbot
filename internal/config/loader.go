package config

// Configuration loading and saving.
//
// Port of upstream/nanobot/nanobot/config/loader.py:42-174.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/adrianolimagarcia/nanobot-go/internal/pyjson"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// Load reads configuration from path.
//
// Port of load_config (loader.py:42-136):
//   - an empty path selects DefaultConfigPath() (loader.py:57);
//   - a MISSING file yields defaults WITHOUT error (loader.py:59-80);
//   - a non-object root, invalid JSON, invalid UTF-8 and I/O failures each map to
//     a distinct ConfigLoadError kind;
//   - legacy key migration runs before validation (loader.py:122).
//
// Scope note: the reference's `Config` is a pydantic-settings model, so it also
// merges NANOBOT_* environment variables (env_prefix "NANOBOT_",
// env_nested_delimiter "__", schema.py:664-667) ahead of the file. This port
// reads the file only. UNVERIFIED: env-var-sourced settings are therefore not
// honored; a deployment that configures nanobot purely through NANOBOT_* would
// see defaults here.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath()
	}
	target := resolvePath(path)

	info, statErr := os.Stat(target)
	if statErr != nil {
		if !errors.Is(statErr, os.ErrNotExist) {
			return nil, &LoadError{
				Path:    target,
				Kind:    KindIOError,
				Summary: "Unable to read the file: " + sentence(ioDetail(statErr)),
			}
		}
		// Missing file: defaults, no error.
		cfg := DefaultConfig()
		cfg.BindSourcePath(target)
		return cfg, nil
	}
	if info.IsDir() {
		return nil, &LoadError{
			Path:    target,
			Kind:    KindIOError,
			Summary: "Unable to read the file: Is a directory.",
		}
	}

	raw, readErr := os.ReadFile(target)
	if readErr != nil {
		return nil, &LoadError{
			Path:    target,
			Kind:    KindIOError,
			Summary: "Unable to read the file: " + sentence(ioDetail(readErr)),
		}
	}

	// Python opens the file with encoding="utf-8", so invalid bytes raise
	// UnicodeDecodeError before any JSON parsing happens (loader.py:94-99).
	if !utf8.Valid(raw) {
		return nil, &LoadError{
			Path:    target,
			Kind:    KindIOError,
			Summary: "The file is not valid UTF-8.",
		}
	}

	data, decodeErr := decodeJSONObject(raw)
	if decodeErr != nil {
		return nil, decodeErr.withPath(target)
	}

	data = migrateConfig(data)
	cfg, issues := decodeConfig(data)
	if len(issues) > 0 {
		return nil, &LoadError{
			Path:    target,
			Kind:    KindInvalidSchema,
			Summary: fmt.Sprintf("Found %d invalid setting(s).", len(issues)),
			Issues:  issues,
		}
	}
	cfg.BindSourcePath(target)
	return cfg, nil
}

// LoadDefault loads from DefaultConfigPath().
func LoadDefault() (*Config, error) { return Load("") }

// pendingLoadError lets decodeJSONObject report a failure before the target path
// is known to it.
type pendingLoadError struct {
	kind    ErrorKind
	summary string
	issues  []Issue
}

func (e *pendingLoadError) withPath(path string) *LoadError {
	return &LoadError{Path: path, Kind: e.kind, Summary: e.summary, Issues: e.issues}
}

// decodeJSONObject parses raw into a JSON object, mirroring loader.py:83-120.
func decodeJSONObject(raw []byte) (map[string]any, *pendingLoadError) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// UseNumber preserves the exact numeric literal so round-tripping a config
	// cannot silently reformat integers (Python keeps arbitrary precision ints).
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, &pendingLoadError{
			kind:    KindInvalidJSON,
			summary: jsonSyntaxSummary(raw, err),
		}
	}
	// Python's json.load rejects trailing content; encoding/json's Decoder does
	// not, so a second token is treated as a syntax error.
	if _, err := dec.Token(); err != io.EOF {
		line, col := offsetToLineCol(raw, int(dec.InputOffset()))
		return nil, &pendingLoadError{
			kind: KindInvalidJSON,
			summary: fmt.Sprintf("JSON syntax error at line %d, column %d: %s", line, col,
				sentence("Extra data")),
		}
	}

	obj, ok := v.(map[string]any)
	if !ok {
		return nil, &pendingLoadError{
			kind:    KindInvalidRoot,
			summary: "The top level of config.json must be a JSON object.",
			issues: []Issue{{
				Path:    nil,
				Message: fmt.Sprintf("Expected an object, but found %s.", jsonTypeName(v)),
			}},
		}
	}
	return obj, nil
}

// jsonSyntaxSummary ports loader.py:85-93: "JSON syntax error at line L, column
// C: <msg>.". Go reports a byte offset instead of line/column, so the position is
// derived here. The message TEXT differs from Python's json module wording.
func jsonSyntaxSummary(raw []byte, err error) string {
	line, col := 1, 1
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		line, col = offsetToLineCol(raw, int(syn.Offset))
	} else if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		line, col = offsetToLineCol(raw, len(raw))
	}
	msg := err.Error()
	if errors.Is(err, io.EOF) {
		msg = "Expecting value"
	}
	return fmt.Sprintf("JSON syntax error at line %d, column %d: %s", line, col, sentence(msg))
}

// offsetToLineCol converts a 1-based byte offset into 1-based line/column.
func offsetToLineCol(raw []byte, offset int) (int, int) {
	if offset > len(raw) {
		offset = len(raw)
	}
	if offset < 0 {
		offset = 0
	}
	line, col := 1, 1
	for i := 0; i < offset; i++ {
		if raw[i] == '\n' {
			line++
			col = 1
			continue
		}
		col++
	}
	return line, col
}

func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case json.Number:
		return "int"
	case string:
		return "str"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	default:
		return "object"
	}
}

func ioDetail(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// migrateConfig ports _migrate_config (loader.py:347-382). It mutates data in
// place and returns it.
func migrateConfig(data map[string]any) map[string]any {
	toolsValue, ok := data["tools"]
	if !ok {
		return data
	}
	tools, ok := toolsValue.(map[string]any)
	if !ok {
		return data
	}

	// tools.exec.restrictToWorkspace -> tools.restrictToWorkspace
	if execValue, present := tools["exec"]; present {
		if execCfg, isObj := execValue.(map[string]any); isObj {
			if v, has := execCfg["restrictToWorkspace"]; has {
				if _, already := tools["restrictToWorkspace"]; !already {
					tools["restrictToWorkspace"] = v
					delete(execCfg, "restrictToWorkspace")
				}
			}
		}
	}

	// tools.myEnabled / tools.mySet -> tools.my.{enable, allowSet}
	_, hasEnabled := tools["myEnabled"]
	_, hasSet := tools["mySet"]
	if hasEnabled || hasSet {
		myValue, hasMy := tools["my"]
		var myCfg map[string]any
		if !hasMy || myValue == nil {
			myCfg = map[string]any{}
			tools["my"] = myCfg
		} else {
			var isObj bool
			myCfg, isObj = myValue.(map[string]any)
			if !isObj {
				return data
			}
		}
		if hasEnabled {
			if _, taken := myCfg["enable"]; !taken {
				myCfg["enable"] = tools["myEnabled"]
				delete(tools, "myEnabled")
			} else {
				delete(tools, "myEnabled")
			}
		}
		if hasSet {
			if _, taken := myCfg["allowSet"]; !taken {
				myCfg["allowSet"] = tools["mySet"]
				delete(tools, "mySet")
			} else {
				delete(tools, "mySet")
			}
		}
	}
	return data
}

// Save writes the configuration to path, creating parent directories.
//
// Port of save_config (loader.py:146-174):
//   - model_dump(mode="json", by_alias=True) -> serialization aliases, nulls kept
//     except for the two exclude_if None fields (display_name, dream.cron);
//   - the OAuth providers excluded from the dump are re-added with only their
//     non-credential request settings;
//   - written atomically (temp file + rename) so a crash cannot truncate it.
func (c *Config) Save(path string) error {
	if path == "" {
		path = DefaultConfigPath()
	}
	target := resolvePath(path)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}

	base, err := marshalNoEscape(c)
	if err != nil {
		return err
	}
	withOAuth, err := reinsertOAuthProviders(base, c.Providers.OpenAICodex, c.Providers.XAIGrok)
	if err != nil {
		return err
	}

	text, err := indentJSON(withOAuth)
	if err != nil {
		return err
	}
	// Go's encoder escapes U+2028/U+2029 unconditionally, and
	// SetEscapeHTML(false) does not disable it. Python's json.dumps with
	// ensure_ascii=False leaves them literal (loader.py:174), so the bytes are
	// reconciled here, after the last encoding step.
	text = pyjson.UnescapeLineSeparators(text)
	return writeFileAtomic(target, text)
}

// marshalNoEscape marshals without HTML escaping, matching Python's
// json.dumps(..., ensure_ascii=False), which leaves <, > and & literal.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// indentJSON renders compact JSON with two-space indentation and no trailing
// newline, matching json.dumps(data, indent=2).
func indentJSON(compact []byte) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(json.RawMessage(compact)); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// reinsertOAuthProviders ports loader.py:158-171: OAuth credentials live in
// dedicated token stores, so only `proxy` and `extra_body` are persisted, and
// only when at least one of them is set.
func reinsertOAuthProviders(base []byte, codex, grok ProviderConfig) ([]byte, error) {
	codexJSON := oauthSettingsJSON(codex)
	grokJSON := oauthSettingsJSON(grok)
	if codexJSON == nil && grokJSON == nil {
		return base, nil
	}
	top, err := splitObjectPairs(base)
	if err != nil {
		return nil, err
	}
	for i, pair := range top {
		if string(pair[0]) != `"providers"` {
			continue
		}
		providers, err := splitObjectPairs(pair[1])
		if err != nil {
			return nil, err
		}
		if codexJSON != nil {
			providers = append(providers, jsonPair("openaiCodex", codexJSON))
		}
		if grokJSON != nil {
			providers = append(providers, jsonPair("xaiGrok", grokJSON))
		}
		top[i][1] = joinObjectPairs(providers)
	}
	return joinObjectPairs(top), nil
}

// oauthSettingsJSON renders {"extraBody": ..., "proxy": ...} with None values
// omitted. Field order follows ProviderConfig's declaration order
// (extra_body precedes proxy in schema.py:205-207).
func oauthSettingsJSON(p ProviderConfig) json.RawMessage {
	var pairs [][2]json.RawMessage
	if p.ExtraBody != nil {
		if b, err := marshalNoEscape(p.ExtraBody); err == nil {
			pairs = append(pairs, jsonPair("extraBody", b))
		}
	}
	if p.Proxy != nil {
		if b, err := marshalNoEscape(*p.Proxy); err == nil {
			pairs = append(pairs, jsonPair("proxy", b))
		}
	}
	if len(pairs) == 0 {
		return nil
	}
	return joinObjectPairs(pairs)
}

func jsonPair(key string, value []byte) [2]json.RawMessage {
	kb, _ := json.Marshal(key)
	return [2]json.RawMessage{kb, value}
}

// splitObjectPairs returns the ordered key/value pairs of a JSON object.
func splitObjectPairs(raw []byte) ([][2]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected a JSON object")
	}
	var pairs [][2]json.RawMessage
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, fmt.Errorf("object key is not a string")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		pairs = append(pairs, jsonPair(key, value))
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return pairs, nil
}

func joinObjectPairs(pairs [][2]json.RawMessage) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(p[0])
		b.WriteByte(':')
		b.Write(p[1])
	}
	b.WriteByte('}')
	return b.Bytes()
}

// writeFileAtomic ports utils/helpers._write_text_atomic (helpers.py:556-577):
// write a sibling temp file, fsync it, rename over the target, then fsync the
// directory, preserving the target's existing permission bits.
func writeFileAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)
	existingMode, modeErr := os.Stat(path)

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()

	if modeErr == nil {
		_ = tmp.Chmod(existingMode.Mode().Perm())
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// FormatLoadError renders an error the way ConfigLoadError.__str__ does. It is
// exported for callers that print diagnostics.
func FormatLoadError(err error) string {
	var le *LoadError
	if errors.As(err, &le) {
		return le.Error()
	}
	if err == nil {
		return ""
	}
	return err.Error()
}
