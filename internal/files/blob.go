package files

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

type Blob interface {
	io.ReadSeekCloser
	Size() int64
}

type BlobInfo struct {
	Size     int64
	Modified time.Time
}

type BlobStore interface {
	Stat(ctx context.Context, ns, sum string) (BlobInfo, error)
	Put(ctx context.Context, ns, sum, path string) error
	Open(ctx context.Context, ns, sum string) (Blob, error)
	Delete(ctx context.Context, ns, sum string) error
	List(ctx context.Context, ns string, fn func(sum string, info BlobInfo) error) error
	DropNamespace(ctx context.Context, ns string) error
}

type localBlobs struct {
	root string
}

func NewLocal(root string) BlobStore { return &localBlobs{root: root} }

func (l *localBlobs) nsDir(ns string) string {
	return filepath.Join(l.root, filepath.FromSlash(ns)+nsDirSuffix)
}

func (l *localBlobs) path(ns, sum string) string {
	return filepath.Join(l.nsDir(ns), sum[:2], sum)
}

func (l *localBlobs) Stat(_ context.Context, ns, sum string) (BlobInfo, error) {
	info, err := os.Stat(l.path(ns, sum))
	if err != nil {
		return BlobInfo{}, err
	}
	return BlobInfo{Size: info.Size(), Modified: info.ModTime()}, nil
}

func (l *localBlobs) Put(_ context.Context, ns, sum, path string) error {
	final := l.path(ns, sum)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return err
	}
	if err := os.Rename(path, final); err != nil {
		return err
	}
	return syncDir(filepath.Dir(final))
}

type localBlob struct {
	*os.File
	size int64
}

func (b localBlob) Size() int64 { return b.size }

func (l *localBlobs) Open(_ context.Context, ns, sum string) (Blob, error) {
	f, err := os.Open(l.path(ns, sum))
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return localBlob{File: f, size: info.Size()}, nil
}

func (l *localBlobs) Delete(_ context.Context, ns, sum string) error {
	path := l.path(ns, sum)
	if err := os.Remove(path); err != nil {
		return err
	}
	os.Remove(filepath.Dir(path))
	return nil
}

func (l *localBlobs) List(_ context.Context, ns string, fn func(sum string, info BlobInfo) error) error {
	return filepath.WalkDir(l.nsDir(ns), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() || !ValidHash(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return fn(d.Name(), BlobInfo{Size: info.Size(), Modified: info.ModTime()})
	})
}

func (l *localBlobs) DropNamespace(_ context.Context, ns string) error {
	return os.RemoveAll(l.nsDir(ns))
}
