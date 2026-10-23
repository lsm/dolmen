package conformance

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/files"
	"github.com/lsm/dolmen/internal/files/s3store"
)

func newHarnessBlobs(t *testing.T, blobs files.BlobStore, to *api.Timeouts) *harness {
	t.Helper()
	h := newHarnessAtMode(t, t.TempDir(), &fakeProvider{}, authOff)
	h.blobs = blobs
	if to != nil {
		h.timeouts = to
		h.apiOpts = []api.Option{api.WithTimeouts(*to)}
	}
	h.reopen()
	return h
}

func fakeBucket(t *testing.T) files.BlobStore {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket("blobs"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	b, err := s3store.New(context.Background(), s3store.Config{Bucket: "blobs", Endpoint: srv.URL, Region: "us-east-1", PathStyle: true, Creds: credentials.NewStaticV4("key", "secret", "")})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFileURLOverAnS3StoreUsesThePublicEndpoint(t *testing.T) {
	backend := s3mem.New()
	if err := backend.CreateBucket("blobs"); err != nil {
		t.Fatal(err)
	}
	internal := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(internal.Close)
	target, _ := url.Parse(internal.URL)
	public := httptest.NewServer(httputil.NewSingleHostReverseProxy(target))
	t.Cleanup(public.Close)
	b, err := s3store.New(context.Background(), s3store.Config{Bucket: "blobs", Endpoint: internal.URL, PublicEndpoint: public.URL, Region: "us-east-1", PathStyle: true, Creds: credentials.NewStaticV4("key", "secret", "")})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessBlobs(t, b, nil)
	h.seedTable("fil", "docs", fileTableFields())
	data := []byte("served from the public endpoint")
	if code, body := h.multipartCall("insert", `{"namespace":"fil","table":"docs","records":[{"title":"a","doc":{"part":"notes"}}]}`, map[string][]byte{"notes": data}); code != http.StatusOK {
		t.Fatalf("multipart insert over S3: %d %v", code, body)
	}
	link, _ := h.mustMCP("file_url", map[string]any{"namespace": "fil", "table": "docs", "id": 1, "field": "doc"})["url"].(string)
	if !strings.HasPrefix(link, public.URL+"/") {
		t.Fatalf("file_url must be presigned for -files-s3-public-endpoint %s, got %q", public.URL, link)
	}
	res, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || string(got) != string(data) {
		t.Fatalf("GET through the public endpoint: %d %q", res.StatusCode, got)
	}
}

func TestFileFieldsWorkOverAnS3Store(t *testing.T) {
	h := newHarnessBlobs(t, fakeBucket(t), nil)
	h.seedTable("fil", "docs", fileTableFields())
	data := []byte("kept in a bucket")
	if code, body := h.multipartCall("insert", `{"namespace":"fil","table":"docs","records":[{"title":"a","doc":{"part":"notes"}},{"title":"b"}]}`, map[string][]byte{"notes": data}); code != http.StatusOK {
		t.Fatalf("multipart insert over S3: %d %v", code, body)
	}
	httpRef := h.mustHTTP("read_rows", map[string]any{"namespace": "fil", "table": "docs", "ids": []int{1}})["rows"].([]any)[0].(map[string]any)["doc"]
	mcpRef := h.mustMCP("read_rows", map[string]any{"namespace": "fil", "table": "docs", "ids": []int{1}})["rows"].([]any)[0].(map[string]any)["doc"]
	assertJSONEqual(t, "file reference over S3", mcpRef, httpRef)
	res, err := http.Get(h.srv.URL + "/v1/files?namespace=fil&table=docs&id=1&field=doc")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || string(got) != string(data) {
		t.Fatalf("download from S3: %d %q", res.StatusCode, got)
	}
	h.mustHTTP("update", map[string]any{"namespace": "fil", "table": "docs", "filter": "id = 1", "set": map[string]any{"doc": nil}})
	if res := h.mustHTTP("vacuum", map[string]any{"namespace": "fil"}); res["files_removed"] != float64(1) || res["file_bytes"] != float64(0) {
		t.Fatalf("vacuum over S3 must remove the unreferenced blob, got %v", res)
	}
}

type switchableBlobs struct {
	files.BlobStore
	down  atomic.Bool
	delay time.Duration
}

func (s *switchableBlobs) Shared() bool { return true }

func (s *switchableBlobs) outage() error {
	return &files.Error{Status: http.StatusServiceUnavailable, Code: derr.StorageUnavailable, Message: "the file store did not answer (network error), so nothing was written or read; retry shortly"}
}

func (s *switchableBlobs) Stat(ctx context.Context, ns, sum string) (files.BlobInfo, error) {
	if s.down.Load() {
		return files.BlobInfo{}, s.outage()
	}
	return s.BlobStore.Stat(ctx, ns, sum)
}

func (s *switchableBlobs) Put(ctx context.Context, ns, sum, path string) error {
	if s.down.Load() {
		return s.outage()
	}
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.BlobStore.Put(ctx, ns, sum, path)
}

func (s *switchableBlobs) Open(ctx context.Context, ns, sum string) (files.Blob, error) {
	if s.down.Load() {
		return nil, s.outage()
	}
	return s.BlobStore.Open(ctx, ns, sum)
}

func TestAnUnreachableFileStoreIsStorageUnavailableOnEveryTransport(t *testing.T) {
	blobs := &switchableBlobs{BlobStore: files.NewLocal(t.TempDir())}
	h := newHarnessBlobs(t, blobs, nil)
	h.seedTable("fil", "docs", fileTableFields())
	if code, body := h.multipartCall("insert", `{"namespace":"fil","table":"docs","records":[{"title":"a","doc":{"part":"notes"}}]}`, map[string][]byte{"notes": []byte("notes")}); code != http.StatusOK {
		t.Fatalf("multipart insert: %d %v", code, body)
	}
	blobs.down.Store(true)
	code, body := h.multipartCall("insert", `{"namespace":"fil","table":"docs","records":[{"title":"b","doc":{"part":"other"}}]}`, map[string][]byte{"other": []byte("other bytes")})
	if code != http.StatusServiceUnavailable || errCode(body) != "storage_unavailable" {
		t.Fatalf("a multipart write while the store is down: %d %v", code, body)
	}
	copyWrite := map[string]any{"namespace": "fil", "table": "docs", "records": []map[string]any{{"title": "c", "doc": map[string]any{"copy_from": map[string]any{"table": "docs", "id": 1, "field": "doc"}}}}}
	if code, body := h.httpCall("insert", copyWrite); code != http.StatusServiceUnavailable || errCode(body) != "storage_unavailable" {
		t.Fatalf("a copy_from write over HTTP while the store is down: %d %v", code, body)
	}
	if env := h.mcpCall("insert", copyWrite).toolError(); env == nil || env["code"] != "storage_unavailable" {
		t.Fatalf("a copy_from write over MCP while the store is down must carry the same envelope, got %v", env)
	}
	res, err := http.Get(h.srv.URL + "/v1/files?namespace=fil&table=docs&id=1&field=doc")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a download while the store is down: %d, want 503", res.StatusCode)
	}
}

func TestARemoteUploadGetsTheFetchTimeoutOnTopOfTheOperationLimit(t *testing.T) {
	blobs := &switchableBlobs{BlobStore: files.NewLocal(t.TempDir()), delay: 600 * time.Millisecond}
	h := newHarnessBlobs(t, blobs, nil)
	h.seedTable("fil", "docs", fileTableFields())
	h.retime(api.Timeouts{Op: 200 * time.Millisecond})
	code, body := h.multipartCall("insert", `{"namespace":"fil","table":"docs","records":[{"title":"a","doc":{"part":"notes"}}]}`, map[string][]byte{"notes": []byte("slow upload")})
	if code != http.StatusOK {
		t.Fatalf("an upload slower than -op-timeout to a remote store must still finish within -files-fetch-timeout: %d %v", code, body)
	}
}

func TestFileURLOverAnS3StoreIsThePresignedObjectURL(t *testing.T) {
	h := newHarnessBlobs(t, fakeBucket(t), nil)
	h.seedTable("fil", "docs", fileTableFields())
	data := []byte("presigned bytes")
	if code, body := h.multipartCall("insert", `{"namespace":"fil","table":"docs","records":[{"title":"a","doc":{"part":"notes"}}]}`, map[string][]byte{"notes": data}); code != http.StatusOK {
		t.Fatalf("multipart insert over S3: %d %v", code, body)
	}
	input := map[string]any{"namespace": "fil", "table": "docs", "id": 1, "field": "doc", "ttl_seconds": 120}
	for transport, out := range map[string]map[string]any{"http": h.mustHTTP("file_url", input), "mcp": h.mustMCP("file_url", input)} {
		link, _ := out["url"].(string)
		u, err := url.Parse(link)
		if err != nil || strings.HasPrefix(link, h.srv.URL) || u.Query().Get("X-Amz-Signature") == "" || u.Query().Get("X-Amz-Expires") != "120" {
			t.Fatalf("%s: file_url over S3 must be the bucket's own presigned GET for ttl_seconds, got %q", transport, link)
		}
		if got := u.Query().Get("response-content-disposition"); !strings.HasPrefix(got, "attachment;") || u.Query().Get("response-content-type") != "text/plain" {
			t.Fatalf("%s: the presigned URL must make the bucket answer as the download endpoint would: disposition %q type %q", transport, got, u.Query().Get("response-content-type"))
		}
		res, err := http.Get(link)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || string(got) != string(data) {
			t.Fatalf("%s: GET of the presigned URL: %d %q", transport, res.StatusCode, got)
		}
	}
}
