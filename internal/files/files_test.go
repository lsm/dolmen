package files

import (
	"context"
	"errors"
	"io/fs"
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

func TestPlacedBytesStopCountingAsPending(t *testing.T) {
	s := New(Config{Root: t.TempDir(), MaxPendingFiles: 10})
	r := s.NewRequest()
	defer r.Close()
	if err := r.Receive("f", "a.bin", "", strings.NewReader("12345678")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Attach(context.Background(), "app", "f"); err != nil {
		t.Fatal(err)
	}
	other := s.NewRequest()
	defer other.Close()
	if err := other.Receive("g", "b.bin", "", strings.NewReader("12345678")); err != nil {
		t.Fatalf("a placed file still counted as pending: %v", err)
	}
}

func TestAnAttachedHashStaysPinnedUntilItsRequestEnds(t *testing.T) {
	root := t.TempDir()
	s := New(Config{Root: root})
	r := s.NewRequest()
	if err := r.Receive("f", "a.txt", "", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	b, err := r.Attach(context.Background(), "team/app", "f")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "team", "app.files", b.SHA256[:2], b.SHA256)); err != nil {
		t.Fatalf("blob not stored under the namespace's directory: %v", err)
	}
	if !s.Pinned("team/app", b.SHA256) {
		t.Fatal("an attached hash must be pinned while its request runs")
	}
	r.Close()
	if s.Pinned("team/app", b.SHA256) {
		t.Fatal("closing the request must unpin its hashes")
	}
	f, err := s.Open("team/app", b.SHA256)
	if err != nil {
		t.Fatalf("a placed file must stay after its request ends: %v", err)
	}
	f.Close()
}

func TestIdenticalBytesArePlacedOnceAndTheCapCountsThemOnce(t *testing.T) {
	root := t.TempDir()
	s := New(Config{Root: root, MaxNamespaceFiles: 10})
	place := func(data string) error {
		r := s.NewRequest()
		defer r.Close()
		if err := r.Receive("f", "x", "", strings.NewReader(data)); err != nil {
			return err
		}
		_, err := r.Attach(context.Background(), "app", "f")
		return err
	}
	if err := place("12345678"); err != nil {
		t.Fatal(err)
	}
	if err := place("12345678"); err != nil {
		t.Fatalf("bytes already stored add nothing and must be accepted at the cap: %v", err)
	}
	err := place("abcdefgh")
	var fe *Error
	if !errors.As(err, &fe) || fe.Status != 507 || !strings.Contains(fe.Message, "-max-namespace-files-size") {
		t.Fatalf("over the cap: %v", err)
	}
	var n int
	filepath.WalkDir(filepath.Join(root, "app.files"), func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			n++
		}
		return nil
	})
	if n != 1 {
		t.Fatalf("stored %d files, want 1", n)
	}
}

func TestOpenRefusesWhatIsNotAHash(t *testing.T) {
	s := New(Config{Root: t.TempDir()})
	for _, sum := range []string{"", "../x", strings.Repeat("A", 64), strings.Repeat("a", 63)} {
		if _, err := s.Open("app", sum); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Open(%q) = %v, want not-exist", sum, err)
		}
	}
}
