package store

import (
	"context"

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
