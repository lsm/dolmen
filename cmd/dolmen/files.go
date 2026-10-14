package main

import (
	"fmt"

	"github.com/lsm/dolmen/internal/files"
	"github.com/lsm/dolmen/internal/store"
)

func parseFilesConfig(maxFile, maxNamespace, maxRequest, maxPending string) (files.Config, error) {
	var cfg files.Config
	for _, f := range []struct {
		flag string
		raw  string
		dst  *int64
		zero bool
	}{
		{"-max-file-size", maxFile, &cfg.MaxFileSize, false},
		{"-max-namespace-files-size", maxNamespace, &cfg.MaxNamespaceFiles, true},
		{"-max-request-files-size", maxRequest, &cfg.MaxRequestFiles, false},
		{"-max-pending-files-size", maxPending, &cfg.MaxPendingFiles, false},
	} {
		n, err := store.ParseSize(f.raw)
		if err != nil {
			return files.Config{}, fmt.Errorf("%s: %w", f.flag, err)
		}
		if n == 0 && !f.zero {
			return files.Config{}, fmt.Errorf("%s must be greater than 0", f.flag)
		}
		*f.dst = n
	}
	return cfg, nil
}

func newFileStore(cfg *config) *files.Store {
	c := cfg.Files
	c.Root = cfg.DataDir
	return files.New(c)
}
