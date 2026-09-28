//go:build !windows

package store

import "os"

func secureDir(path string) error {
	return os.Chmod(path, 0o700)
}
