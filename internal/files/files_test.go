package files

import (
	"context"
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
	if entries, _ := os.ReadDir(filepath.Join(root, tmpDirName)); len(entries) != 0 {
		t.Fatalf("temporary files left: %v", entries)
	}
}

func TestMediaTypeInference(t *testing.T) {
	for _, tc := range []struct {
		declared, filename string
		head               []byte
		want               string
	}{
		{"audio/ogg; codecs=opus", "x.bin", nil, "audio/ogg"},
		{"application/octet-stream", "Report.PDF", nil, "application/pdf"},
		{"", "notes.md", nil, "text/markdown"},
		{"", "blob", []byte("%PDF-1.7"), "application/pdf"},
		{"", "blob", []byte{0, 1, 2, 3}, "application/octet-stream"},
		{"not a type", "a.png", nil, "image/png"},
	} {
		if got := InferMediaType(tc.declared, tc.filename, tc.head); got != tc.want {
			t.Errorf("InferMediaType(%q, %q) = %q, want %q", tc.declared, tc.filename, got, tc.want)
		}
	}
}

func TestAPartFileNameIsReducedToAValidBaseName(t *testing.T) {
	s := New(Config{Root: t.TempDir()})
	for _, tc := range []struct{ in, want string }{
		{`C:\Users\me\report.pdf`, "report.pdf"},
		{"a\tb\x01c.txt", "abc.txt"},
		{"bad\xffbyte.txt", "bad_byte.txt"},
		{strings.Repeat("é", 200), strings.Repeat("é", 127)},
		{"dir/", ""},
	} {
		r := s.NewRequest()
		if err := r.Receive("f", tc.in, "", strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
		b, err := r.Describe("f")
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if b.Name != tc.want {
			t.Errorf("%q became %q, want %q", tc.in, b.Name, tc.want)
		}
	}
}
