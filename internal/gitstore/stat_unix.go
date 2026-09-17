//go:build unix

package gitstore

import (
	"os"
	"syscall"
)

// statInfo carries the POSIX fields git records in an index entry.
type statInfo struct {
	ctimeSec, ctimeNsec uint32
	mtimeSec, mtimeNsec uint32
	dev, ino            uint32
	uid, gid            uint32
	size                uint32
}

// statOf extracts the index-relevant fields of an lstat result. dulwich reads
// the same fields, using st_ctime_ns/st_mtime_ns when Python exposes them
// (dulwich/index.py:index_entry_from_stat), so nanosecond precision is kept.
func statOf(fi os.FileInfo) statInfo {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return statInfo{
			mtimeSec:  uint32(fi.ModTime().Unix()),
			mtimeNsec: uint32(fi.ModTime().Nanosecond()),
			size:      uint32(fi.Size()),
		}
	}
	return statInfo{
		ctimeSec:  uint32(st.Ctim.Sec),
		ctimeNsec: uint32(st.Ctim.Nsec),
		mtimeSec:  uint32(st.Mtim.Sec),
		mtimeNsec: uint32(st.Mtim.Nsec),
		dev:       uint32(st.Dev),
		ino:       uint32(st.Ino),
		uid:       st.Uid,
		gid:       st.Gid,
		size:      uint32(st.Size),
	}
}
