//go:build !unix

package session

import (
	"errors"
	"os"
)

// errFlockBusy is never returned on this platform: there is no OS advisory
// lock, so only the in-process mutex serialises writers. This keeps the
// package buildable on non-Unix targets without pretending to offer the
// cross-process guarantee that filelock provides on POSIX.
var errFlockBusy = errors.New("session: lock is held")

func tryFlock(f *os.File) error { return nil }

func funlock(f *os.File) error { return nil }
