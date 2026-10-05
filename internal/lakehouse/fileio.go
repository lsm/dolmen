package lakehouse

import (
	"context"
	"io/fs"
	"net/url"
	"runtime"

	icebergio "github.com/apache/iceberg-go/io"
)

type localFS struct{ icebergio.LocalFS }

func (f localFS) Open(name string) (icebergio.File, error) { return f.LocalFS.Open(localPath(name)) }

func (f localFS) Create(name string) (icebergio.FileWriter, error) {
	return f.LocalFS.Create(localPath(name))
}

func (f localFS) WriteFile(name string, content []byte) error {
	return f.LocalFS.WriteFile(localPath(name), content)
}

func (f localFS) Remove(name string) error { return f.LocalFS.Remove(localPath(name)) }

func (f localFS) WalkDir(root string, fn fs.WalkDirFunc) error {
	return f.LocalFS.WalkDir(localPath(root), fn)
}

func init() {
	factory := func(context.Context, *url.URL, map[string]string) (icebergio.IO, error) { return localFS{}, nil }
	schemes := []string{"file", ""}
	if runtime.GOOS == "windows" {
		for drive := 'a'; drive <= 'z'; drive++ {
			schemes = append(schemes, string(drive))
		}
	}
	for _, scheme := range schemes {
		icebergio.Unregister(scheme)
		icebergio.Register(scheme, factory)
	}
}
