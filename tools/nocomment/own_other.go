//go:build !unix

package main

import "os"

func fileOwner(info os.FileInfo) (int, int, bool) {
	return 0, 0, false
}

func preserveOwner(path string, info os.FileInfo) error {
	return nil
}
