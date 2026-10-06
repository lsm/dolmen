package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/lsm/dolmen/internal/derr"
	"reflect"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/lakehouse"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
)

type lakehouseMutationEngine interface {
	lakehouseReadEngine
	Update(context.Context, string, string, string, []any, map[string]any, store.Embedder, *store.RowScope, store.Incarnation) (store.UpdateResult, error)
	Upsert(context.Context, string, string, string, []any, map[string]any, store.WriteOpts, store.Embedder, *store.RowScope, store.Incarnation) (store.InsertResult, error)
	UpsertByKey(context.Context, string, string, []string, []map[string]any, store.WriteOpts, store.Embedder, *store.RowScope, store.Incarnation) (store.InsertResult, error)
	Delete(context.Context, string, string, string, []any, store.DeleteOpts, *store.RowScope, store.Incarnation) (store.DeleteResult, error)
}

func TestLakehouseMutationBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "lakehouse"} {
		t.Run(backend, func(t *testing.T) {
			key, err := secret.New(make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			var raw namespaceEngine
			if backend == "lakehouse" {
				raw, err = lakehouse.Open(t.TempDir(), lakehouse.WithSecretKeyring(key), lakehouse.WithSQLEngine(lakehouseSQLEngine(t)))
			} else {
				raw, err = store.Open(t.TempDir(), store.WithSecretKey(key))
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { raw.Close() })
			eng, ok := raw.(lakehouseMutationEngine)
			if !ok {
				t.Fatal("engine has no mutation path")
			}
			ctx := t.Context()
			ns := "project"
			if err := eng.CreateNamespace(ctx, ns, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			fields := []schema.Field{
				{Name: "email", Type: schema.String, Required: true},
				{Name: "name", Type: schema.String},
				{Name: "score", Type: schema.Number},
				{Name: "token", Type: schema.Secret},
			}
			if _, err := eng.CreateTable(ctx, ns, "people", fields, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			none := store.Incarnation{}
			emb := store.Embedder{}
			if _, err := eng.Insert(ctx, ns, "people", []map[string]any{
				{"email": "a@x", "name": "Ada", "score": 1, "token": "PLAINTEXT-a"},
				{"email": "b@x", "name": "Bob", "score": 2},
				{"email": "c@x", "name": "Cy", "score": 3},
			}, store.WriteOpts{}, emb, nil, none); err != nil {
				t.Fatal(err)
			}
			read := func(ids ...int64) []map[string]any {
				t.Helper()
				res, err := eng.GetRows(store.WithReveal(ctx, []string{"token"}), ns, "people", ids, nil, none)
				if err != nil {
					t.Fatal(err)
				}
				return res.Rows
			}
			count := func() int64 {
				t.Helper()
				_, n, err := eng.DescribeTable(ctx, ns, "people", nil, none)
				if err != nil {
					t.Fatal(err)
				}
				return n
			}

			up, err := eng.Update(ctx, ns, "people", "score >= ?", []any{2}, map[string]any{"name": "Two+"}, emb, nil, none)
			if err != nil || up.Updated != 2 || up.Changes.Count != 2 {
				t.Fatalf("update of two rows = %+v %v", up, err)
			}
			rows := read(1, 2, 3)
			if rows[0]["name"] != "Ada" || rows[1]["name"] != "Two+" || rows[2]["name"] != "Two+" || rows[1]["email"] != "b@x" {
				t.Fatalf("update must change the matched rows only and keep their other fields: %v", rows)
			}
			if rows[0]["token"] != "PLAINTEXT-a" {
				t.Fatalf("an update that does not name a secret must keep it: %v", rows[0])
			}
			if _, err := eng.Update(ctx, ns, "people", "id = 1", nil, map[string]any{"email": nil}, emb, nil, none); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("setting a required field to null must be refused: %v", err)
			}
			if _, err := eng.Update(ctx, ns, "people", "id = 1", nil, map[string]any{"token": nil, "score": 1.5}, emb, nil, none); err != nil {
				t.Fatal(err)
			}
			if row := read(1)[0]; row["token"] != nil || row["score"] != 1.5 {
				t.Fatalf("set to null clears a secret and numbers keep their value: %v", row)
			}
			miss, err := eng.Update(ctx, ns, "people", "email = ?", []any{"nobody"}, map[string]any{"name": "x"}, emb, nil, none)
			if err != nil || miss.Updated != 0 {
				t.Fatalf("an update matching nothing changes nothing: %+v %v", miss, err)
			}

			inserted, err := eng.Upsert(ctx, ns, "people", "email = ?", []any{"d@x"}, map[string]any{"email": "d@x", "name": "Dee"}, store.WriteOpts{}, emb, nil, none)
			if err != nil || !reflect.DeepEqual(inserted.Ids, []int64{4}) {
				t.Fatalf("upsert with no match inserts one row: %+v %v", inserted, err)
			}
			updated, err := eng.Upsert(ctx, ns, "people", "email = ?", []any{"d@x"}, map[string]any{"name": "Dee2"}, store.WriteOpts{}, emb, nil, none)
			if err != nil || !reflect.DeepEqual(updated.Ids, []int64{4}) {
				t.Fatalf("upsert with a match updates it: %+v %v", updated, err)
			}
			if read(4)[0]["name"] != "Dee2" || count() != 4 {
				t.Fatalf("after the upserts: %v, row_count %d", read(4), count())
			}

			byKey, err := eng.UpsertByKey(ctx, ns, "people", []string{"email"}, []map[string]any{
				{"email": "a@x", "name": "Ada2"},
				{"email": "e@x", "name": "Eve"},
				{"email": "e@x", "name": "Eve2"},
			}, store.WriteOpts{}, emb, nil, none)
			if err != nil || !reflect.DeepEqual(byKey.Ids, []int64{1, 5, 5}) {
				t.Fatalf("upsert_by_key = %+v %v; want ids [1 5 5]", byKey, err)
			}
			if read(1)[0]["name"] != "Ada2" || read(5)[0]["name"] != "Eve2" || count() != 5 {
				t.Fatalf("after upsert_by_key: %v %v, row_count %d", read(1), read(5), count())
			}

			dry, err := eng.Delete(ctx, ns, "people", "score >= 2", nil, store.DeleteOpts{DryRun: true}, nil, none)
			if err != nil || dry.Matched != 2 || dry.Deleted != 0 {
				t.Fatalf("a dry run reports what would match and deletes nothing: %+v %v", dry, err)
			}
			if _, err := eng.Delete(ctx, ns, "people", "1=1", nil, store.DeleteOpts{Limit: 2}, nil, none); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("a delete over its limit must be refused: %v", err)
			}
			del, err := eng.Delete(ctx, ns, "people", "score >= 2", nil, store.DeleteOpts{}, nil, none)
			if err != nil || del.Deleted != 2 || del.Changes.Count != 2 {
				t.Fatalf("delete = %+v %v", del, err)
			}
			if got := read(1, 2, 3, 4, 5); len(got) != 3 || count() != 3 {
				t.Fatalf("a deleted row must not read back: %v, row_count %d", got, count())
			}
			ghosts, err := eng.Update(ctx, ns, "people", "email IN ('b@x', 'c@x')", nil, map[string]any{"name": "ghost"}, emb, nil, none)
			if err != nil || ghosts.Updated != 0 {
				t.Fatalf("a filter must not see rows removed by an earlier delete: %+v %v", ghosts, err)
			}
			again, err := eng.Delete(ctx, ns, "people", "email IN ('b@x', 'c@x')", nil, store.DeleteOpts{}, nil, none)
			if err != nil || again.Matched != 0 || again.Deleted != 0 || count() != 3 {
				t.Fatalf("deleting already-deleted rows must match nothing: %+v %v, row_count %d", again, err, count())
			}
			if _, err := eng.Delete(ctx, ns, "people", "token = ?", []any{secret.Mask}, store.DeleteOpts{DryRun: true}, nil, none); err != nil {
				t.Fatal(err)
			}
			masked, err := eng.Delete(ctx, ns, "people", "token = ?", []any{secret.Mask}, store.DeleteOpts{DryRun: true}, nil, none)
			if err != nil || masked.Matched != 0 {
				t.Fatalf("a filter comparing a secret with the mask must match nothing: %+v %v", masked, err)
			}
			numeric, err := eng.UpsertByKey(ctx, ns, "people", []string{"score"}, []map[string]any{
				{"email": "n@x", "score": json.Number("50")},
				{"email": "n@x", "name": "Fifty", "score": json.Number("50.0")},
			}, store.WriteOpts{}, emb, nil, none)
			if err != nil || len(numeric.Ids) != 2 || numeric.Ids[0] != numeric.Ids[1] || numeric.Inserted != 1 || numeric.Changes.Count != 2 {
				t.Fatalf("records whose number keys are equal values must land on one row, one change each: %+v %v", numeric, err)
			}
			big, err := eng.Insert(ctx, ns, "people", []map[string]any{{"email": "big1@x", "score": json.Number("9007199254740992")}, {"email": "big2@x", "score": json.Number("9007199254740993")}}, store.WriteOpts{}, emb, nil, none)
			if err != nil {
				t.Fatal(err)
			}
			exact, err := eng.UpsertByKey(ctx, ns, "people", []string{"score"}, []map[string]any{{"email": "big2@x", "name": "Exact", "score": json.Number("9007199254740993")}}, store.WriteOpts{}, emb, nil, none)
			if err != nil || exact.Updated != 1 || !reflect.DeepEqual(exact.Ids, []int64{big.Ids[1]}) {
				t.Fatalf("a number key above 2^53 must match its exact value only: %+v %v", exact, err)
			}
			if _, err := eng.CreateTable(ctx, ns, "pair", []schema.Field{{Name: "a", Type: schema.String}, {Name: "b", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			pair, err := eng.UpsertByKey(ctx, ns, "pair", []string{"a", "b"}, []map[string]any{{"a": "a\x1fb", "b": "c"}, {"a": "a", "b": "b\x1fc"}}, store.WriteOpts{}, emb, nil, none)
			if err != nil || pair.Inserted != 2 || pair.Ids[0] == pair.Ids[1] {
				t.Fatalf("distinct composite keys must stay distinct whatever their bytes: %+v %v", pair, err)
			}
			_, inc, err := eng.TableState(ctx, ns, "people", nil)
			if err != nil {
				t.Fatal(err)
			}
			stale := inc
			stale.Version++
			if _, err := eng.Update(ctx, ns, "people", "id = 1", nil, map[string]any{"name": "x"}, emb, nil, stale); !errors.Is(err, derr.ErrConflict) {
				t.Fatalf("an update scoped against another schema version must be refused: %v", err)
			}
			if _, err := eng.Delete(ctx, ns, "people", "id = 1", nil, store.DeleteOpts{}, nil, stale); !errors.Is(err, derr.ErrConflict) {
				t.Fatalf("a delete scoped against another schema version must be refused: %v", err)
			}
			if _, err := eng.UpsertByKey(ctx, ns, "people", []string{"email"}, []map[string]any{{"email": "a@x"}}, store.WriteOpts{}, emb, nil, stale); !errors.Is(err, derr.ErrConflict) {
				t.Fatalf("an upsert_by_key scoped against another schema version must be refused: %v", err)
			}
			if _, err := eng.Upsert(ctx, ns, "people", "id = 1", nil, map[string]any{"name": "x"}, store.WriteOpts{}, emb, nil, stale); !errors.Is(err, derr.ErrConflict) {
				t.Fatalf("an upsert scoped against another schema version must be refused: %v", err)
			}
			if _, err := eng.CreateTable(ctx, ns, "stamped", []schema.Field{{Name: "at", Type: schema.Timestamp}}, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Insert(ctx, ns, "stamped", []map[string]any{{"at": "2026-01-01T00:00:00Z"}}, store.WriteOpts{}, emb, nil, none); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Update(ctx, ns, "stamped", "1=1", nil, map[string]any{"at": "now()"}, emb, nil, none); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "now()") {
				t.Fatalf("an update's error must name the value the caller sent: %v", err)
			}
			if semi, err := eng.Delete(ctx, ns, "people", "name = 'a;b'", nil, store.DeleteOpts{DryRun: true}, nil, none); err != nil || semi.Matched != 0 {
				t.Fatalf("a semicolon inside a string literal is not a statement separator: %+v %v", semi, err)
			}
			if _, err := eng.CreateTable(ctx, ns, "keyed", []schema.Field{{Name: "name", Type: schema.String}, {Name: "meta", Type: schema.JSON}}, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Insert(ctx, ns, "keyed", []map[string]any{{"name": "dup", "meta": map[string]any{"a": 1}}, {"name": "dup"}}, store.WriteOpts{}, emb, nil, none); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.UpsertByKey(ctx, ns, "keyed", []string{"name"}, []map[string]any{{"name": "dup", "meta": map[string]any{"b": 2}}}, store.WriteOpts{}, emb, nil, none); !errors.Is(err, derr.ErrConflict) {
				t.Fatalf("a natural key matching several rows must be a conflict: %v", err)
			}
			if _, err := eng.UpsertByKey(ctx, ns, "keyed", []string{"name", " NAME"}, []map[string]any{{"name": "x"}}, store.WriteOpts{}, emb, nil, none); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "duplicate key field") {
				t.Fatalf("a key field named twice must be refused: %v", err)
			}
			if _, err := eng.UpsertByKey(ctx, ns, "keyed", []string{"meta"}, []map[string]any{{"meta": map[string]any{"a": 1}}}, store.WriteOpts{}, emb, nil, none); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "natural keys must be string, text, number, boolean, or timestamp fields") {
				t.Fatalf("a json key field must be refused: %v", err)
			}
			if _, err := eng.UpsertByKey(ctx, ns, "keyed", []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}, []map[string]any{{"name": "x"}}, store.WriteOpts{}, emb, nil, none); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "too many key fields") {
				t.Fatalf("more than %d key fields must be refused: %v", store.MaxKeyFields, err)
			}
		})
	}
}
