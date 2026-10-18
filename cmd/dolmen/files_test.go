package main

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestFileSizeFlags(t *testing.T) {
	env := map[string]string{"DOLMEN_MAX_NAMESPACE_FILES_SIZE": "5GiB"}
	getenv := func(k string) string { return env[k] }
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	cfg, err := loadConfig([]string{"-max-file-size", "10MiB", "-max-request-files-size", "20MiB", "-max-pending-files-size", "1GiB"}, getenv, lookup, io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Files.MaxFileSize != 10<<20 || cfg.Files.MaxNamespaceFiles != 5<<30 || cfg.Files.MaxRequestFiles != 20<<20 || cfg.Files.MaxPendingFiles != 1<<30 {
		t.Fatalf("files config = %+v", cfg.Files)
	}
	for _, flag := range []string{"-max-file-size", "-max-request-files-size", "-max-pending-files-size"} {
		_, err := loadConfig([]string{flag, "0"}, getenv, lookup, io.Discard, false)
		if err == nil || !strings.Contains(err.Error(), flag+" must be greater than 0") {
			t.Errorf("%s 0: %v", flag, err)
		}
	}
}

func TestFileFetchFlags(t *testing.T) {
	env := map[string]string{"DOLMEN_FILES_FETCH_HOSTS": " files.example.com, *.cdn.example.org ,"}
	getenv := func(k string) string { return env[k] }
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	cfg, err := loadConfig([]string{"-files-fetch-timeout", "30s"}, getenv, lookup, io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Files.FetchHosts, "|") != "files.example.com|*.cdn.example.org" || cfg.Files.FetchTimeout != 30*time.Second {
		t.Fatalf("fetch config = %v %s", cfg.Files.FetchHosts, cfg.Files.FetchTimeout)
	}
	if _, err := loadConfig([]string{"-files-fetch-timeout", "0"}, getenv, lookup, io.Discard, false); err == nil || !strings.Contains(err.Error(), "-files-fetch-timeout") {
		t.Fatalf("a zero fetch timeout must be refused: %v", err)
	}
}

func TestFileStoreFlags(t *testing.T) {
	env := map[string]string{"DOLMEN_FILES_S3_BUCKET": "blobs", "DOLMEN_FILES_S3_PATH_STYLE": "true", "DOLMEN_FILES_S3_PUBLIC_ENDPOINT": "https://files.example.com"}
	getenv := func(k string) string { return env[k] }
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	cfg, err := loadConfig([]string{"-files-store", "s3", "-files-s3-endpoint", "http://127.0.0.1:9000", "-files-s3-prefix", "prod/"}, getenv, lookup, io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FilesStore != "s3" || cfg.FilesS3.Bucket != "blobs" || !cfg.FilesS3.PathStyle || cfg.FilesS3.Endpoint != "http://127.0.0.1:9000" || cfg.FilesS3.Prefix != "prod/" || cfg.FilesS3.PublicEndpoint != "https://files.example.com" {
		t.Fatalf("s3 config = %s %+v", cfg.FilesStore, cfg.FilesS3)
	}
	none := func(string) string { return "" }
	nolookup := func(string) (string, bool) { return "", false }
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-files-store", "gcs"}, "-files-store must be local or s3"},
		{[]string{"-files-store", "s3"}, "needs -files-s3-bucket"},
		{[]string{"-files-store", "s3", "-files-s3-bucket", "b", "-max-namespace-files-size", "1GiB"}, "-max-namespace-files-size needs the local file store"},
		{[]string{"-files-store", "s3", "-files-s3-bucket", "b", "-max-file-size", "6TiB"}, "S3's 5TiB object limit"},
	} {
		if _, err := loadConfig(c.args, none, nolookup, io.Discard, false); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
	}
}
