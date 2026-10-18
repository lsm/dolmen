package files

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const storeMarkerFile = "files.store"

func MarkS3(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, storeMarkerFile), []byte("s3\n"), 0o644)
}

func UsesS3(dataDir string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, storeMarkerFile))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(b)) == "s3", nil
}
