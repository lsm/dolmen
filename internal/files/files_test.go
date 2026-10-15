package files

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPendingBytesAreReleasedWhenARequestEnds(t *testing.T) {
	s := New(Config{Root: t.TempDir(), MaxPendingFiles: 10})
	for i := 0; i < 3; i++ {
		r := s.NewRequest()
		if err := r.Receive("f", "a.bin", "", strings.NewReader("12345678")); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		r.Close()
	}
	r := s.NewRequest()
	defer r.Close()
	if err := r.Receive("f", "a.bin", "", strings.NewReader("12345678")); err != nil {
		t.Fatalf("pending bytes leaked across requests: %v", err)
	}
	if err := r.Receive("g", "b.bin", "", strings.NewReader("123")); err == nil {
		t.Fatal("a second part past the pending limit must be refused")
	}
}

func TestARequestCleansUpItsTemporaryFiles(t *testing.T) {
	root := t.TempDir()
	s := New(Config{Root: root})
	r := s.NewRequest()
	if err := r.Receive("f", "a.txt", "text/plain", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	b, err := r.Describe("f")
	if err != nil || b.Size != 5 || b.MediaType != "text/plain" || b.Name != "a.txt" || len(b.SHA256) != 64 {
		t.Fatalf("Describe = %+v, %v", b, err)
	}
	if _, err := r.Describe("g"); err == nil || !strings.Contains(err.Error(), `"f"`) {
		t.Fatalf("a missing part should name the parts the request carries, got %v", err)
	}
	r.Close()
	if entries, _ := os.ReadDir(filepath.Join(root, tmpDirName)); len(entries) != 0 {
		t.Fatalf("temporary files left: %v", entries)
	}
}

func TestPartLimits(t *testing.T) {
	s := New(Config{Root: t.TempDir(), MaxFileSize: 4, MaxRequestFiles: 6})
	r := s.NewRequest()
	defer r.Close()
	if err := r.Receive("a", "a", "", strings.NewReader("12345")); err == nil || !strings.Contains(err.Error(), "-max-file-size") {
		t.Fatalf("over -max-file-size: %v", err)
	}
	r2 := s.NewRequest()
	defer r2.Close()
	if err := r2.Receive("a", "a", "", strings.NewReader("1234")); err != nil {
		t.Fatal(err)
	}
	if err := r2.Receive("b", "b", "", strings.NewReader("123")); err == nil || !strings.Contains(err.Error(), "-max-request-files-size") {
		t.Fatalf("over -max-request-files-size: %v", err)
	}
	if err := r2.Receive("a", "c", "", strings.NewReader("1")); err == nil || !strings.Contains(err.Error(), "two parts named") {
		t.Fatalf("duplicate part name: %v", err)
	}
	if err := r2.Receive("request", "c", "", strings.NewReader("1")); err == nil {
		t.Fatal("a file part named request must be refused")
	}
}
