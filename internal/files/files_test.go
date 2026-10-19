package files

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/store"
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
	f, err := s.Open(context.Background(), "team/app", b.SHA256)
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
	if !errors.As(err, &fe) || fe.Status != 507 || !strings.Contains(fe.Message, "-max-namespace-files-size") || !strings.Contains(fe.Message, "admin on the namespace") {
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
		if _, err := s.Open(context.Background(), "app", sum); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Open(%q) = %v, want not-exist", sum, err)
		}
	}
}

func TestAFailedWriteKeepsAFileAnotherRequestAttached(t *testing.T) {
	s := New(Config{Root: t.TempDir()})
	a, b := s.NewRequest(), s.NewRequest()
	for _, r := range []*Request{a, b} {
		if err := r.Receive("f", "x", "", strings.NewReader("shared")); err != nil {
			t.Fatal(err)
		}
	}
	blob, err := a.Attach(context.Background(), "app", "f")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Attach(context.Background(), "app", "f"); err != nil {
		t.Fatal(err)
	}
	b.Close()
	a.Discard()
	a.Close()
	f, err := s.Open(context.Background(), "app", blob.SHA256)
	if err != nil {
		t.Fatalf("another request attached this file and may have committed a reference to it: %v", err)
	}
	f.Close()
}

func TestAFailedWriteRemovesAFileOnlyItAttached(t *testing.T) {
	s := New(Config{Root: t.TempDir()})
	r := s.NewRequest()
	if err := r.Receive("f", "x", "", strings.NewReader("alone")); err != nil {
		t.Fatal(err)
	}
	blob, err := r.Attach(context.Background(), "app", "f")
	if err != nil {
		t.Fatal(err)
	}
	r.Discard()
	r.Close()
	if _, err := s.Open(context.Background(), "app", blob.SHA256); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a file only the failed write attached must go: %v", err)
	}
}

func TestASweepRemovesOnlyUnreferencedUnpinnedFiles(t *testing.T) {
	root := t.TempDir()
	s := New(Config{Root: root, MaxNamespaceFiles: 30})
	place := func(data string) (store.FileBlob, *Request) {
		r := s.NewRequest()
		if err := r.Receive("f", "x", "", strings.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		b, err := r.Attach(context.Background(), "app", "f")
		if err != nil {
			t.Fatal(err)
		}
		return b, r
	}
	kept, r1 := place("referenced")
	r1.Close()
	gone, r2 := place("orphan-1")
	r2.Close()
	pinned, inFlight := place("in-flight")
	streaming := s.NewRequest()
	defer streaming.Close()
	if err := streaming.Receive("g", "y", "", strings.NewReader("still streaming")); err != nil {
		t.Fatal(err)
	}
	orphanTmp := filepath.Join(root, tmpDirName, "part-crashed")
	if err := os.WriteFile(orphanTmp, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := s.Sweep(context.Background(), "app", map[string]bool{kept.SHA256: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 || res.RemovedBytes != gone.Size || res.Bytes != kept.Size+pinned.Size {
		t.Fatalf("sweep = %+v", res)
	}
	for _, b := range []store.FileBlob{kept, pinned} {
		if f, err := s.Open(context.Background(), "app", b.SHA256); err != nil {
			t.Fatalf("%s must stay: %v", b.SHA256, err)
		} else {
			f.Close()
		}
	}
	if _, err := s.Open(context.Background(), "app", gone.SHA256); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the unreferenced file must go: %v", err)
	}
	if _, err := os.Stat(orphanTmp); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a temporary file no request holds must go")
	}
	if entries, _ := os.ReadDir(filepath.Join(root, tmpDirName)); len(entries) != 1 {
		t.Fatalf("the streaming request's temporary file must stay, got %v", entries)
	}
	inFlight.Close()
	if _, r := place("0123456789"); r != nil {
		r.Close()
	}
}

func TestASweepNeverRemovesAPartBeingReceived(t *testing.T) {
	s := New(Config{Root: t.TempDir()})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				s.Sweep(context.Background(), "app", map[string]bool{})
			}
		}
	}()
	defer func() { close(stop); <-done }()
	for i := 0; i < 300; i++ {
		r := s.NewRequest()
		if err := r.Receive("f", "x", "", strings.NewReader(fmt.Sprintf("part %d", i))); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Attach(context.Background(), "ns"+fmt.Sprint(i), "f"); err != nil {
			r.Close()
			t.Fatalf("attempt %d: a sweep removed a part still being received: %v", i, err)
		}
		r.Close()
	}
}

func TestAnAbortedSweepStillReleasesWhatItRemoved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block reads on Windows")
	}
	root := t.TempDir()
	s := New(Config{Root: root, MaxNamespaceFiles: 20})
	r := s.NewRequest()
	if err := r.Receive("f", "x", "", strings.NewReader("0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	b, err := r.Attach(context.Background(), "app", "f")
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if b.SHA256[:2] == "ff" {
		t.Skip("the fixture's hash sorts after the unreadable directory")
	}
	locked := filepath.Join(root, "app.files", "ff")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	if _, err := s.Sweep(context.Background(), "app", map[string]bool{}); err == nil {
		t.Skip("the walk did not fail; running with permissions that ignore mode bits")
	}
	r2 := s.NewRequest()
	defer r2.Close()
	if err := r2.Receive("g", "y", "", strings.NewReader("ABCDEFGHIJKLMNOP")); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Attach(context.Background(), "app", "g"); err != nil {
		t.Fatalf("the removed file must stop counting toward the cap even when the sweep fails later: %v", err)
	}
}

func TestTheStoreKeepsBlobsInItsConfiguredBlobStore(t *testing.T) {
	root, blobs := t.TempDir(), t.TempDir()
	s := New(Config{Root: root, Blobs: NewLocal(blobs)})
	r := s.NewRequest()
	if err := r.Receive("f", "a.txt", "", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	b, err := r.Attach(context.Background(), "app", "f")
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if _, err := os.Stat(filepath.Join(blobs, "app.files", b.SHA256[:2], b.SHA256)); err != nil {
		t.Fatalf("blob not in the configured blob store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "app.files")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("blob also written under the data directory: %v", err)
	}
	f, err := s.Open(context.Background(), "app", b.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if f.Size() != 5 {
		t.Fatalf("size = %d, want 5", f.Size())
	}
	f.Close()
	res, err := s.Sweep(context.Background(), "app", map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 {
		t.Fatalf("sweep removed %d blobs from the configured store, want 1", res.Removed)
	}
}

func TestTheSigningKeyIsNeverLeftHalfWritten(t *testing.T) {
	root := t.TempDir()
	s := New(Config{Root: root})
	sig, err := s.Signer().Sign("app", "docs", 1, "doc", strings.Repeat("a", 64), 1000)
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(root, "files.key"))
	if err != nil || len(key) != 32 {
		t.Fatalf("files.key = %d bytes, %v; want a complete 32-byte key", len(key), err)
	}
	if info, _ := os.Stat(filepath.Join(root, "files.key")); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("files.key mode = %v, want 0600", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".files-key-") {
			t.Fatalf("a temporary key file was left behind: %s", e.Name())
		}
	}
	again := New(Config{Root: root})
	if ok, err := again.Signer().Valid("app", "docs", 1, "doc", strings.Repeat("a", 64), 1000, sig); err != nil || !ok {
		t.Fatalf("a restarted server must verify links signed before: %v %v", ok, err)
	}
}
