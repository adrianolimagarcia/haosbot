//go:build unix

package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPythonExecRunsBrandedInterpreter guards the CALL SITE, not just the
// selection helper: Execute used to build the interpreter path inline from the
// legacy literal, so a fix that only added pythonMinBin without wiring it in
// would still pass TestPythonMinBin. The stand-in interpreter ignores "-c" and
// prints a marker, so the assertion fails whenever any other interpreter runs.
func TestPythonExecRunsBrandedInterpreter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	pyBin := filepath.Join(home, ".haosbot", "python-min", "bin", "python3")
	if err := os.MkdirAll(filepath.Dir(pyBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pyBin, []byte("#!/bin/sh\necho branded-interpreter\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	tool := NewPythonExec("")
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"code":"print(1)"}`))
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !strings.Contains(res.Content, "branded-interpreter") {
		t.Errorf("python_exec did not run the branded interpreter; output = %q", res.Content)
	}
}
