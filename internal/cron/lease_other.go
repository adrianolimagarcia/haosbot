//go:build !unix

package cron

import (
	"fmt"
	"os"
)

type processLease struct {
	path string
	f    *os.File
}

func acquireProcessLease(path string) (*processLease, error) {
	if err := os.MkdirAll(filepathDir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, ErrLeaseHeld
		}
		return nil, err
	}
	_, _ = fmt.Fprintf(f, "pid=%d\n", os.Getpid())
	_ = f.Sync()
	return &processLease{path: path, f: f}, nil
}

func (l *processLease) release() {
	if l == nil {
		return
	}
	if l.f != nil {
		_ = l.f.Close()
	}
	if l.path != "" {
		_ = os.Remove(l.path)
	}
	l.f = nil
}
