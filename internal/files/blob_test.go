package files

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
)

func tempBlob(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "part")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const sumA = "aa00000000000000000000000000000000000000000000000000000000000000"
const sumB = "bb00000000000000000000000000000000000000000000000000000000000000"

type gatedPuts struct {
	BlobStore
	entered chan struct{}
	release chan struct{}
	gate    bool
}

func (g *gatedPuts) Put(ctx context.Context, ns, sum, path string) error {
	if g.gate {
		g.gate = false
		close(g.entered)
		<-g.release
	}
	return g.BlobStore.Put(ctx, ns, sum, path)
}

func TestAnUploadDoesNotHoldTheStoreLock(t *testing.T) {
	ctx := context.Background()
	blobs := &gatedPuts{BlobStore: NewLocal(t.TempDir()), entered: make(chan struct{}), release: make(chan struct{}), gate: true}
	s := New(Config{Root: t.TempDir(), Blobs: blobs})
	slow, quick := s.NewRequest(), s.NewRequest()
	defer slow.Close()
	defer quick.Close()
	if err := slow.Receive("f", "a.txt", "", strings.NewReader("slow upload")); err != nil {
		t.Fatal(err)
	}
	if err := quick.Receive("f", "b.txt", "", strings.NewReader("other bytes")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := slow.Attach(ctx, "app", "f")
		done <- err
	}()
	<-blobs.entered
	if _, err := quick.Attach(ctx, "app", "f"); err != nil {
		t.Fatalf("a second attach while an upload runs: %v", err)
	}
	close(blobs.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type gatedDrop struct {
	BlobStore
	entered chan struct{}
	release chan struct{}
}

func (g *gatedDrop) DropNamespace(ctx context.Context, ns string) error {
	close(g.entered)
	<-g.release
	return g.BlobStore.DropNamespace(ctx, ns)
}

func TestNoFileAttachesToANamespaceWhileItsFilesAreDeleted(t *testing.T) {
	ctx := context.Background()
	blobs := &gatedDrop{BlobStore: NewLocal(t.TempDir()), entered: make(chan struct{}), release: make(chan struct{})}
	s := New(Config{Root: t.TempDir(), Blobs: blobs})
	done := make(chan error, 1)
	go func() { done <- s.DropNamespace(ctx, "app") }()
	<-blobs.entered
	r := s.NewRequest()
	defer r.Close()
	if err := r.Receive("f", "a.txt", "", strings.NewReader("recreated")); err != nil {
		t.Fatal(err)
	}
	_, err := r.Attach(ctx, "app", "f")
	var fe *Error
	if !errors.As(err, &fe) || fe.Code != derr.StorageUnavailable || !strings.Contains(fe.Message, "being dropped") {
		t.Fatalf("an attach while the namespace's files are deleted must be refused as retryable, got %v", err)
	}
	if err := r.Pin("app", sumA); !errors.As(err, &fe) || fe.Code != derr.StorageUnavailable {
		t.Fatalf("a copy_from pin while the namespace's files are deleted must be refused as retryable, got %v", err)
	}
	close(blobs.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := r.Attach(ctx, "app", "f"); err != nil {
		t.Fatalf("once the drop finishes, attaching works again: %v", err)
	}
}

type failingWipe struct {
	BlobStore
}

func (failingWipe) DropNamespace(context.Context, string) error {
	return &Error{Status: http.StatusServiceUnavailable, Code: derr.StorageUnavailable, Message: "the file store did not answer"}
}

func (failingWipe) Location(ns string) string { return "s3://blobs/" + ns + ".files/" }

func TestAFailedWipeSaysWhereTheLeftoverFilesAre(t *testing.T) {
	s := New(Config{Root: t.TempDir(), Blobs: failingWipe{NewLocal(t.TempDir())}})
	err := s.DropNamespace(context.Background(), "app")
	var fe *Error
	if !errors.As(err, &fe) || fe.Code != derr.StorageUnavailable || fe.Status != http.StatusServiceUnavailable {
		t.Fatalf("a failed wipe must stay a retryable storage error, got %v", err)
	}
	if !strings.Contains(fe.Message, "was dropped") || !strings.Contains(fe.Message, "s3://blobs/app.files/") {
		t.Fatalf("the error must say the namespace is gone and where its files are left: %q", fe.Message)
	}
}

func TestASecondUploadOfTheSameBytesWaitsAndNeverOwnsTheBlob(t *testing.T) {
	ctx := context.Background()
	blobs := &gatedPuts{BlobStore: NewLocal(t.TempDir()), entered: make(chan struct{}), release: make(chan struct{}), gate: true}
	s := New(Config{Root: t.TempDir(), Blobs: blobs})
	first, second := s.NewRequest(), s.NewRequest()
	defer second.Close()
	for _, r := range []*Request{first, second} {
		if err := r.Receive("f", "a.txt", "", strings.NewReader("same bytes")); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() {
		_, err := first.Attach(ctx, "app", "f")
		done <- err
	}()
	<-blobs.entered
	secondDone := make(chan error, 1)
	go func() {
		_, err := second.Attach(ctx, "app", "f")
		secondDone <- err
	}()
	close(blobs.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	b, _ := first.Describe("f")
	first.Close()
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	second.Discard()
	if _, err := s.Open(ctx, "app", b.SHA256); err != nil {
		t.Fatalf("a failed write that found the blob already placed deleted another request's committed blob: %v", err)
	}
}
