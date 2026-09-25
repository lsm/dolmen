package postgres

import (
	"context"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func secretUnsupported(field string) error {
	return invalidf("field %q has type secret, which is not yet supported on the postgres engine; store it on a server running -engine sqlite, or use another field type until postgres support lands", field)
}

func refuseSecretFields(fields []schema.Field) error {
	for _, f := range fields {
		if f.Type == schema.Secret {
			return secretUnsupported(f.Name)
		}
	}
	return nil
}

func refuseSecretChanges(changes []schema.Change) error {
	for _, ch := range changes {
		if ch.Op == schema.OpAddField && ch.Field != nil && ch.Field.Type == schema.Secret {
			return secretUnsupported(ch.Field.Name)
		}
	}
	return nil
}

func refuseReveal(ctx context.Context) error {
	if names := store.RevealFrom(ctx); len(names) > 0 {
		return invalidf("reveal names %q, but the postgres engine does not yet support secret fields, so no table here has one to reveal; omit reveal", names[0])
	}
	return nil
}
