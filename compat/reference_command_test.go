// Bounded execution of the external oracles the compat package compares
// against.
//
// Every differential test here shells out: to `.tools/venv/bin/python` running
// one of compat/python/dump_*.py, or to `git` for the git-store oracle. Those
// subprocesses are the only unbounded wait in the package. `exec.Command` +
// `cmd.Output()` has no deadline, so a dumper that stalls for any reason — a
// starved scheduler on a loaded machine, a blocked filesystem read, a
// reference import that waits on something outside this repo — blocks the test
// goroutine forever.
//
// The failure mode that matters is not the delay itself, it is the reporting.
// With no per-command bound the only backstop is `go test -timeout`, which
// kills the whole test binary: the output is a goroutine dump that names no
// dumper, every test that had not run yet is reported as if it had failed, and
// the result reads like a regression in the port. That is exactly what happened
// to TestDeliveryPolicyDifferential, which was reported as hung for over three
// minutes in a run where the same dumper, timed directly, takes 13-16 s.
//
// runReferenceCommand therefore puts a deadline on each oracle invocation and
// reports a timeout as a timeout, naming the dumper and showing its stderr.
package compat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"
	"time"
)

// referenceDumpTimeout bounds one oracle invocation.
//
// The slowest dumper measured on this host takes ~16 s cold. 120 s is ~8x that
// headroom, which absorbs a loaded machine without letting a genuinely stuck
// dumper hold the package anywhere near the suite's own `go test -timeout`.
const referenceDumpTimeout = 120 * time.Second

// referenceDumpWaitDelay bounds the wait AFTER the deadline fires.
//
// Killing the direct child is not enough to unblock `Output()`: the pipes it
// wrote to stay open as long as any process still holds them, so a dumper that
// spawned a child of its own would keep the read blocked past the deadline.
// WaitDelay closes them and lets Wait return.
const referenceDumpWaitDelay = 5 * time.Second

// ReferenceTimeoutError reports that an oracle was killed for exceeding its
// deadline. It is a distinct type so a caller can tell a stalled oracle apart
// from one that ran and exited non-zero.
type ReferenceTimeoutError struct {
	Label   string
	Timeout time.Duration
	Stderr  string
}

func (e *ReferenceTimeoutError) Error() string {
	return fmt.Sprintf("%s did not finish within %s and was killed; stderr:\n%s",
		e.Label, e.Timeout, e.Stderr)
}

// runReferenceCommand runs argv with the given working directory and returns
// its stdout, under the package's standard oracle bounds.
//
// env replaces the child environment when non-nil, matching the sites that
// append a variable to os.Environ(). Stderr is captured and folded into the
// returned error, so callers no longer need to reach into *exec.ExitError
// (setting cmd.Stderr is what empties ExitError.Stderr, and the previous call
// sites all read it).
func runReferenceCommand(label string, argv []string, dir string, env []string) ([]byte, error) {
	return runBoundedCommand(label, argv, dir, env, referenceDumpTimeout, referenceDumpWaitDelay)
}

// runBoundedCommand is runReferenceCommand with explicit bounds, so the
// mechanism itself is testable without waiting two minutes.
func runBoundedCommand(label string, argv []string, dir string, env []string, timeout, waitDelay time.Duration) ([]byte, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("%s: no command given", label)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	cmd.WaitDelay = waitDelay

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &ReferenceTimeoutError{
				Label:   label,
				Timeout: timeout,
				Stderr:  stderr.String(),
			}
		}
		return nil, fmt.Errorf("%w\nstderr:\n%s", err, stderr.String())
	}
	return out, nil
}

// requireShell skips a test when /bin/sh is unavailable.
func requireShell(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("SKIP: /bin/sh not available")
	}
}

// TestBoundedCommandKillsAHangingOracle is the regression test for the reported
// hang: an oracle that never returns must be killed at its deadline and
// reported as a timeout that names it, not left to block the package.
func TestBoundedCommandKillsAHangingOracle(t *testing.T) {
	requireShell(t)

	const bound = 1 * time.Second
	start := time.Now()
	_, err := runBoundedCommand("hanging oracle", []string{"/bin/sh", "-c", "sleep 300"},
		t.TempDir(), nil, bound, time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("a command that never exits returned no error after %s", elapsed)
	}
	var timeoutErr *ReferenceTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error is %T (%v), want *ReferenceTimeoutError", err, err)
	}
	if timeoutErr.Label != "hanging oracle" {
		t.Errorf("timeout error names %q, want the label %q", timeoutErr.Label, "hanging oracle")
	}
	// The bound is 1 s; anything close to the 300 s the command asked for means
	// the deadline did not fire.
	if elapsed > 30*time.Second {
		t.Fatalf("deadline did not fire: command took %s", elapsed)
	}
}

// TestBoundedCommandDoesNotOutliveAChildHoldingStdout covers the subtle half of
// the bound. Killing the direct child does not close a stdout pipe that a
// grandchild inherited, so `Output()` can stay blocked on I/O rather than on
// the process. WaitDelay is what caps that: the call is bounded by
// timeout+waitDelay no matter what the child leaves behind.
//
// The delay is load-bearing and measured: with waitDelay raised to 60 s this
// same call took 61.1 s, tracking the delay rather than the ~20 s the orphan
// actually lives for. Without it a dumper that forks would keep the package
// blocked and the suite would still hang.
func TestBoundedCommandDoesNotOutliveAChildHoldingStdout(t *testing.T) {
	requireShell(t)

	const bound = 1 * time.Second
	const waitDelay = 1 * time.Second
	// The background sleep inherits stdout and outlives the shell that spawned
	// it. It is kept short so the test does not leave a long-lived orphan.
	start := time.Now()
	_, err := runBoundedCommand("oracle with a lingering child",
		[]string{"/bin/sh", "-c", "sleep 20 & sleep 300"},
		t.TempDir(), nil, bound, waitDelay)
	elapsed := time.Since(start)

	var timeoutErr *ReferenceTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error is %T (%v), want *ReferenceTimeoutError", err, err)
	}
	// 3 s of slack over the 2 s the two bounds add up to.
	if elapsed > bound+waitDelay+3*time.Second {
		t.Fatalf("call took %s, want it bounded by timeout+waitDelay = %s — "+
			"the lingering child is holding the pipe past the deadline",
			elapsed, bound+waitDelay)
	}
}

// TestBoundedCommandKeepsStderrOutOfStdout pins the property every JSON dumper
// depends on: loguru and other reference diagnostics go to stderr, and the
// parser must still see exactly one JSON document on stdout.
func TestBoundedCommandKeepsStderrOutOfStdout(t *testing.T) {
	requireShell(t)

	out, err := runBoundedCommand("noisy oracle",
		[]string{"/bin/sh", "-c", `printf '{"ok":true}\n'; printf 'loguru noise\n' >&2`},
		t.TempDir(), nil, 30*time.Second, time.Second)
	if err != nil {
		t.Fatalf("noisy oracle failed: %v", err)
	}
	if got, want := string(out), "{\"ok\":true}\n"; got != want {
		t.Fatalf("stdout = %q, want %q — stderr leaked into the parsed stream", got, want)
	}
}

// TestBoundedCommandReportsStderrOnFailure checks that a dumper which exits
// non-zero still surfaces why, which is what makes a broken reference
// actionable rather than a bare "exit status 1".
func TestBoundedCommandReportsStderrOnFailure(t *testing.T) {
	requireShell(t)

	_, err := runBoundedCommand("failing oracle",
		[]string{"/bin/sh", "-c", `printf 'traceback: boom\n' >&2; exit 3`},
		t.TempDir(), nil, 30*time.Second, time.Second)
	if err == nil {
		t.Fatal("a command exiting non-zero returned no error")
	}
	var timeoutErr *ReferenceTimeoutError
	if errors.As(err, &timeoutErr) {
		t.Fatalf("a fast non-zero exit was misreported as a timeout: %v", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("traceback: boom")) {
		t.Fatalf("error %q does not carry the child's stderr", err)
	}
}

// TestBoundedCommandHonoursEnv guards the sites that pass a modified
// environment (the legacy-history dumper pins a UTC offset this way).
func TestBoundedCommandHonoursEnv(t *testing.T) {
	requireShell(t)

	out, err := runBoundedCommand("env oracle",
		[]string{"/bin/sh", "-c", "printf %s \"$COMPAT_PROBE\""},
		t.TempDir(), []string{"COMPAT_PROBE=present", "PATH=/usr/bin:/bin"},
		30*time.Second, time.Second)
	if err != nil {
		t.Fatalf("env oracle failed: %v", err)
	}
	if string(out) != "present" {
		t.Fatalf("stdout = %q, want %q", out, "present")
	}
}
