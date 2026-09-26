package dolmen

import (
	"context"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/store"
)

type SecretRotation struct {
	Rotated   int64
	Remaining int64
	Keys      map[string]int64
	Tables    []SecretTableRotation
}

type SecretTableRotation struct {
	Table     string
	Rotated   int64
	Remaining int64
}

func (s *Store) RotateSecretKey(ctx context.Context, namespace string, limit int) (SecretRotation, error) {
	if err := s.begin(); err != nil {
		return SecretRotation{}, err
	}
	defer s.done()
	if err := ctx.Err(); err != nil {
		return SecretRotation{}, facadeErr(err)
	}
	if limit < 0 {
		return SecretRotation{}, derr.New(derr.InvalidRequest, "RotateSecretKey: limit must not be negative (0 rotates everything in one call)")
	}
	res, err := s.eng.RotateSecrets(ctx, ops.NormalizeNamespace(namespace), store.RotateOpts{Limit: limit})
	if err != nil {
		return SecretRotation{}, facadeErr(err)
	}
	out := SecretRotation{Rotated: res.Rotated(), Remaining: res.Remaining(), Keys: map[string]int64{}}
	for _, t := range res.Tables {
		out.Tables = append(out.Tables, SecretTableRotation{Table: t.Table, Rotated: t.Rotated, Remaining: t.Remaining})
		for id, n := range t.Keys {
			out.Keys[id] += n
		}
	}
	return out, nil
}
