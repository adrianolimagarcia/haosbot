package builtin

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These tests answer a concrete operational question: can this exec tool drive
// real system administration — inspecting machine state and running privileged
// network tooling — or does the port's guard list block it?
//
// They run real commands. They are read-only and safe: nothing here modifies
// the machine.

func newExec(t *testing.T, opts ExecOptions) *Exec {
	t.Helper()
	return NewExec(opts)
}

func run(t *testing.T, tool *Exec, command string) (string, bool) {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"command": command})
	res, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute(%q): %v", command, err)
	}
	return res.Content, res.IsError
}

// TestExecRunsSystemInspectionCommands proves the tool can read machine state.
func TestExecRunsSystemInspectionCommands(t *testing.T) {
	tool := newExec(t, ExecOptions{})

	for _, cmd := range []string{
		"uname -a",
		"id",
		"ps aux | head -3",
		"df -h | head -3",
	} {
		out, isErr := run(t, tool, cmd)
		if isErr {
			t.Errorf("%q was blocked or failed: %s", cmd, out)
			continue
		}
		if strings.TrimSpace(out) == "" {
			t.Errorf("%q produced no output", cmd)
		}
	}
}

// TestExecDoesNotBlockNetworkAndFirewallTooling is the decisive test for the
// sysadmin use case: tcpdump, nmap and the firewall CLIs must NOT be caught by
// the deny-pattern filter. The filter targets destructive disk and power
// operations, not network tooling.
func TestExecDoesNotBlockNetworkAndFirewallTooling(t *testing.T) {
	tool := newExec(t, ExecOptions{})

	// Invoke each tool in a way that is harmless even if the binary exists:
	// --version / --help only print and exit.
	for _, cmd := range []string{
		"nmap --version",
		"tcpdump --version",
		"iptables --version",
		"nft --version",
		"ss -tuln | head -3",
		"ip -brief addr",
	} {
		out, isErr := run(t, tool, cmd)

		// A missing binary is a legitimate environment result, not a block.
		// What must NOT happen is the deny filter rejecting the command.
		if strings.Contains(out, "blocked by deny pattern") {
			t.Errorf("%q was blocked by the deny filter: %s", cmd, out)
			continue
		}
		if strings.Contains(out, "path traversal") {
			t.Errorf("%q was blocked by the traversal guard: %s", cmd, out)
			continue
		}
		if isErr {
			t.Logf("%q: not available in this environment (%s)",
				cmd, strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
			continue
		}
		t.Logf("%q: OK", cmd)
	}
}

// TestExecBlocklistStillCatchesDestructiveCommands verifies the guard that DOES
// exist still works, so the previous test is not passing merely because the
// filter is absent.
//
// This tests the PATTERN MATCHING directly and deliberately executes nothing.
// Running "rm -rf /" or "shutdown -h now" to prove they are blocked would mean
// that any bug in the filter destroys the machine or powers it off. A guard
// must be verified by inspecting the decision, not by attempting the act.
func TestExecBlocklistStillCatchesDestructiveCommands(t *testing.T) {
	blocked := func(command string) bool {
		lower := strings.ToLower(strings.TrimSpace(command))
		for _, re := range execDenyPatterns {
			if re.MatchString(lower) {
				return true
			}
		}
		return false
	}

	for _, cmd := range []string{
		"rm -rf /",
		"rm -rf ~/important",
		"mkfs.ext4 /dev/sda1",
		"shutdown -h now",
		"reboot",
		"poweroff",
		"dd if=/dev/zero of=/dev/sda",
		"echo x > /dev/sda",
		":(){ :|:& };:",
	} {
		if !blocked(cmd) {
			t.Errorf("%q is NOT matched by any deny pattern", cmd)
		}
	}

	// And the network/firewall tooling the sysadmin workflow needs must pass.
	for _, cmd := range []string{
		"nmap -sS 10.0.0.0/24",
		"tcpdump -i eth0 -w /var/tmp/cap.pcap",
		"iptables -L -n -v",
		"nft list ruleset",
		"systemctl status nginx",
		"python3 /opt/scripts/check.py",
	} {
		if blocked(cmd) {
			t.Errorf("%q is blocked but should not be", cmd)
		}
	}
}

// TestExecTimeoutKillsLongRunningCapture documents the hard limit that matters
// for tcpdump: a capture longer than MaxTimeout is killed. Long captures must
// therefore run detached, not as a single tool call.
func TestExecTimeoutKillsLongRunningCapture(t *testing.T) {
	tool := newExec(t, ExecOptions{
		DefaultTimeout: 1 * time.Second,
		MaxTimeout:     2 * time.Second,
	})

	start := time.Now()
	_, isErr := run(t, tool, "sleep 30")
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Errorf("timeout not enforced: command ran for %s", elapsed)
	}
	if !isErr {
		t.Error("a timed-out command should report an error")
	}
	t.Logf("sleep 30 was terminated after %s", elapsed.Round(time.Millisecond))
}

// TestExecRunsPythonScripts proves the "create and run a Python script" part of
// the workflow.
func TestExecRunsPythonScripts(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	tool := newExec(t, ExecOptions{})

	out, isErr := run(t, tool, python+" -c \"print('hello from python')\"")
	if isErr {
		t.Fatalf("python invocation failed: %s", out)
	}
	if !strings.Contains(out, "hello from python") {
		t.Errorf("unexpected output: %q", out)
	}
}

// TestExecRunsAsInvokingUser records which identity the child process gets.
// The tool does not elevate: it runs as whoever nanobot-go runs as. That is the
// decisive constraint for tcpdump, nmap SYN scans and firewall edits, all of
// which need root or CAP_NET_RAW.
func TestExecRunsAsInvokingUser(t *testing.T) {
	tool := newExec(t, ExecOptions{})
	out, _ := run(t, tool, "id -un")

	// The tool appends "\nExit code: N" to every result, exactly as the
	// reference does (shell.py:335-337). So the output is not a bare username;
	// the first line is.
	firstLine := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
	t.Logf("exec runs as: %q (full result: %q)", firstLine, out)

	if !strings.Contains(out, "Exit code:") {
		t.Error("the exit-code suffix is missing; output format diverged from the reference")
	}

	if firstLine == "root" {
		t.Log("ROOT: tcpdump, nmap SYN scans and firewall edits would work")
	} else {
		t.Logf("NOT root (%s): tcpdump, nmap SYN scans and firewall edits would fail with EPERM", firstLine)
	}
}
