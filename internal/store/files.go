package store

import (
	"context"
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
	name      *string
	mediaType *string
}

func parseFileValue(f schema.Field, v any) (fileValue, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return fileValue{}, invalidf("field %q is a file field: write {\"part\": \"<form name>\"} and send the file in the same request as multipart/form-data, with the JSON request in a part named request (curl -F 'request=<json>' -F '<form name>=@path'); to clear it, write null", f.Name)
	}
	if _, isRead := m["sha256"]; isRead {
		return fileValue{}, invalidf("field %q: a file value read back from a row cannot be written; to attach a file, write {\"part\": \"<form name>\"} and send the file in the same multipart request, or write null to clear the field", f.Name)
	}
	for _, k := range []string{"url", "copy_from"} {
		if _, has := m[k]; has {
			return fileValue{}, invalidf("field %q: %q file values are not supported by this server yet; download the file and send it as a multipart part with {\"part\": \"<form name>\"}", f.Name, k)
		}
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
			return fileValue{}, invalidf("field %q: unknown key %q in a file value; a file value takes \"part\", and optionally \"name\" and \"media_type\"", f.Name, k)
		}
	}
	if out.part == "" {
		return fileValue{}, invalidf("field %q: a file value needs \"part\", the form name of a file part in the same multipart request", f.Name)
	}
	return out, nil
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

func coerceFile(ctx context.Context, nsName string, f schema.Field, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	fv, err := parseFileValue(f, v)
	if err != nil {
		return nil, err
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
	if f.MaxBytes > 0 && b.Size > f.MaxBytes {
		return nil, &FileLimitError{Status: 413, Message: fmt.Sprintf("field %q accepts files up to %d bytes (max_bytes) and part %q is %d bytes; store a smaller file, or keep larger files in a field without that limit", f.Name, f.MaxBytes, fv.part, b.Size)}
	}
	if !schema.MediaTypeAllowed(f.MediaTypes, b.MediaType) {
		return nil, &FileLimitError{Status: 415, Message: fmt.Sprintf("field %q accepts %s and part %q is %s; if the type was guessed wrong, set \"media_type\" in the file value", f.Name, strings.Join(f.MediaTypes, ", "), fv.part, b.MediaType)}
	}
	placed, err := src.Attach(ctx, nsName, fv.part)
	if err != nil {
		return nil, err
	}
	return fileReference(fv.apply(placed))
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
