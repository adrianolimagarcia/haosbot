package session

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Cross-process file locking.
//
// The reference guards every read and write of the session directory with
// filelock.FileLock("<sessions_dir>/.session-files.lock") (manager.py:581-583)
// and workspace-id allocation with "<root>/.workspace-migration.lock" and a
// 30 s timeout (manager.py:568-571, :74).
//
// filelock uses fcntl.flock on POSIX, so an OS advisory lock on the same path
// is protocol-compatible: a Go writer and a Python writer cannot interleave a
// read-modify-write cycle. Without it, both sides rewrite the whole file and
// the last os.replace() silently wins, losing the other writer's messages.
//
// Locks are also serialised inside this process, because flock() is per open
// file description: two file descriptors on the same path conflict even within
// one process. The per-path mutex below is what makes concurrent Save calls
// from several goroutines safe (and it is what keeps -race runs deterministic).

var (
	lockRegistryMu sync.Mutex
	lockRegistry   = map[string]*sync.Mutex{}
)

// pathMutex returns the process-wide mutex for an absolute lock path.
func pathMutex(path string) *sync.Mutex {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	lockRegistryMu.Lock()
	defer lockRegistryMu.Unlock()
	mu, ok := lockRegistry[abs]
	if !ok {
		mu = &sync.Mutex{}
		lockRegistry[abs] = mu
	}
	return mu
}

// fileLock is an exclusive advisory lock on a lock file.
//
// Acquire is not re-entrant: a single goroutine that nests two Acquire calls
// on the same path deadlocks. The store never nests them (every locked entry
// point calls an *Unlocked helper), matching the reference's structure where
// _load_unlocked may call _repair_unlocked while the lock is held.
type fileLock struct {
	mu   *sync.Mutex
	path string
	f    *os.File
}

// acquireFileLock takes the lock, waiting up to timeout (timeout <= 0 waits
// forever) and returns once it is held.
func acquireFileLock(path string, timeout time.Duration) (*fileLock, error) {
	mu := pathMutex(path)
	mu.Lock()

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		mu.Unlock()
		return nil, err
	}

	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		err := tryFlock(f)
		if err == nil {
			return &fileLock{mu: mu, path: path, f: f}, nil
		}
		if !errors.Is(err, errFlockBusy) {
			f.Close()
			mu.Unlock()
			return nil, err
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			f.Close()
			mu.Unlock()
			return nil, errors.New("session: timed out acquiring " + path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// release drops the lock.
func (l *fileLock) release() {
	if l == nil {
		return
	}
	if l.f != nil {
		_ = funlock(l.f)
		_ = l.f.Close()
		l.f = nil
	}
	l.mu.Unlock()
}
