package lakehouse

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	icebergio "github.com/apache/iceberg-go/io"
	"github.com/apache/iceberg-go/table"
)

const vacuumStamp = "vacuum.stamp"

func (s *Store) stampVacuum(ns string, done bool) error {
	path := filepath.Join(s.dir, namespacePath(ns), vacuumStamp)
	n, _ := readStamp(path)
	switch {
	case !done && n%2 == 0:
		n++
	case !done:
		n += 2
	case n%2 == 1:
		n++
	}
	return os.WriteFile(path, []byte(strconv.FormatInt(n, 10)), 0o600)
}

func readStamp(path string) (int64, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
}

type backupFS struct {
	localFS
	live, out string
}

func (f backupFS) Open(name string) (icebergio.File, error) {
	rel, err := filepath.Rel(f.live, localPath(name))
	if err != nil || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("%s lies outside the data directory %s", name, f.live)
	}
	return f.localFS.LocalFS.Open(filepath.Join(f.out, rel))
}

func unreachable(live, outDir string, locations []string) (string, error) {
	fsys := backupFS{live: live, out: outDir}
	check := func(name string) (bool, error) {
		file, err := fsys.Open(name)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, file.Close()
	}
	for _, loc := range locations {
		file, err := fsys.Open(loc)
		if errors.Is(err, fs.ErrNotExist) {
			return loc, nil
		}
		if err != nil {
			return "", err
		}
		meta, err := table.ParseMetadata(file)
		file.Close()
		if err != nil {
			return "", err
		}
		for _, snap := range meta.Snapshots() {
			if ok, err := check(snap.ManifestList); err != nil || !ok {
				return snap.ManifestList, err
			}
			manifests, err := snap.Manifests(fsys)
			if err != nil {
				return "", err
			}
			for _, mf := range manifests {
				if ok, err := check(mf.FilePath()); err != nil || !ok {
					return mf.FilePath(), err
				}
				entries, err := mf.FetchEntries(fsys, true)
				if err != nil {
					return "", err
				}
				for _, e := range entries {
					if ok, err := check(e.DataFile().FilePath()); err != nil || !ok {
						return e.DataFile().FilePath(), err
					}
				}
			}
		}
	}
	return "", nil
}
