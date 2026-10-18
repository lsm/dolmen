package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lsm/dolmen/internal/files"
	"github.com/lsm/dolmen/internal/files/s3store"
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

func checkFilesStore(kind, bucket string, cfg files.Config) error {
	switch kind {
	case "local":
		return nil
	case "s3":
	default:
		return fmt.Errorf("-files-store must be local or s3, got %q", kind)
	}
	if bucket == "" {
		return fmt.Errorf("-files-store s3 needs -files-s3-bucket (DOLMEN_FILES_S3_BUCKET)")
	}
	if cfg.MaxNamespaceFiles > 0 {
		return fmt.Errorf("-max-namespace-files-size needs the local file store: servers sharing an S3 bucket cannot keep one namespace's total exact; unset it, or use -files-store local")
	}
	if cfg.MaxFileSize > s3store.MaxFileSize {
		return fmt.Errorf("-max-file-size is above S3's 5TiB object limit; lower it to 5TiB or less with -files-store s3")
	}
	return nil
}

func localFilesOnly(getenv func(string) string, dataDir, what string) error {
	inS3, err := files.UsesS3(dataDir)
	if err != nil {
		return err
	}
	if getenv("DOLMEN_FILES_STORE") == "s3" || inS3 {
		return fmt.Errorf("%s copies files through the data directory, but this deployment keeps them in an S3 bucket (-files-store s3); back up the bucket with your own S3 tooling, and use this command only with the local file store", what)
	}
	return nil
}

func newFileStore(cfg *config) (*files.Store, error) {
	c := cfg.Files
	c.Root = cfg.DataDir
	if cfg.FilesStore == "s3" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		blobs, err := s3store.New(ctx, cfg.FilesS3)
		if err != nil {
			return nil, err
		}
		if err := files.MarkS3(cfg.DataDir); err != nil {
			return nil, err
		}
		c.Blobs = blobs
	}
	return files.New(c), nil
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
