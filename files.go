package dolmen

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	req  *files.Request
	n    int
	salt string
}

type FileURL struct {
	URL       string
	Name      string
	MediaType string
}

type FileCopy struct {
	Table     string
	ID        int64
	Field     string
	Name      string
	MediaType string
}

func (s *Store) withFiles(ctx context.Context, maps []map[string]any) (context.Context, []map[string]any, *fileParts, error) {
	var fp *fileParts
	done := false
	defer func() {
		if !done {
			fp.finish(nil)
		}
	}()
	out := maps
	copied := map[int]bool{}
	for i, m := range maps {
		for k, v := range m {
			fv, ok := fileValue(v)
			if !ok {
				continue
			}
			if s.files == nil {
				return ctx, nil, nil, derr.New(derr.InvalidRequest, "field %q: this store cannot hold files; file fields need the SQLite or lakehouse engine", k)
			}
			if fp == nil {
				fp = &fileParts{req: s.files.NewRequest(), salt: randomSalt()}
				out = append([]map[string]any(nil), maps...)
			}
			if !copied[i] {
				cp := make(map[string]any, len(m))
				for kk, vv := range m {
					cp[kk] = vv
				}
				out[i], copied[i] = cp, true
			}
			val, err := s.fileWireValue(k, fv, fp)
			if err != nil {
				return ctx, nil, nil, err
			}
			out[i][k] = val
		}
	}
	done = true
	if fp == nil {
		return ctx, maps, nil, nil
	}
	return store.WithFileSource(ctx, fp.req), out, fp, nil
}

var libraryFetchWording = files.FetchWording{
	Hosts:    "the hosts given to WithFileFetch",
	Timeout:  "the WithFileFetch timeout",
	Fallback: "read the file yourself and pass a dolmen.FileUpload",
}

func (s *Store) fileWireValue(field string, v any, fp *fileParts) (map[string]any, error) {
	val := map[string]any{}
	name, mediaType := "", ""
	switch f := v.(type) {
	case FileUpload:
		if f.Reader == nil {
			return nil, derr.New(derr.InvalidRequest, "field %q: dolmen.FileUpload needs a Reader", field)
		}
		if base := files.BaseName(f.Name); base != "" {
			if err := schema.ValidateFileName(field, base); err != nil {
				return nil, derr.New(derr.InvalidRequest, "%s; set dolmen.FileUpload.Name to a valid base name, or leave it empty", err)
			}
		}
		fp.n++
		part := fmt.Sprintf("upload-%s-%d", fp.salt, fp.n)
		if err := fp.req.Receive(part, f.Name, f.MediaType, f.Reader); err != nil {
			return nil, err
		}
		return map[string]any{"part": part}, nil
	case FileURL:
		if !s.files.Fetches() {
			return nil, derr.New(derr.InvalidRequest, "field %q: dolmen.FileURL needs Open(dir, dolmen.WithFileFetch(hosts, timeout)) naming the hosts to fetch from; or read the file yourself and pass a dolmen.FileUpload", field)
		}
		val["url"], name, mediaType = f.URL, f.Name, f.MediaType
	case FileCopy:
		val["copy_from"] = map[string]any{"table": f.Table, "id": f.ID, "field": f.Field}
		name, mediaType = f.Name, f.MediaType
	}
	if name != "" {
		val["name"] = name
	}
	if mediaType != "" {
		val["media_type"] = mediaType
	}
	return val, nil
}

func fileValue(v any) (any, bool) {
	switch x := v.(type) {
	case FileUpload, FileURL, FileCopy:
		return x, true
	case *FileUpload:
		if x != nil {
			return *x, true
		}
	case *FileURL:
		if x != nil {
			return *x, true
		}
	case *FileCopy:
		if x != nil {
			return *x, true
		}
	}
	return nil, false
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
	res, err := s.eng.GetRows(ctx, ns, tbl, []int64{id}, nil, store.Incarnation{})
	if err != nil {
		return nil, FileInfo{}, facadeErr(ctx, err)
	}
	if len(res.Rows) == 0 {
		return nil, FileInfo{}, derr.New(derr.NotFound, "table %s has no row %d", table, id)
	}
	sc, _, err := s.eng.TableState(ctx, ns, tbl, nil)
	if err != nil {
		return nil, FileInfo{}, facadeErr(ctx, err)
	}
	fd := sc.Field(field)
	if fd == nil {
		return nil, FileInfo{}, derr.New(derr.NotFound, "table %s has no field %q", table, field)
	}
	if fd.Type != schema.File {
		return nil, FileInfo{}, derr.New(derr.InvalidRequest, "field %q of %s is not a file field", field, table)
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
	f, err := s.files.Open(ctx, ns, info.SHA256)
	if err != nil {
		return nil, FileInfo{}, &derr.Error{Code: derr.Internal, Message: fmt.Sprintf("the file of %s row %d field %q cannot be read from the file store", table, id, field), Cause: err}
	}
	return f, info, nil
}

func randomSalt() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
