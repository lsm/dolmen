package files

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/lsm/dolmen/internal/derr"
)

const MaxS3FileSize int64 = 5 << 40

type S3Config struct {
	Bucket    string
	Endpoint  string
	Region    string
	Prefix    string
	PathStyle bool
	Transport http.RoundTripper
	Creds     *credentials.Credentials
}

type s3Blobs struct {
	client *minio.Client
	bucket string
	prefix string
}

func NewS3(ctx context.Context, cfg S3Config) (BlobStore, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("-files-s3-bucket is required with -files-store s3")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://s3.amazonaws.com"
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("-files-s3-endpoint must be an http:// or https:// URL with no path, got %q", cfg.Endpoint)
	}
	creds := cfg.Creds
	if creds == nil {
		creds = credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{},
			&credentials.FileAWSCredentials{},
			&credentials.IAM{},
		})
	}
	lookup := minio.BucketLookupDNS
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:        creds,
		Secure:       u.Scheme == "https",
		Region:       cfg.Region,
		BucketLookup: lookup,
		Transport:    cfg.Transport,
	})
	if err != nil {
		return nil, fmt.Errorf("-files-s3-endpoint: %w", err)
	}
	ok, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("cannot reach S3 bucket %q at %s: %s", cfg.Bucket, cfg.Endpoint, s3ErrorClass(err))
	}
	if !ok {
		return nil, fmt.Errorf("S3 bucket %q does not exist at %s; create it, or set -files-s3-bucket to one that does", cfg.Bucket, cfg.Endpoint)
	}
	return &s3Blobs{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

func s3ErrorClass(err error) string {
	resp := minio.ToErrorResponse(err)
	if resp.Code != "" {
		return resp.Code
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return "network error"
	}
	return "error"
}

func (b *s3Blobs) nsPrefix(ns string) string { return b.prefix + ns + nsDirSuffix + "/" }

func (b *s3Blobs) key(ns, sum string) string { return b.nsPrefix(ns) + sum[:2] + "/" + sum }

func unavailable(err error) error {
	if err == nil {
		return nil
	}
	resp := minio.ToErrorResponse(err)
	if resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound {
		return fs.ErrNotExist
	}
	return &Error{Status: http.StatusServiceUnavailable, Code: derr.StorageUnavailable, Message: "the file store did not answer (" + s3ErrorClass(err) + "), so nothing was written or read; retry shortly"}
}

func (b *s3Blobs) Stat(ctx context.Context, ns, sum string) (BlobInfo, error) {
	info, err := b.client.StatObject(ctx, b.bucket, b.key(ns, sum), minio.StatObjectOptions{})
	if err != nil {
		return BlobInfo{}, unavailable(err)
	}
	return BlobInfo{Size: info.Size, Modified: info.LastModified}, nil
}

func (b *s3Blobs) Put(ctx context.Context, ns, sum, path string) error {
	if _, err := b.client.FPutObject(ctx, b.bucket, b.key(ns, sum), path, minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		return unavailable(err)
	}
	return os.Remove(path)
}

type s3Blob struct {
	*minio.Object
	size int64
}

func (o s3Blob) Size() int64 { return o.size }

func (b *s3Blobs) Open(ctx context.Context, ns, sum string) (Blob, error) {
	obj, err := b.client.GetObject(ctx, b.bucket, b.key(ns, sum), minio.GetObjectOptions{})
	if err != nil {
		return nil, unavailable(err)
	}
	info, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, unavailable(err)
	}
	return s3Blob{Object: obj, size: info.Size}, nil
}

func (b *s3Blobs) Delete(ctx context.Context, ns, sum string) error {
	return unavailable(b.client.RemoveObject(ctx, b.bucket, b.key(ns, sum), minio.RemoveObjectOptions{}))
}

func (b *s3Blobs) List(ctx context.Context, ns string, fn func(sum string, info BlobInfo) error) error {
	prefix := b.nsPrefix(ns)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for obj := range b.client.ListObjects(ctx, b.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return unavailable(obj.Err)
		}
		rest := strings.TrimPrefix(obj.Key, prefix)
		dir, sum, ok := strings.Cut(rest, "/")
		if !ok || !ValidHash(sum) || dir != sum[:2] {
			continue
		}
		if err := fn(sum, BlobInfo{Size: obj.Size, Modified: obj.LastModified}); err != nil {
			return err
		}
	}
	return nil
}

const dropTimeout = 10 * time.Minute

func (b *s3Blobs) DropNamespace(ctx context.Context, ns string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dropTimeout)
	defer cancel()
	var failed error
	for attempt := 0; attempt < 3; attempt++ {
		objects := make(chan minio.ObjectInfo)
		go func() {
			defer close(objects)
			for obj := range b.client.ListObjects(ctx, b.bucket, minio.ListObjectsOptions{Prefix: b.nsPrefix(ns), Recursive: true}) {
				if obj.Err != nil {
					return
				}
				objects <- obj
			}
		}()
		failed = nil
		for res := range b.client.RemoveObjects(ctx, b.bucket, objects, minio.RemoveObjectsOptions{}) {
			if res.Err != nil && failed == nil {
				failed = unavailable(res.Err)
			}
		}
		if failed == nil {
			left := false
			for obj := range b.client.ListObjects(ctx, b.bucket, minio.ListObjectsOptions{Prefix: b.nsPrefix(ns), Recursive: true, MaxKeys: 1}) {
				if obj.Err != nil {
					failed = unavailable(obj.Err)
				} else {
					left = true
				}
				break
			}
			if failed == nil && !left {
				return nil
			}
		}
	}
	if failed == nil {
		failed = errors.New("objects remained under the namespace's prefix after three passes")
	}
	return failed
}

const storeMarkerFile = "files.store"

func MarkS3(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, storeMarkerFile), []byte("s3\n"), 0o644)
}

func UsesS3(dataDir string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, storeMarkerFile))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(b)) == "s3", nil
}
