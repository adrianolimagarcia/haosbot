//go:build unix

package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

// TestReadFileRefusesFIFO guards against a hang: Path.is_file() is S_ISREG, so
// the reference refuses anything that is not a regular file
// (filesystem.py:318-319) instead of blocking on an open() that never returns.
func TestReadFileRefusesFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	type outcome struct {
		res tools.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := NewReadFile(PathPolicy{}).Execute(context.Background(),
			json.RawMessage(`{"path":`+quote(fifo)+`}`))
		done <- outcome{res, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("unexpected error: %v", got.err)
		}
		if !got.res.IsError || !strings.Contains(got.res.Content, "Not a file") {
			t.Fatalf("result = %+v, want a 'Not a file' error", got.res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("read_file blocked on a FIFO instead of refusing it")
	}
}

func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
