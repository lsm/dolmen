package store

import "syscall"

var networkMagic = map[int64]string{
	0x6969:     "nfs",
	0x517b:     "smb",
	0xff534d42: "cifs",
	0xfe534d42: "smb2",
	0x65735546: "fuse",
	0x01021997: "9p",
	0x564c:     "ncp",
	0x73757245: "coda",
	0x47504653: "gpfs",
	0x0bd00bd0: "lustre",
	0x00c36400: "ceph",
	0x6b414653: "afs",
	0x5346414f: "afs",
}

func networkFilesystem(dir string) (string, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return "", false
	}
	name, ok := networkMagic[int64(st.Type)]
	return name, ok
}
