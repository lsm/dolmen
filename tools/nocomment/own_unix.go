//go:build unix

package main

import (
	"os"
	"syscall"
)

func fileOwner(info os.FileInfo) (int, int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

func preserveOwner(path string, info os.FileInfo) error {
	uid, gid, ok := fileOwner(info)
	if !ok {
		return nil
	}
	return os.Chown(path, uid, gid)
}
