//go:build !unix

package gitstore

import "os"

// statInfo carries the POSIX fields git records in an index entry. On platforms
// without them (Windows) only the modification time and size are available; the
// reference runs on POSIX, so this path exists only to keep the package
// buildable elsewhere.
type statInfo struct {
	ctimeSec, ctimeNsec uint32
	mtimeSec, mtimeNsec uint32
	dev, ino            uint32
	uid, gid            uint32
	size                uint32
}

func statOf(fi os.FileInfo) statInfo {
	return statInfo{
		mtimeSec:  uint32(fi.ModTime().Unix()),
		mtimeNsec: uint32(fi.ModTime().Nanosecond()),
		size:      uint32(fi.Size()),
	}
}
