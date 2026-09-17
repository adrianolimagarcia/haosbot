package builtin

import (
	"os"
	"path/filepath"
	"testing"
)

// mkPythonMin creates <home>/<dir>/python-min/bin/python3 and returns its path.
// It stands in for the minimal CPython runtime that scripts/build_cpython_min.sh
// installs.
func mkPythonMin(t *testing.T, home, dir string) string {
	t.Helper()
	p := filepath.Join(home, dir, "python-min", "bin", "python3")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPythonMinBin checks that python_exec locates the minimal CPython runtime
// through the project's branding rule ("prefer ~/.haosbot, fall back to a legacy
// ~/.nanobot") instead of the legacy directory hardcoded at python_exec.go:69.
//
// The Python reference has no minimal-CPython runtime at all — `grep -rn
// "python-min" upstream/nanobot` returns nothing — so this tool is a Go-port
// addition and the branding rule from internal/config/paths.go is the only
// authority for where its interpreter lives.
func TestPythonMinBin(t *testing.T) {
	t.Run("branded ~/.haosbot is preferred", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		want := mkPythonMin(t, home, ".haosbot")

		if got := pythonMinBin(); got != want {
			t.Errorf("pythonMinBin() = %q, want the branded interpreter %q", got, want)
		}
	})

	t.Run("both present prefers the branded interpreter", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		mkPythonMin(t, home, ".nanobot")
		want := mkPythonMin(t, home, ".haosbot")

		if got := pythonMinBin(); got != want {
			t.Errorf("pythonMinBin() = %q, want the branded interpreter %q", got, want)
		}
	})

	t.Run("legacy ~/.nanobot is still found", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		want := mkPythonMin(t, home, ".nanobot")

		if got := pythonMinBin(); got != want {
			t.Errorf("pythonMinBin() = %q, want the legacy interpreter %q", got, want)
		}
	})

	// A machine that already has ~/.nanobot/python-min and then gets a fresh
	// ~/.haosbot must keep using the interpreter that exists, rather than
	// silently dropping to the system python3.
	t.Run("legacy interpreter found next to a fresh ~/.haosbot", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if err := os.MkdirAll(filepath.Join(home, ".haosbot"), 0o755); err != nil {
			t.Fatal(err)
		}
		want := mkPythonMin(t, home, ".nanobot")

		if got := pythonMinBin(); got != want {
			t.Errorf("pythonMinBin() = %q, want the legacy interpreter %q", got, want)
		}
	})

	t.Run("no interpreter falls back to system python3", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		if got := pythonMinBin(); got != "python3" {
			t.Errorf("pythonMinBin() = %q, want the system fallback %q", got, "python3")
		}
	})
}
