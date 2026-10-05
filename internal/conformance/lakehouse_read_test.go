package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/lakehouse"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
)

type lakehouseReadEngine interface {
	lakehouseAppendEngine
	TableState(context.Context, string, string, []store.AuthBinding) (*schema.TableSchema, store.Incarnation, error)
	GetRows(context.Context, string, string, []int64, *store.RowScope, store.Incarnation) (store.QueryResult, error)
}

func TestLakehouseTypedReadsBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "lakehouse"} {
		t.Run(backend, func(t *testing.T) {
			key, err := secret.New(make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			var raw namespaceEngine
			if backend == "lakehouse" {
				raw, err = lakehouse.Open(t.TempDir(), lakehouse.WithSecretKeyring(key))
			} else {
				raw, err = store.Open(t.TempDir(), store.WithSecretKey(key))
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { raw.Close() })
			eng, ok := raw.(lakehouseReadEngine)
			if !ok {
				t.Fatal("engine has no typed read path")
			}
			ctx := t.Context()
			ns := "project"
			if err := eng.CreateNamespace(ctx, ns, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			fields := []schema.Field{
				{Name: "title", Type: schema.String},
				{Name: "score", Type: schema.Number},
				{Name: "ratio", Type: schema.Number},
				{Name: "ok", Type: schema.Boolean, Default: false},
				{Name: "meta", Type: schema.JSON},
				{Name: "at", Type: schema.Timestamp},
				{Name: "v", Type: schema.Vector, Dim: 2},
				{Name: "token", Type: schema.Secret},
			}
			if _, err := eng.CreateTable(ctx, ns, "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			records := []map[string]any{
				{"title": "a", "score": 3, "ratio": 2.5, "ok": true, "meta": map[string]any{"k": []any{1, "x"}}, "at": "2026-10-05T12:00:00Z", "v": []any{1, 0.5}, "token": "PLAINTEXT-a"},
				{"title": "b"},
				{"title": "never requested"},
			}
			if _, err := eng.Insert(ctx, ns, "notes", records, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
				t.Fatal(err)
			}
			res, err := eng.GetRows(ctx, ns, "notes", []int64{2, 99, 1}, nil, store.Incarnation{})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Rows) != 2 || res.Truncated {
				t.Fatalf("read_rows must return each found row once, ascending, and skip missing ids: %+v", res)
			}
			first, second := res.Rows[0], res.Rows[1]
			want := map[string]any{
				"id":    int64(1),
				"title": "a",
				"score": int64(3),
				"ratio": 2.5,
				"ok":    true,
				"meta":  map[string]any{"k": []any{json.Number("1"), "x"}},
				"at":    "2026-10-05T12:00:00Z",
				"v":     []float64{1, 0.5},
				"token": secret.Mask,
			}
			for k, v := range want {
				if !reflect.DeepEqual(first[k], v) {
					t.Fatalf("%s = %#v, want %#v", k, first[k], v)
				}
			}
			if _, ok := first["created_at"].(string); !ok {
				t.Fatalf("created_at must read back as text: %#v", first["created_at"])
			}
			if _, ok := first["_embedding"]; ok {
				t.Fatal("read_rows must not expose the hidden _embedding column")
			}
			if second["id"] != int64(2) || second["score"] != nil || second["ok"] != false || second["token"] != nil || second["meta"] != nil {
				t.Fatalf("an omitted field reads back null, or its default: %#v", second)
			}

			revealed, err := eng.GetRows(store.WithReveal(ctx, []string{"token"}), ns, "notes", []int64{1}, nil, store.Incarnation{})
			if err != nil || revealed.Rows[0]["token"] != "PLAINTEXT-a" {
				t.Fatalf("reveal must return the plaintext: %+v %v", revealed, err)
			}
			if _, err := eng.GetRows(ctx, ns, "notes", make([]int64, store.MaxReadRowsIDs+1), nil, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("more than %d ids must be refused: %v", store.MaxReadRowsIDs, err)
			}

			if _, err := eng.CreateTable(ctx, ns, "mine", []schema.Field{{Name: "body", Type: schema.Text}}, store.TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			for _, owner := range []string{"alice", "bob"} {
				if _, err := eng.Insert(ctx, ns, "mine", []map[string]any{{"body": owner}}, store.WriteOpts{Owner: owner}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
					t.Fatal(err)
				}
			}
			scoped, err := eng.GetRows(ctx, ns, "mine", []int64{1, 2}, &store.RowScope{Owner: "alice"}, store.Incarnation{})
			if err != nil || len(scoped.Rows) != 1 || scoped.Rows[0]["body"] != "alice" {
				t.Fatalf("a row scope must hide other owners' rows: %+v %v", scoped, err)
			}
			none, err := eng.GetRows(ctx, ns, "mine", []int64{1, 2}, &store.RowScope{Empty: true}, store.Incarnation{})
			if err != nil || len(none.Rows) != 0 {
				t.Fatalf("an empty scope reads nothing: %+v %v", none, err)
			}
			_, inc, err := eng.TableState(ctx, ns, "mine", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := eng.GetRows(ctx, ns, "mine", []int64{1}, &store.RowScope{Owner: "alice"}, inc); err != nil {
				t.Fatalf("a current incarnation must be accepted: %v", err)
			}
			stale := inc
			stale.Version++
			if _, err := eng.GetRows(ctx, ns, "mine", []int64{1}, &store.RowScope{Owner: "alice"}, stale); !errors.Is(err, derr.ErrConflict) {
				t.Fatalf("a scope resolved against another schema version must be refused: %v", err)
			}

			if _, err := eng.CreateTable(ctx, ns, "wide", []schema.Field{{Name: "n", Type: schema.Number}, {Name: "doc", Type: schema.JSON}}, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			big := strings.Repeat("x", 2<<20)
			wide := []map[string]any{{"n": float64(1 << 62)}}
			for range 20 {
				wide = append(wide, map[string]any{"doc": map[string]any{"s": big}})
			}
			if _, err := eng.Insert(ctx, ns, "wide", wide, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
				t.Fatal(err)
			}
			exact, err := eng.GetRows(ctx, ns, "wide", []int64{1}, nil, store.Incarnation{})
			if err != nil || exact.Rows[0]["n"] != int64(1<<62) {
				t.Fatalf("an integral double reads back as the integer it equals: %#v %v", exact.Rows, err)
			}
			ids := make([]int64, 21)
			for i := range ids {
				ids[i] = int64(i + 1)
			}
			budget, err := eng.GetRows(ctx, ns, "wide", ids, nil, store.Incarnation{})
			if err != nil || !budget.Truncated || len(budget.Rows) >= 21 {
				t.Fatalf("json fields count their stored size against the response budget: %d rows, truncated %v, %v", len(budget.Rows), budget.Truncated, err)
			}
		})
	}
}
