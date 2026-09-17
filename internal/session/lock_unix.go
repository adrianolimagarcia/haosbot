//go:build unix

package session

import (
	"errors"
	"os"
	"syscall"
)

// errFlockBusy reports that the lock is held by someone else. It is returned
// for EWOULDBLOCK/EAGAIN so acquireFileLock can retry until its deadline.
var errFlockBusy = errors.New("session: lock is held")

// tryFlock takes an exclusive flock without blocking.
func tryFlock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errFlockBusy
	}
	return err
}

// funlock releases the flock.
func funlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
