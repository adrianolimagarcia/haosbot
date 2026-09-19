package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// Reference limits for exec (nanobot/agent/tools/shell.py:250-251 and
// nanobot/agent/tools/exec_session.py:23-29).
const (
	// execMaxTimeoutSeconds mirrors ExecTool._MAX_TIMEOUT: a model-supplied
	// timeout is capped here.
	execMaxTimeoutSeconds = 600
	// execDefaultTimeoutSeconds mirrors ExecToolConfig.timeout's default
	// (shell.py:97).
	execDefaultTimeoutSeconds = 60
	// execMaxOutputChars mirrors ExecTool._MAX_OUTPUT.
	execMaxOutputChars = 10_000
	// execMinOutputChars / execMaxOutputCharsLimit mirror
	// clamp_session_int(max_output_chars, ..., 1000, MAX_OUTPUT_CHARS)
	// (shell.py:339, exec_session.py:29).
	execMinOutputChars      = 1000
	execMaxOutputCharsLimit = 50_000
)

// execMaxCaptureBytes caps the bytes buffered per stream.
//
// This cap does not exist in the reference, which buffers the whole output of
// the child process and only then truncates the rendered result
// (shell.py:309-346). Buffering is capped here so a command that produces
// gigabytes cannot exhaust memory; the model is told when the cap was hit.
const execMaxCaptureBytes = 1 << 20

// execDenyPatterns mirrors the built-in deny list (shell.py:215-233).
//
// Divergence: Python's `format(?!=)` uses a negative lookahead, which RE2 does
// not support, so the ported pattern is `(^|[;&|]\s*)format\b` and also matches
// `format=...`.
var execDenyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\brm\s+-[rf]{1,2}\b`),
	regexp.MustCompile(`\bdel\s+/[fq]\b`),
	regexp.MustCompile(`\brmdir\s+/s\b`),
	regexp.MustCompile(`(^|[;&|]\s*)format\b`),
	regexp.MustCompile(`\b(mkfs|diskpart)\b`),
	regexp.MustCompile(`\bdd\s+if=`),
	regexp.MustCompile(`>\s*/dev/sd`),
	regexp.MustCompile(`\b(shutdown|reboot|poweroff)\b`),
	regexp.MustCompile(`:\(\)\s*\{.*\};\s*:`),
	regexp.MustCompile(`>>?\s*\S*(history\.jsonl|\.dream_cursor)`),
	regexp.MustCompile(`\btee\b[^|;&<>]*(history\.jsonl|\.dream_cursor)`),
	regexp.MustCompile(`\b(cp|mv)\b(\s+[^\s|;&<>]+)+\s+\S*(history\.jsonl|\.dream_cursor)`),
	regexp.MustCompile(`\bdd\b[^|;&<>]*\bof=\S*(history\.jsonl|\.dream_cursor)`),
	regexp.MustCompile(`\bsed\s+-i[^|;&<>]*(history\.jsonl|\.dream_cursor)`),
}

// ExecOptions configures the exec tool.
type ExecOptions struct {
	// Workspace resolves a relative working_dir (ExecTool.working_dir,
	// shell.py:177-181). Empty means the process working directory.
	Workspace string
	// RestrictToWorkspace enables the workspace guard: working_dir containment,
	// the ".." traversal check and the deny-pattern filter
	// (shell.py:428-455, :807-909). It is off by default, matching the
	// reference's restrict_to_workspace default.
	RestrictToWorkspace bool
	// DefaultTimeout is the hard timeout used when the model passes no timeout.
	// Zero selects 60s (shell.py:97).
	//
	// NOTE: the reference's config-level timeout of 0 meaning "no limit"
	// (shell.py:386-398) is not implemented; a limit is always enforced.
	DefaultTimeout time.Duration
	// MaxTimeout caps a model-supplied timeout. Zero selects 600s
	// (shell.py:250).
	MaxTimeout time.Duration
	// Shell overrides the default shell program.
	Shell string
	// DenyPatterns are additional deny regexes; they are matched in addition to
	// the built-in list (shell.py:215-233).
	DenyPatterns []string
	// Env overrides the child environment. Nil selects the default environment
	// (see buildEnv).
	Env []string
}

// Exec runs a shell command with a hard timeout and bounded output capture.
//
// Ports ExecTool (nanobot/agent/tools/shell.py:118-356). It is exclusive: the
// reference declares `exclusive = True` (shell.py:270-272), and tools.Base
// provides the conservative mutating defaults for ReadOnly/ConcurrencySafe.
type Exec struct {
	tools.Base
	opts   ExecOptions
	params json.RawMessage
}

// NewExec returns an exec tool configured by opts.
func NewExec(opts ExecOptions) *Exec {
	return &Exec{
		opts: opts,
		// Mirrors the @tool_parameters decorator on ExecTool (shell.py:118-160).
		// The reference's schema has no required list, because either `command`
		// or its `cmd` alias is accepted.
		params: objectSchema(
			nil,
			map[string]any{
				"command":     strProp("The shell command to execute"),
				"cmd":         strProp("Compatibility alias for command"),
				"working_dir": strProp("Optional working directory for the command"),
				"workdir":     strProp("Compatibility alias for working_dir"),
				"timeout": intProp("Hard timeout in seconds (default 60, max 600).",
					1, execMaxTimeoutSeconds),
				"shell": nullableStrProp("Shell override; omit for bash, or pass 'sh' or 'zsh'."),
				"login": nullableBoolProp("Run bash/zsh as a login shell.", false),
				// yield_time_ms is deliberately NOT advertised. The reference
				// offers background exec sessions here (shell.py:118-160,
				// exec_session.py), but this port has no session manager and
				// rejects the argument at execute time (see the hasYield branch
				// below). Advertising a parameter that is always refused costs
				// the model a full provider round trip per command: it sees the
				// parameter in the schema, sends it, and only then learns that
				// it is unsupported. parseArgs still accepts the key so that a
				// model which sends it anyway receives the explanatory error
				// rather than a generic unknown-argument failure.
				"max_output_chars": nullableIntProp(
					"Session output limit in characters (default 10000, max 50000).",
					execMinOutputChars, execMaxOutputCharsLimit),
				"max_output_tokens": nullableIntProp(
					"Compatibility alias for max_output_chars.",
					execMinOutputChars, execMaxOutputCharsLimit),
			},
		),
	}
}

// Name mirrors ExecTool.name (shell.py:246-248).
func (t *Exec) Name() string { return "exec" }

// Description mirrors ExecTool.description (shell.py:266-268) verbatim.
func (t *Exec) Description() string { return "Execute a shell command." }

// Parameters returns the JSON Schema for the arguments (shell.py:118-160).
func (t *Exec) Parameters() json.RawMessage { return t.params }

// Exclusive mirrors ExecTool.exclusive (shell.py:270-272).
func (t *Exec) Exclusive() bool { return true }

// Execute ports ExecTool.execute (shell.py:274-356).
func (t *Exec) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	args, bad := parseArgs(t.Name(), raw,
		"command", "cmd", "working_dir", "workdir", "timeout", "shell", "login",
		"yield_time_ms", "max_output_chars", "max_output_tokens")
	if bad != nil {
		return *bad, nil
	}

	command, _, bad := optionalString(t.Name(), args, "command")
	if bad != nil {
		return *bad, nil
	}
	if command == "" {
		alias, _, bad := optionalString(t.Name(), args, "cmd")
		if bad != nil {
			return *bad, nil
		}
		command = alias
	}
	if command == "" {
		return tools.Errf("Error: Missing command. Provide command or cmd."), nil
	}

	workingDir, _, bad := optionalString(t.Name(), args, "working_dir")
	if bad != nil {
		return *bad, nil
	}
	if workingDir == "" {
		alias, _, bad := optionalString(t.Name(), args, "workdir")
		if bad != nil {
			return *bad, nil
		}
		workingDir = alias
	}

	timeoutSeconds, hasTimeout, bad := optionalInt(t.Name(), args, "timeout")
	if bad != nil {
		return *bad, nil
	}
	shellOverride, _, bad := optionalString(t.Name(), args, "shell")
	if bad != nil {
		return *bad, nil
	}
	login, _, bad := optionalBool(t.Name(), args, "login")
	if bad != nil {
		return *bad, nil
	}
	_, hasYield, bad := optionalInt(t.Name(), args, "yield_time_ms")
	if bad != nil {
		return *bad, nil
	}
	maxOutput, hasMaxOutput, bad := optionalInt(t.Name(), args, "max_output_chars")
	if bad != nil {
		return *bad, nil
	}
	if !hasMaxOutput {
		maxOutput, hasMaxOutput, bad = optionalInt(t.Name(), args, "max_output_tokens")
		if bad != nil {
			return *bad, nil
		}
	}
	if hasYield {
		// The reference returns a background session poll here
		// (shell.py:294-295, exec_session.py). This port has no session
		// manager, and silently ignoring the argument would misreport a
		// still-running command as finished.
		return tools.Errf("Error: yield_time_ms (background exec sessions) is not supported by this port; " +
			"omit it to run the command in the foreground."), nil
	}

	cwd, guardResult := t.prepare(command, workingDir)
	if guardResult != nil {
		return *guardResult, nil
	}

	shellProgram, shellResult := t.resolveShell(shellOverride)
	if shellResult != nil {
		return *shellResult, nil
	}

	timeout := t.resolveTimeout(timeoutSeconds, hasTimeout)
	maxChars := execMaxOutputChars
	if hasMaxOutput {
		maxChars = clampInt(maxOutput, execMinOutputChars, execMaxOutputCharsLimit)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, shellProgram.program, shellProgram.extraArgs(command, login)...)
	cmd.Dir = cwd
	cmd.Env = t.buildEnv()
	configureProcessTree(cmd)
	stdout := &cappedBuffer{limit: execMaxCaptureBytes}
	stderr := &cappedBuffer{limit: execMaxCaptureBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// stdin is /dev/null, mirroring asyncio.subprocess.DEVNULL (shell.py:524).

	err := cmd.Run()
	if err != nil {
		// Cancellation and the internal deadline are only meaningful when the
		// command did not complete on its own.
		if ctx.Err() != nil {
			// The caller canceled the turn: propagate so the runner can abort
			// cleanly, mirroring the reference re-raising CancelledError
			// (shell.py:316-318).
			return tools.Result{}, ctx.Err()
		}
		if runCtx.Err() != nil {
			return tools.Errf("Error: Command timed out after %d seconds", int(timeout.Seconds())), nil
		}
	}

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		switch {
		case errors.Is(err, exec.ErrWaitDelay):
			// The process was killed and its output pipes were still draining
			// when WaitDelay expired; the exit status below is best-effort.
			exitCode = -1
		case errors.As(err, &exitErr):
			if code, ok := signalExitCode(exitErr.ProcessState); ok {
				exitCode = code
			} else {
				exitCode = exitErr.ExitCode()
			}
		default:
			return tools.Errf("Error executing command: %v", err), nil
		}
	}

	parts := make([]string, 0, 3)
	if stdout.Len() > 0 {
		parts = append(parts, stdout.text("stdout"))
	}
	if strings.TrimSpace(stderr.String()) != "" {
		parts = append(parts, "STDERR:\n"+stderr.text("stderr"))
	}
	parts = append(parts, fmt.Sprintf("\nExit code: %d", exitCode))
	result := strings.Join(parts, "\n")

	result = truncateRunes(result, maxChars)
	// Divergence: the reference reports a non-zero exit as ordinary content
	// (shell.py:335-349). This port flags it as an error result so the model
	// cannot mistake a failed command for a successful one.
	return tools.Result{Content: result, IsError: exitCode != 0}, nil
}

// prepare resolves the working directory and applies the workspace guard
// (shell.py:400-455).
func (t *Exec) prepare(command, workingDir string) (string, *tools.Result) {
	workspaceRoot := t.opts.Workspace
	if workspaceRoot == "" {
		if wd, err := os.Getwd(); err == nil {
			workspaceRoot = wd
		}
	}
	cwd := workspaceRoot
	if workingDir != "" {
		requested := expandUser(workingDir)
		if filepath.IsAbs(requested) {
			cwd = requested
		} else {
			cwd = filepath.Join(workspaceRoot, requested)
		}
	}
	if !t.opts.RestrictToWorkspace {
		return cwd, nil
	}

	// A model-supplied working_dir must not escape the workspace
	// (shell.py:428-441). The reference performs this check only when a
	// workspace root is configured, which is also the case here.
	if t.opts.Workspace != "" && !isPathWithin(cwd, t.opts.Workspace) {
		r := tools.Errf("Error: working_dir is outside the configured workspace" + workspaceBoundaryNote)
		return "", &r
	}

	lower := strings.ToLower(strings.TrimSpace(command))
	for _, re := range execDenyPatterns {
		if re.MatchString(lower) {
			r := tools.Errf("Error: Command blocked by deny pattern filter")
			return "", &r
		}
	}
	for _, pattern := range t.opts.DenyPatterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			r := tools.Errf("Error: invalid deny pattern %q: %v", pattern, err)
			return "", &r
		}
		if re.MatchString(lower) {
			r := tools.Errf("Error: Command blocked by deny pattern filter")
			return "", &r
		}
	}
	// NOTE: the reference also blocks internal/private URLs
	// (shell.py:836-844) and absolute paths outside the working directory
	// (shell.py:864-907); neither is ported (see doc.go).
	if strings.Contains(lower, "../") || strings.Contains(lower, `..\`) {
		r := tools.Errf("Error: Command blocked by safety guard (path traversal detected)" + workspaceBoundaryNote)
		return "", &r
	}
	return cwd, nil
}

// resolveTimeout mirrors ExecTool._resolve_timeout (shell.py:386-398).
func (t *Exec) resolveTimeout(seconds int, provided bool) time.Duration {
	max := t.opts.MaxTimeout
	if max <= 0 {
		max = execMaxTimeoutSeconds * time.Second
	}
	if provided && seconds > 0 {
		d := time.Duration(seconds) * time.Second
		if d > max {
			return max
		}
		return d
	}
	if t.opts.DefaultTimeout > 0 {
		return t.opts.DefaultTimeout
	}
	return execDefaultTimeoutSeconds * time.Second
}

// buildEnv returns the child environment.
//
// Divergence: the reference forwards only HOME/LANG/TERM/PYTHONUNBUFFERED on
// Unix (shell.py:794-805) and relies on path_prepend / a login shell to make
// external binaries reachable. This port has neither mechanism, so PATH is
// forwarded as well. Secrets are still not forwarded.
func (t *Exec) buildEnv() []string {
	if t.opts.Env != nil {
		return t.opts.Env
	}
	env := make([]string, 0, 5)
	if path, ok := os.LookupEnv("PATH"); ok {
		env = append(env, "PATH="+path)
	}
	// HOME fallback.
	//
	// DIVERGENCE (deliberate): the reference is
	// `os.environ.get("HOME", "/tmp")` (shell.py:792), i.e. a child with no
	// HOME in its parent environment gets HOME=/tmp. This port refuses to
	// point a child process at a shared, world-writable, RAM-backed tmpfs.
	// HOME is set in every realistic deployment, so this path is degenerate;
	// when it is taken we prefer the real user home, then the configured
	// workspace, then the process working directory.
	home := os.Getenv("HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			home = h
		} else if t.opts.Workspace != "" {
			home = t.opts.Workspace
		} else {
			home = "."
		}
	}
	env = append(env, "HOME="+home)
	lang := os.Getenv("LANG")
	if lang == "" {
		lang = "C.UTF-8"
	}
	env = append(env, "LANG="+lang)
	term := os.Getenv("TERM")
	if term == "" {
		term = "dumb"
	}
	env = append(env, "TERM="+term)
	env = append(env, "PYTHONUNBUFFERED=1")
	return env
}

// shellCommand is a resolved shell program.
type shellCommand struct {
	program string
}

// extraArgs builds the arguments following the shell program
// (shell.py:581-586): an optional login flag, then -c and the command.
func (s shellCommand) extraArgs(command string, login bool) []string {
	argv := make([]string, 0, 3)
	name := strings.ToLower(filepath.Base(s.program))
	if login && (name == "bash" || name == "zsh") {
		argv = append(argv, "-l")
	}
	return append(argv, "-c", command)
}

// resolveShell mirrors ExecTool._resolve_shell (shell.py:629-675) for POSIX
// shells. The Windows PowerShell/cmd branch is not ported.
func (t *Exec) resolveShell(shell string) (shellCommand, *tools.Result) {
	allowed := map[string]bool{"sh": true, "bash": true, "zsh": true}
	if shell == "" {
		if t.opts.Shell != "" {
			shell = t.opts.Shell
		} else {
			if resolved, err := exec.LookPath("bash"); err == nil {
				return shellCommand{program: resolved}, nil
			}
			return shellCommand{program: "/bin/bash"}, nil
		}
	}
	if strings.ContainsAny(shell, "\x00\n\r") {
		r := tools.Errf("Error: shell contains invalid characters")
		return shellCommand{}, &r
	}
	path := expandUser(shell)
	if filepath.IsAbs(path) {
		if !allowed[strings.ToLower(filepath.Base(path))] {
			r := tools.Errf("Error: unsupported shell '%s'. Allowed: bash, sh, zsh", shell)
			return shellCommand{}, &r
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			r := tools.Errf("Error: shell is not executable: %s", shell)
			return shellCommand{}, &r
		}
		return shellCommand{program: path}, nil
	}
	if strings.ContainsAny(shell, `/\`) {
		r := tools.Errf("Error: shell must be a shell name or absolute path")
		return shellCommand{}, &r
	}
	if !allowed[shell] {
		r := tools.Errf("Error: unsupported shell '%s'. Allowed: bash, sh, zsh", shell)
		return shellCommand{}, &r
	}
	resolved, err := exec.LookPath(shell)
	if err != nil {
		r := tools.Errf("Error: shell not found: %s", shell)
		return shellCommand{}, &r
	}
	return shellCommand{program: resolved}, nil
}

// cappedBuffer buffers at most limit bytes and counts what it discarded.
type cappedBuffer struct {
	limit   int
	buf     []byte
	dropped int64
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - len(b.buf); room > 0 {
		if len(p) <= room {
			b.buf = append(b.buf, p...)
			return len(p), nil
		}
		b.buf = append(b.buf, p[:room]...)
		b.dropped += int64(len(p) - room)
		return len(p), nil
	}
	b.dropped += int64(len(p))
	return len(p), nil
}

func (b *cappedBuffer) Len() int { return len(b.buf) }

func (b *cappedBuffer) String() string { return strings.ToValidUTF8(string(b.buf), "\uFFFD") }

// text renders the captured bytes and reports byte-level truncation, which the
// reference never has to do because it buffers everything (shell.py:325-334).
func (b *cappedBuffer) text(stream string) string {
	out := b.String()
	if b.dropped > 0 {
		out += fmt.Sprintf("\n\n... (%s truncated: %d bytes discarded, capture capped at %d bytes) ...",
			stream, b.dropped, b.limit)
	}
	return out
}

// truncateRunes mirrors the reference's head/tail truncation (shell.py:339-346).
// Python counts characters, so this counts runes.
func truncateRunes(result string, maxLen int) string {
	runes := []rune(result)
	if len(runes) <= maxLen {
		return result
	}
	half := maxLen / 2
	dropped := len(runes) - maxLen
	return string(runes[:half]) +
		fmt.Sprintf("\n\n... (%s chars truncated) ...\n\n", groupThousands(dropped)) +
		string(runes[len(runes)-half:])
}

// groupThousands renders an integer with comma separators, matching Python's
// f"{n:,}".
func groupThousands(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
		if len(s) > lead {
			b.WriteByte(',')
		}
	}
	for i := lead; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteByte(',')
		}
	}
	return b.String()
}

// clampInt mirrors clamp_session_int (exec_session.py:466-469) for a value that
// is present.
func clampInt(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}
