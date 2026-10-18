//go:build windows

package store

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func openShared(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(extendedPath(path))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

func extendedPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil || strings.HasPrefix(abs, `\\?\`) {
		return path
	}
	if rest, ok := strings.CutPrefix(abs, `\\`); ok {
		return `\\?\UNC\` + rest
	}
	return `\\?\` + abs
}
