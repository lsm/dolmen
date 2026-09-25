package store

import (
	"strings"
	"syscall"
)

var networkTypes = map[string]bool{"nfs": true, "smbfs": true, "afpfs": true, "webdav": true, "cifs": true, "osxfuse": true, "macfuse": true, "fuse": true}

func networkFilesystem(dir string) (string, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return "", false
	}
	b := make([]byte, 0, len(st.Fstypename))
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	name := strings.ToLower(string(b))
	return name, networkTypes[name]
}
