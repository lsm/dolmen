package dolmen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/files"
	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type FileUpload struct {
	Reader    io.Reader
	Name      string
	MediaType string
}

type FileInfo struct {
	SHA256    string
	Size      int64
	MediaType string
	Name      string
}

type fileParts struct {
	req *files.Request
	n   int
}

func (s *Store) withFiles(ctx context.Context, maps []map[string]any) (context.Context, []map[string]any, *fileParts, error) {
	var fp *fileParts
	out := maps
	copied := map[int]bool{}
	for i, m := range maps {
		for k, v := range m {
			f, ok := fileValue(v)
			if !ok {
				continue
			}
			if s.files == nil {
				fp.finish(nil)
				return ctx, nil, nil, derr.New(derr.InvalidRequest, "field %q: this store cannot hold files; file fields need the SQLite engine", k)
			}
			if f.Reader == nil {
				fp.finish(nil)
				return ctx, nil, nil, derr.New(derr.InvalidRequest, "field %q: dolmen.FileUpload needs a Reader", k)
			}
			if fp == nil {
				fp = &fileParts{req: s.files.NewRequest()}
				out = append([]map[string]any(nil), maps...)
			}
			if !copied[i] {
				cp := make(map[string]any, len(m))
				for kk, vv := range m {
					cp[kk] = vv
				}
				out[i], copied[i] = cp, true
			}
			fp.n++
			part := fmt.Sprintf("file%d", fp.n)
			if err := fp.req.Receive(part, f.Name, f.MediaType, f.Reader); err != nil {
				fp.finish(nil)
				return ctx, nil, nil, err
			}
			out[i][k] = map[string]any{"part": part}
		}
	}
	if fp == nil {
		return ctx, maps, nil, nil
	}
	return store.WithFileSource(ctx, fp.req), out, fp, nil
}

func fileValue(v any) (FileUpload, bool) {
	switch x := v.(type) {
	case FileUpload:
		return x, true
	case *FileUpload:
		if x != nil {
			return *x, true
		}
	}
	return FileUpload{}, false
}

func (fp *fileParts) finish(err error) {
	if fp == nil {
		return
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		fp.req.Discard()
	}
	fp.req.Close()
}

func (s *Store) OpenFile(ctx context.Context, namespace, table string, id int64, field string) (_ io.ReadSeekCloser, _ FileInfo, err error) {
	ctx, span := s.startOp(ctx, "open_file", namespace, table)
	defer func() { endOp(span, err) }()
	if err := s.begin(); err != nil {
		return nil, FileInfo{}, err
	}
	defer s.done()
	if s.files == nil {
		return nil, FileInfo{}, derr.New(derr.NotFound, "this store holds no files")
	}
	ns, tbl := ops.NormalizeNamespace(namespace), ops.NormalizeTable(table)
	sc, _, err := s.eng.TableState(ctx, ns, tbl, nil)
	if err != nil {
		return nil, FileInfo{}, facadeErr(ctx, err)
	}
	if f := sc.Field(field); f == nil || f.Type != schema.File {
		return nil, FileInfo{}, derr.New(derr.InvalidRequest, "field %q of %s is not a file field", field, table)
	}
	res, err := s.eng.GetRows(ctx, ns, tbl, []int64{id}, nil, store.Incarnation{})
	if err != nil {
		return nil, FileInfo{}, facadeErr(ctx, err)
	}
	if len(res.Rows) == 0 {
		return nil, FileInfo{}, derr.New(derr.NotFound, "table %s has no row %d", table, id)
	}
	ref, _ := res.Rows[0][field].(map[string]any)
	if ref == nil {
		return nil, FileInfo{}, derr.New(derr.NotFound, "row %d of %s holds no file in %q", id, table, field)
	}
	info := FileInfo{}
	info.SHA256, _ = ref["sha256"].(string)
	info.MediaType, _ = ref["media_type"].(string)
	info.Name, _ = ref["name"].(string)
	if n, ok := ref["size"].(json.Number); ok {
		info.Size, _ = n.Int64()
	}
	f, err := s.files.Open(ns, info.SHA256)
	if err != nil {
		return nil, FileInfo{}, err
	}
	return f, info, nil
}
