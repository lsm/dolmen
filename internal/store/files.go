package store

import "github.com/lsm/dolmen/internal/schema"

func RefuseFileFields(engine string, fields []schema.Field) error {
	for _, f := range fields {
		if f.Type == schema.File {
			return invalidf("field %q: file fields are not supported on the %s engine yet; on PostgreSQL they will need the S3 file store, because a local store on one server's disk would be invisible to the others", f.Name, engine)
		}
	}
	return nil
}
