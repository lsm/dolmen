package main

import (
	"io"
	"strings"
	"testing"
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
