package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
)

func RefuseFileFields(engine string, fields []schema.Field) error {
	for _, f := range fields {
		if f.Type == schema.File {
			return invalidf("field %q: file fields are not supported on the %s engine yet; on PostgreSQL they will need the S3 file store, because a local store on one server's disk would be invisible to the others", f.Name, engine)
		}
	}
	return nil
}

type FileBlob struct {
	SHA256    string
	Size      int64
	MediaType string
	Name      string
}

type FileSource interface {
	Attach(ctx context.Context, ns, part string) (FileBlob, error)
	Describe(part string) (FileBlob, error)
	Pin(ns, sha256 string) error
}

type CopyReach func(ctx context.Context, ns, table string) (*RowScope, error)

type copyReachKey struct{}

func WithCopyReach(ctx context.Context, reach CopyReach) context.Context {
	return context.WithValue(ctx, copyReachKey{}, reach)
}

func HasFileSource(ctx context.Context) bool {
	return fileSourceFrom(ctx) != nil
}

type fileSourceKey struct{}

func WithFileSource(ctx context.Context, src FileSource) context.Context {
	return context.WithValue(ctx, fileSourceKey{}, src)
}

func fileSourceFrom(ctx context.Context) FileSource {
	src, _ := ctx.Value(fileSourceKey{}).(FileSource)
	return src
}

type fileValue struct {
	part      string
	copy      *fileCopy
	name      *string
	mediaType *string
}

type fileCopy struct {
	table string
	id    int64
	field string
}

func parseFileValue(f schema.Field, v any) (fileValue, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return fileValue{}, invalidf("field %q is a file field: write {\"part\": \"<form name>\"} and send the file in the same request as multipart/form-data, with the JSON request in a part named request (curl -F 'request=<json>' -F '<form name>=@path'); to clear it, write null", f.Name)
	}
	if _, isRead := m["sha256"]; isRead {
		return fileValue{}, invalidf("field %q: a file value read back from a row cannot be written; to attach a file, write {\"part\": \"<form name>\"} and send the file in the same multipart request, or write null to clear the field", f.Name)
	}
	if _, has := m["url"]; has {
		return fileValue{}, invalidf("field %q: \"url\" file values are not supported by this server yet; download the file and send it as a multipart part with {\"part\": \"<form name>\"}", f.Name)
	}
	var out fileValue
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		raw := m[k]
		switch k {
		case "part":
			s, ok := raw.(string)
			if !ok || s == "" {
				return fileValue{}, invalidf("field %q: \"part\" must be the form name of a file part in the same multipart request", f.Name)
			}
			out.part = s
		case "copy_from":
			c, err := parseFileCopy(f.Name, raw)
			if err != nil {
				return fileValue{}, err
			}
			out.copy = c
		case "name":
			s, ok := raw.(string)
			if !ok {
				return fileValue{}, invalidf("field %q: \"name\" must be a string", f.Name)
			}
			if err := schema.ValidateFileName(f.Name, s); err != nil {
				return fileValue{}, invalidf("%s", err)
			}
			out.name = &s
		case "media_type":
			s, ok := raw.(string)
			if !ok {
				return fileValue{}, invalidf("field %q: \"media_type\" must be a string", f.Name)
			}
			if err := schema.ValidateMediaType(f.Name, s); err != nil {
				return fileValue{}, invalidf("%s", err)
			}
			out.mediaType = &s
		default:
			return fileValue{}, invalidf("field %q: unknown key %q in a file value; a file value takes \"part\" or \"copy_from\", and optionally \"name\" and \"media_type\"", f.Name, k)
		}
	}
	if (out.part == "") == (out.copy == nil) {
		return fileValue{}, invalidf("field %q: a file value needs exactly one of \"part\", the form name of a file part in the same multipart request, or \"copy_from\", {\"table\", \"id\", \"field\"} of a row whose file to reuse", f.Name)
	}
	return out, nil
}

func parseFileCopy(field string, raw any) (*fileCopy, error) {
	m, ok := raw.(map[string]any)
	bad := invalidf("field %q: \"copy_from\" must be {\"table\": \"<table>\", \"id\": <row id>, \"field\": \"<file field>\"}", field)
	if !ok || len(m) != 3 {
		return nil, bad
	}
	c := &fileCopy{}
	c.table, _ = m["table"].(string)
	c.field, _ = m["field"].(string)
	c.table = strings.ToLower(strings.TrimSpace(c.table))
	c.field = strings.ToLower(strings.TrimSpace(c.field))
	switch id := m["id"].(type) {
	case json.Number:
		n, err := id.Int64()
		if err != nil {
			return nil, bad
		}
		c.id = n
	case float64:
		c.id = int64(id)
		if float64(c.id) != id {
			return nil, bad
		}
	case int64:
		c.id = id
	case int:
		c.id = int64(id)
	}
	if c.table == "" || c.field == "" || c.id < 1 {
		return nil, bad
	}
	return c, nil
}

func (fv fileValue) apply(b FileBlob) FileBlob {
	if fv.name != nil {
		b.Name = *fv.name
	}
	if fv.mediaType != nil {
		b.MediaType = *fv.mediaType
	}
	return b
}

func fileReference(b FileBlob) (string, error) {
	ref := map[string]any{"sha256": b.SHA256, "size": b.Size, "media_type": b.MediaType, "name": nil}
	if b.Name != "" {
		ref["name"] = b.Name
	}
	raw, err := json.Marshal(ref)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (s *Store) coerceFile(ctx context.Context, nsName string, f schema.Field, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	fv, err := parseFileValue(f, v)
	if err != nil {
		return nil, err
	}
	if fv.copy != nil {
		return s.copyFile(ctx, nsName, f, fv)
	}
	src := fileSourceFrom(ctx)
	if src == nil {
		return nil, invalidf("field %q: {\"part\": %q} needs the file in the same request, and this request carries none (a JSON body or an MCP call cannot); send the write over HTTP as multipart/form-data, with the JSON request in a part named request and the file in a part named %s (curl -F 'request=<json>' -F '%s=@path')", f.Name, fv.part, fv.part, fv.part)
	}
	b, err := src.Describe(fv.part)
	if err != nil {
		return nil, err
	}
	b = fv.apply(b)
	if err := fileLimits(f, "part "+fmt.Sprintf("%q", fv.part), b); err != nil {
		return nil, err
	}
	placed, err := src.Attach(ctx, nsName, fv.part)
	if err != nil {
		return nil, err
	}
	return fileReference(fv.apply(placed))
}

func fileLimits(f schema.Field, what string, b FileBlob) error {
	if f.MaxBytes > 0 && b.Size > f.MaxBytes {
		return &FileLimitError{Status: 413, Message: fmt.Sprintf("field %q accepts files up to %d bytes (max_bytes) and %s is %d bytes; store a smaller file, or keep larger files in a field without that limit", f.Name, f.MaxBytes, what, b.Size)}
	}
	if !schema.MediaTypeAllowed(f.MediaTypes, b.MediaType) {
		return &FileLimitError{Status: 415, Message: fmt.Sprintf("field %q accepts %s and %s is %s; if the type was guessed wrong, set \"media_type\" in the file value", f.Name, strings.Join(f.MediaTypes, ", "), what, b.MediaType)}
	}
	return nil
}

func (s *Store) copyFile(ctx context.Context, nsName string, f schema.Field, fv fileValue) (any, error) {
	c := fv.copy
	unreadable := invalidf("field %q: copy_from names no file you can read: table %s has no readable row %d with a file in field %q", f.Name, c.table, c.id, c.field)
	var scope *RowScope
	if reach, ok := ctx.Value(copyReachKey{}).(CopyReach); ok {
		var err error
		if scope, err = reach(ctx, nsName, c.table); err != nil {
			return nil, unreadable
		}
	}
	sc, _, err := s.TableState(ctx, nsName, c.table, nil)
	if err != nil {
		return nil, unreadable
	}
	if src := sc.Field(c.field); src == nil || src.Type != schema.File {
		return nil, unreadable
	}
	res, err := s.GetRows(ctx, nsName, c.table, []int64{c.id}, scope, Incarnation{})
	if err != nil || len(res.Rows) == 0 {
		return nil, unreadable
	}
	ref, _ := res.Rows[0][c.field].(map[string]any)
	if ref == nil {
		return nil, unreadable
	}
	b := FileBlob{}
	b.SHA256, _ = ref["sha256"].(string)
	b.MediaType, _ = ref["media_type"].(string)
	b.Name, _ = ref["name"].(string)
	if n, ok := ref["size"].(json.Number); ok {
		b.Size, _ = n.Int64()
	}
	b = fv.apply(b)
	if err := fileLimits(f, fmt.Sprintf("the file of %s row %d", c.table, c.id), b); err != nil {
		return nil, err
	}
	src := fileSourceFrom(ctx)
	if src == nil {
		return nil, invalidf("field %q: copy_from needs the server's file store, which this request does not have", f.Name)
	}
	if err := src.Pin(nsName, b.SHA256); err != nil {
		return nil, err
	}
	return fileReference(b)
}

type FileLimitError struct {
	Status  int
	Message string
}

func (e *FileLimitError) Error() string { return e.Message }

func (e *FileLimitError) Unwrap() error { return ErrInvalid }

func canonicalFiles(ctx context.Context, sc *schema.TableSchema, records []map[string]any) []map[string]any {
	ff := sc.FileFields()
	if len(ff) == 0 {
		return records
	}
	src := fileSourceFrom(ctx)
	out := make([]map[string]any, len(records))
	for i, rec := range records {
		cp := make(map[string]any, len(rec))
		for k, v := range rec {
			cp[k] = v
		}
		for _, f := range ff {
			v, ok := rec[f.Name]
			if !ok || v == nil {
				continue
			}
			fv, err := parseFileValue(f, v)
			if err != nil || src == nil {
				continue
			}
			b, derr := src.Describe(fv.part)
			if derr != nil {
				continue
			}
			b = fv.apply(b)
			cp[f.Name] = map[string]any{"sha256": b.SHA256, "media_type": b.MediaType, "name": b.Name}
		}
		out[i] = cp
	}
	return out
}

type FileSweep struct {
	Removed      int
	RemovedBytes int64
	Bytes        int64
}

type FileSweeper interface {
	Sweep(ns string, referenced map[string]bool) (FileSweep, error)
}

type fileSweeperKey struct{}

func WithFileSweeper(ctx context.Context, sw FileSweeper) context.Context {
	return context.WithValue(ctx, fileSweeperKey{}, sw)
}

func fileSweeperFrom(ctx context.Context) FileSweeper {
	sw, _ := ctx.Value(fileSweeperKey{}).(FileSweeper)
	return sw
}

func fileReferences(ctx context.Context, conn *sql.Conn) (map[string]bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name, schema_json FROM _dolmen_tables ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var tables []schema.TableSchema
	for rows.Next() {
		var name, raw string
		if err := rows.Scan(&name, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		var sc schema.TableSchema
		if err := json.Unmarshal([]byte(raw), &sc); err != nil {
			rows.Close()
			return nil, fmt.Errorf("corrupt schema for table %s: %w", name, err)
		}
		if len(sc.FileFields()) > 0 {
			sc.Name = name
			tables = append(tables, sc)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	refs := map[string]bool{}
	for _, sc := range tables {
		for _, f := range sc.FileFields() {
			r, err := conn.QueryContext(ctx, fmt.Sprintf(`SELECT DISTINCT json_extract(%s, '$.sha256') FROM %s WHERE %s IS NOT NULL`, q(f.Name), q(sc.Name), q(f.Name)))
			if err != nil {
				return nil, err
			}
			for r.Next() {
				var sum sql.NullString
				if err := r.Scan(&sum); err != nil {
					r.Close()
					return nil, err
				}
				if sum.Valid {
					refs[sum.String] = true
				}
			}
			r.Close()
			if err := r.Err(); err != nil {
				return nil, err
			}
		}
	}
	return refs, nil
}
