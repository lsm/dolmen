package s3store

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/files"
)

func fakeS3(t *testing.T, bucket string) (string, *credentials.Credentials) {
	t.Helper()
	backend := s3mem.New()
	if bucket != "" {
		if err := backend.CreateBucket(bucket); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	return srv.URL, credentials.NewStaticV4("key", "secret", "")
}

func newFakeS3Store(t *testing.T, prefix string) files.BlobStore {
	t.Helper()
	endpoint, creds := fakeS3(t, "blobs")
	b, err := New(context.Background(), Config{Bucket: "blobs", Endpoint: endpoint, Region: "us-east-1", Prefix: prefix, PathStyle: true, Creds: creds})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

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

func TestEveryBlobStoreKeepsTheSameContract(t *testing.T) {
	for name, newStore := range map[string]func(t *testing.T) files.BlobStore{
		"local": func(t *testing.T) files.BlobStore { return files.NewLocal(t.TempDir()) },
		"s3":    func(t *testing.T) files.BlobStore { return newFakeS3Store(t, "deploy/") },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			b := newStore(t)
			if _, err := b.Stat(ctx, "app", sumA); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("Stat of a missing blob = %v, want fs.ErrNotExist", err)
			}
			if _, err := b.Open(ctx, "app", sumA); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("Open of a missing blob = %v, want fs.ErrNotExist", err)
			}
			src := tempBlob(t, "hello world")
			if err := b.Put(ctx, "app", sumA, src); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(src); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("Put must consume its temporary file, stat = %v", err)
			}
			for ns, sum := range map[string]string{"app/child": sumB, "app2": sumB} {
				if err := b.Put(ctx, ns, sum, tempBlob(t, "other")); err != nil {
					t.Fatal(err)
				}
			}
			info, err := b.Stat(ctx, "app", sumA)
			if err != nil || info.Size != 11 || info.Modified.IsZero() {
				t.Fatalf("Stat = %+v, %v", info, err)
			}
			f, err := b.Open(ctx, "app", sumA)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Seek(6, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(f)
			f.Close()
			if string(got) != "world" || f.Size() != 11 {
				t.Fatalf("Open read %q size %d, want \"world\" from an 11-byte blob", got, f.Size())
			}
			var listed []string
			if err := b.List(ctx, "app", func(sum string, info files.BlobInfo) error {
				listed = append(listed, sum)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(listed) != 1 || listed[0] != sumA {
				t.Fatalf("List(app) = %v, want only its own blob, not app/child's or app2's", listed)
			}
			if err := b.DropNamespace(ctx, "app"); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Stat(ctx, "app", sumA); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a dropped namespace's blob is still there: %v", err)
			}
			for _, ns := range []string{"app/child", "app2"} {
				if _, err := b.Stat(ctx, ns, sumB); err != nil {
					t.Fatalf("dropping app removed %s's blob: %v", ns, err)
				}
			}
			if err := b.Delete(ctx, "app2", sumB); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Stat(ctx, "app2", sumB); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("Delete left the blob: %v", err)
			}
		})
	}
}

func TestAnS3StoreRefusesToStartWithoutItsBucket(t *testing.T) {
	endpoint, creds := fakeS3(t, "")
	_, err := New(context.Background(), Config{Bucket: "absent", Endpoint: endpoint, Region: "us-east-1", PathStyle: true, Creds: creds})
	if err == nil || !strings.Contains(err.Error(), `bucket "absent" does not exist`) {
		t.Fatalf("a missing bucket must refuse startup naming it, got %v", err)
	}
	for _, bad := range []string{"s3.amazonaws.com", "ftp://host", "https://host/path"} {
		if _, err := New(context.Background(), Config{Bucket: "b", Endpoint: bad}); err == nil || !strings.Contains(err.Error(), "-files-s3-endpoint") {
			t.Errorf("endpoint %q: %v", bad, err)
		}
	}
}

func TestAnUnreachableS3StoreIsStorageUnavailable(t *testing.T) {
	endpoint, creds := fakeS3(t, "blobs")
	down := &flakyTransport{}
	b, err := New(context.Background(), Config{Bucket: "blobs", Endpoint: endpoint, Region: "us-east-1", PathStyle: true, Creds: creds, Transport: down})
	if err != nil {
		t.Fatal(err)
	}
	down.fail = true
	_, err = b.Stat(context.Background(), "app", sumA)
	var fe *files.Error
	if !errors.As(err, &fe) || fe.Code != derr.StorageUnavailable || fe.Status != http.StatusServiceUnavailable {
		t.Fatalf("an unreachable store must answer 503 storage_unavailable, got %v", err)
	}
}

type flakyTransport struct {
	fail bool
}

func (f *flakyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.fail {
		return nil, errors.New("connection refused")
	}
	return http.DefaultTransport.RoundTrip(r)
}
