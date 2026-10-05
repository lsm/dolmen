package conformance

import (
	"bytes"
	"context"
	"testing"

	"github.com/lsm/dolmen/internal/lakehouse"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
)

type lakehouseRotateEngine interface {
	lakehouseReadEngine
	SecretKeyID() string
	RotateSecrets(context.Context, string, store.RotateOpts) (store.SecretRotation, error)
}

func TestLakehouseSecretRotationBackendConformance(t *testing.T) {
	oldKey, newKey := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	for _, backend := range []string{"sqlite", "lakehouse"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			open := func(k *secret.Keyring) lakehouseRotateEngine {
				t.Helper()
				var raw namespaceEngine
				var err error
				if backend == "lakehouse" {
					raw, err = lakehouse.Open(dir, lakehouse.WithSecretKeyring(k))
				} else {
					raw, err = store.Open(dir, store.WithSecretKey(k))
				}
				if err != nil {
					t.Fatal(err)
				}
				eng, ok := raw.(lakehouseRotateEngine)
				if !ok {
					t.Fatal("engine has no secret rotation")
				}
				return eng
			}
			first, err := secret.New(oldKey)
			if err != nil {
				t.Fatal(err)
			}
			eng := open(first)
			ctx := t.Context()
			none := store.Incarnation{}
			if err := eng.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.CreateTable(ctx, "ns", "t", []schema.Field{{Name: "token", Type: schema.Secret}}, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Insert(ctx, "ns", "t", []map[string]any{{"token": "s1"}, {"token": "s2"}, {}}, store.WriteOpts{}, store.Embedder{}, nil, none); err != nil {
				t.Fatal(err)
			}
			eng.Close()
			second, err := secret.New(newKey, oldKey)
			if err != nil {
				t.Fatal(err)
			}
			eng = open(second)
			defer eng.Close()
			if eng.SecretKeyID() != second.ID() {
				t.Fatalf("SecretKeyID = %q", eng.SecretKeyID())
			}
			partial, err := eng.RotateSecrets(ctx, "ns", store.RotateOpts{Limit: 1})
			if err != nil || partial.Rotated() != 1 || partial.Remaining() != 1 {
				t.Fatalf("a limited rotation = %+v %v", partial, err)
			}
			full, err := eng.RotateSecrets(ctx, "ns", store.RotateOpts{})
			if err != nil || full.Rotated() != 1 || full.Remaining() != 0 || full.Tables[0].Keys[second.ID()] != 2 {
				t.Fatalf("a full rotation = %+v %v", full, err)
			}
			res, err := eng.GetRows(store.WithReveal(ctx, []string{"token"}), "ns", "t", []int64{1, 2}, nil, none)
			if err != nil || res.Rows[0]["token"] != "s1" || res.Rows[1]["token"] != "s2" {
				t.Fatalf("rotated secrets still open: %+v %v", res, err)
			}
		})
	}
}
