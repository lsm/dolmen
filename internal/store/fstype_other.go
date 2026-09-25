//go:build !linux && !darwin && !windows

package store

func networkFilesystem(string) (string, bool) {
	return "", false
}
