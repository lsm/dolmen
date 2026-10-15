package main

import (
	"fmt"
	"strings"
	"time"

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

func parseFetchConfig(hosts, rawTimeout string) ([]string, time.Duration, error) {
	timeout, err := time.ParseDuration(rawTimeout)
	if err != nil || timeout < time.Second || timeout > 24*time.Hour {
		return nil, 0, fmt.Errorf("-files-fetch-timeout must be between 1s and 24h")
	}
	var out []string
	for _, h := range strings.Split(hosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out, timeout, nil
}
