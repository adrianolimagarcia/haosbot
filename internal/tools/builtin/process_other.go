//go:build !unix

package builtin

import (
	"os"
	"os/exec"
	"time"
)

// configureProcessTree keeps the default cancel behavior (kill the direct
// child) on platforms without POSIX process groups, and bounds how long Wait
// blocks after a cancel.
//
// UNVERIFIED: this port's process-tree kill is only exercised on Unix
// (see process_unix.go). On Windows the reference uses a Job Object
// (shell.py:696-731); that is not ported.
func configureProcessTree(cmd *exec.Cmd) {
	cmd.WaitDelay = 5 * time.Second
}

// signalExitCode reports that signal-based exit codes are unavailable.
func signalExitCode(*os.ProcessState) (int, bool) { return 0, false }
