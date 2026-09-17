package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// Reference limits for read_file (nanobot/agent/tools/filesystem.py:274-277).
const (
	// readMaxChars mirrors ReadFileTool._MAX_CHARS.
	readMaxChars = 128_000
	// readMaxBytes mirrors ReadFileTool._MAX_FILE_SIZE_BYTES. Unlike the
	// reference, which stats the file and then reads it whole, this port also
	// uses it as the read cap so a file that grows between stat and read cannot
	// be pulled into memory without bound.
	readMaxBytes = 100 * 1024 * 1024
	// readDefaultLimit mirrors ReadFileTool._DEFAULT_LIMIT.
	readDefaultLimit = 2000
)

// ReadFile reads a text file with optional line-based pagination.
//
// Ports ReadFileTool (nanobot/agent/tools/filesystem.py:251-415). Read-only:
// embeds tools.ReadOnlyBase so the runner may parallelize it.
type ReadFile struct {
	tools.ReadOnlyBase
	policy PathPolicy
	params json.RawMessage
}

// NewReadFile returns a read_file tool bound to policy.
func NewReadFile(policy PathPolicy) *ReadFile {
	return &ReadFile{
		policy: policy,
		// Mirrors the @tool_parameters decorator on ReadFileTool
		// (filesystem.py:251-269).
		params: objectSchema(
			[]string{"path"},
			map[string]any{
				"path":   strProp("The file path to read"),
				"offset": minIntProp("1-based text or extracted-document line (default 1)", 1),
				"limit":  minIntProp("Maximum lines to return (default 2000)", 1),
				"pages":  strProp("PDF page number or range, e.g. '7' or '1-5' (max 20 pages)"),
				"force":  boolProp("Return an unchanged range again", false),
			},
		),
	}
}

// Name mirrors ReadFileTool.name (filesystem.py:279-281).
func (t *ReadFile) Name() string { return "read_file" }

// Description mirrors ReadFileTool.description (filesystem.py:283-288) verbatim.
//
// NOTE: the reference description advertises images, PDFs and Office documents;
// this port supports UTF-8 text only (see doc.go). The text is kept verbatim so
// the advertised tool surface matches the reference.
func (t *ReadFile) Description() string {
	return "Read text, images, PDFs, and Office documents by path. " +
		"Text is line-numbered; use offset/limit or pages for targeted ranges."
}

// Parameters returns the JSON Schema for the arguments (filesystem.py:251-269).
func (t *ReadFile) Parameters() json.RawMessage { return t.params }

// Execute ports ReadFileTool.execute (filesystem.py:294-415).
func (t *ReadFile) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	args, bad := parseArgs(t.Name(), raw, "path", "offset", "limit", "pages", "force")
	if bad != nil {
		return *bad, nil
	}
	path, bad := requiredString(t.Name(), args, "path")
	if bad != nil {
		return *bad, nil
	}
	offset, _, bad := optionalInt(t.Name(), args, "offset")
	if bad != nil {
		return *bad, nil
	}
	limit, hasLimit, bad := optionalInt(t.Name(), args, "limit")
	if bad != nil {
		return *bad, nil
	}
	// "pages" and "force" are accepted for schema parity but have no effect:
	// pages only drives PDF extraction, and force only bypasses the
	// FileStates unchanged-file cache, neither of which this port implements
	// (filesystem.py:330-333, :347-350).
	if _, _, bad := optionalString(t.Name(), args, "pages"); bad != nil {
		return *bad, nil
	}
	if _, _, bad := optionalBool(t.Name(), args, "force"); bad != nil {
		return *bad, nil
	}

	if path == "" {
		return tools.Errf("Error reading file: Unknown path"), nil
	}
	if isBlockedDevice(path) {
		return tools.Errf("Error: Reading %s is blocked (device path that could hang or produce infinite output).", path), nil
	}

	fp, err := t.policy.resolveRead(path)
	if err != nil {
		return permissionOrGeneric(path, err), nil
	}
	if isBlockedDevice(fp) {
		return tools.Errf("Error: Reading %s is blocked (device path that could hang or produce infinite output).", fp), nil
	}

	info, err := os.Stat(fp)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// NOTE: _builtin_skill_read_path (filesystem.py:234-248) is not
			// ported; there is no bundled-skills directory in this port.
			return tools.Errf("Error: File not found: %s", path), nil
		}
		return permissionOrGeneric(path, err), nil
	}
	// Path.is_file() is S_ISREG, so directories, FIFOs and devices are refused
	// here rather than opened: reading a FIFO would block the tool forever.
	if !info.Mode().IsRegular() {
		return tools.Errf("Error: Not a file: %s", path), nil
	}
	if info.Size() > readMaxBytes {
		return tools.Errf("Error: File too large to read (%.1f MiB). Maximum is %d MiB.",
			float64(info.Size())/(1024*1024), readMaxBytes/(1024*1024)), nil
	}

	switch ext := strings.ToLower(filepath.Ext(fp)); ext {
	case ".pdf":
		return tools.Errf("Error reading PDF: PDF extraction is not implemented by this port."), nil
	case ".docx", ".xlsx", ".pptx":
		return tools.Errf("Error reading %s file: document extraction is not implemented by this port.",
			strings.ToUpper(strings.TrimPrefix(ext, "."))), nil
	}

	rawBytes, err := readFileCapped(fp)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			return tools.Errf("Error: File too large to read (%.1f MiB). Maximum is %d MiB.",
				float64(len(rawBytes))/(1024*1024), readMaxBytes/(1024*1024)), nil
		}
		return permissionOrGeneric(path, err), nil
	}
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	if len(rawBytes) == 0 {
		return tools.OK(fmt.Sprintf("(Empty file: %s)", path)), nil
	}

	text, ok := decodeText(rawBytes, fp)
	if !ok {
		mime := detectImageMime(rawBytes)
		if mime == "" {
			mime = mimeByExtension(path)
		}
		if mime == "" {
			mime = "unknown"
		}
		// The reference returns image content blocks here (filesystem.py:342-344)
		// or a MIME-aware binary error (filesystem.py:369-372). tools.Result is
		// text-only, so both land on the error path; the trailing note makes the
		// port's limitation explicit instead of implying images are readable.
		return tools.Errf("Error: Cannot read binary file %s (MIME: %s). "+
			"Only supported text files and images can be read."+
			"\n(Image and document reading is not implemented by this port; only UTF-8 text files can be read.)",
			path, mime), nil
	}

	// Normalize CRLF -> LF before line splitting (filesystem.py:374-378).
	text = strings.ReplaceAll(text, "\r\n", "\n")
	allLines := splitLines(text)
	total := len(allLines)

	if offset < 1 {
		offset = 1
	}
	if offset > total {
		return tools.Errf("Error: offset %d is beyond end of file (%d lines)", offset, total), nil
	}
	start := offset - 1
	window := readDefaultLimit
	if hasLimit && limit > 0 {
		window = limit
	}
	end := start + window
	if end > total {
		end = total
	}

	numbered := make([]string, 0, end-start)
	for i, line := range allLines[start:end] {
		numbered = append(numbered, fmt.Sprintf("%d| %s", start+i+1, line))
	}
	result := strings.Join(numbered, "\n")

	if utf8.RuneCountInString(result) > readMaxChars {
		trimmed := make([]string, 0, len(numbered))
		chars := 0
		for _, line := range numbered {
			chars += utf8.RuneCountInString(line) + 1
			if chars > readMaxChars {
				break
			}
			trimmed = append(trimmed, line)
		}
		end = start + len(trimmed)
		result = strings.Join(trimmed, "\n")
	}

	if end < total {
		result += fmt.Sprintf("\n\n(Showing lines %d-%d of %d. Use offset=%d to continue.)", offset, end, total, end+1)
	} else {
		result += fmt.Sprintf("\n\n(End of file — %d lines total)", total)
	}
	return tools.OK(result), nil
}

// errTooLarge reports that the file exceeded readMaxBytes while reading.
var errTooLarge = errors.New("builtin: file exceeds read cap")

// readFileCapped reads at most readMaxBytes+1 bytes so an oversized (or
// concurrently growing) file cannot exhaust memory. It returns the bytes read
// together with errTooLarge when the cap was hit.
func readFileCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, readMaxBytes+1))
	if err != nil {
		return data, err
	}
	if len(data) > readMaxBytes {
		return data, errTooLarge
	}
	return data, nil
}

// decodeText mirrors the reference's decode path (filesystem.py:351-372): UTF-8
// first, then latin-1 for known text extensions, otherwise unsupported.
func decodeText(raw []byte, fp string) (string, bool) {
	if utf8.Valid(raw) {
		return string(raw), true
	}
	if textExtensions[strings.ToLower(filepath.Ext(fp))] {
		return decodeLatin1(raw), true
	}
	return "", false
}

// decodeLatin1 mirrors raw.decode("latin-1") (filesystem.py:359).
func decodeLatin1(raw []byte) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, c := range raw {
		b.WriteRune(rune(c))
	}
	return b.String()
}

// mimeByExtension mirrors the mimetypes.guess_type fallback
// (filesystem.py:342, :361). Divergence: it is a fixed subset of the system
// MIME database Python consults, kept deterministic and dependency-free.
func mimeByExtension(path string) string {
	ext := filepath.Ext(path)
	if ext == "" {
		return ""
	}
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".txt", ".md":
		return "text/plain"
	case ".json":
		return "application/json"
	}
	return ""
}

// permissionOrGeneric mirrors the reference's PermissionError / generic error
// handling (filesystem.py:412-415): permission problems are surfaced verbatim,
// everything else is prefixed.
//
// WorkspaceBoundaryError is a PermissionError subclass upstream
// (security/workspace_policy.py:20), so a policy escape must take the
// reference's `except PermissionError` arm and render as "Error: ...".
// Verified by running the reference: read_file on "../escape.txt" yields
// "Error: Path ../escape.txt is outside allowed directory <dir> (...)".
func permissionOrGeneric(path string, err error) tools.Result {
	var boundary *boundaryError
	if errors.As(err, &boundary) || errors.Is(err, fs.ErrPermission) {
		return tools.Errf("Error: %v", err)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return tools.Errf("Error: File not found: %s", path)
	}
	return tools.Errf("Error reading file: %v", err)
}

// splitLines mirrors Python str.splitlines() (used at filesystem.py:380), which
// breaks on more boundaries than "\n".
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i, r := range s {
		if !isLineBoundary(r) {
			continue
		}
		out = append(out, s[start:i])
		next := i + utf8.RuneLen(r)
		if r == '\r' && next < len(s) && s[next] == '\n' {
			next++
		}
		start = next
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func isLineBoundary(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}
