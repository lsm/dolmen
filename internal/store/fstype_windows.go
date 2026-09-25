package store

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func networkFilesystem(dir string) (string, bool) {
	vol := filepath.VolumeName(dir)
	if strings.HasPrefix(vol, `\\`) {
		return "smb", true
	}
	root, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return "", false
	}
	if windows.GetDriveType(root) == windows.DRIVE_REMOTE {
		return "network drive", true
	}
	return "", false
}
