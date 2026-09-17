//go:build unix

package builtin

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// configureProcessTree places the child in its own process group and installs a
// cancel function that kills the whole group.
//
// Mirrors the reference's start_new_session=True plus os.killpg
// (nanobot/agent/tools/shell.py:595, :718), so a timed-out or canceled command
// cannot leave orphaned descendants behind. WaitDelay mirrors the reference's
// 5s wait after killing (shell.py:691-693, :725-726).
func configureProcessTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		// Negative pid targets the process group created by Setpgid.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
}

// signalExitCode returns the negative signal number when the process was killed
// by a signal, matching Python's Popen.returncode convention (shell.py:335).
func signalExitCode(state *os.ProcessState) (int, bool) {
	if state == nil {
		return 0, false
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -int(ws.Signal()), true
	}
	return 0, false
}
