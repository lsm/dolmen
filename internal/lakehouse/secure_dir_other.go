//go:build !windows

package lakehouse

import "os"

func secureDir(path string) error {
	return os.Chmod(path, 0o700)
}

func (s *Store) renameNamespace(old, next string) error { return s.root.Rename(old, next) }

func (s *Store) syncDirectory(path string) error {
	f, err := s.root.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
