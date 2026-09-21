package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestPostgresGetRowsTypedAndLifetime(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("a", 64)
	fields := []schema.Field{{Name: long}, {Name: "n", Type: schema.Number}, {Name: "active", Type: schema.Boolean}, {Name: "metadata", Type: schema.JSON}, {Name: "vec", Type: schema.Vector, Dim: 2}, {Name: "body", Vectorize: true}}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	_, inc, err := s.TableState(ctx, "app", "notes", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = s.write(ctx, "app", inc.NsGen, func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, "notes")
		if err != nil {
			return err
		}
		for _, number := range []string{"9007199254740993", "-9223372036854775808", "2.5"} {
			_, err = tx.Exec(ctx, "INSERT INTO "+ident(n.physical, state.physical)+" ("+ident(state.columns[long])+",n,active,metadata,vec,_embedding) VALUES($1,$2,true,$3,$4,$4)", "name", number, `{"exact":9007199254740993,"tiny":1e-400}`, schema.EncodeVector([]float32{0.5, -1}))
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{3, 1, 2, 1, 999}
	result, err := s.GetRows(ctx, "app", "notes", ids, nil, inc)
	if err != nil || result.Truncated || len(result.Rows) != 3 {
		t.Fatalf("read: %+v %v", result, err)
	}
	if !reflect.DeepEqual(ids, []int64{3, 1, 2, 1, 999}) {
		t.Fatal("input IDs mutated")
	}
	for i, want := range []any{int64(9007199254740993), int64(-9223372036854775808), float64(2.5)} {
		row := result.Rows[i]
		if row["id"] != int64(i+1) || row["n"] != want || row[long] != "name" || row["active"] != true || row["body"] != nil {
			t.Fatalf("typed row: %#v", row)
		}
		if _, ok := row["_embedding"]; ok {
			t.Fatal("hidden embedding exposed")
		}
		if !reflect.DeepEqual(row["vec"], []float64{0.5, -1}) {
			t.Fatalf("vector: %#v", row["vec"])
		}
		metadata := row["metadata"].(map[string]any)
		if metadata["exact"] != json.Number("9007199254740993") || metadata["tiny"] != json.Number("1e-400") {
			t.Fatalf("JSON fidelity: %#v", metadata)
		}
	}
	if _, err := s.GetRows(ctx, "app", "notes", ids, &store.RowScope{}, inc); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("a row scope on a table that carries no owner column must be refused as invalid, the way SQLite refuses it: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.GetRows(canceled, "app", "notes", ids, nil, inc); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if err := s.DropTable(ctx, "app", "notes", inc); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, inc.NsGen); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRows(ctx, "app", "notes", nil, nil, inc); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("an incarnation resolved against the dropped table must fail as a conflict, the way SQLite fails it, so the caller re-reads and retries: %v", err)
	}
}

func TestPostgresGetRowsBudget(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body", Type: schema.Text}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	err := s.write(ctx, "app", [16]byte{}, func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, "notes")
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO "+ident(n.physical, state.physical)+" (body) SELECT repeat('x',12*1024*1024) FROM generate_series(1,4)")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.GetRows(ctx, "app", "notes", []int64{1, 2, 3, 4}, nil, store.Incarnation{})
	if err != nil || !result.Truncated || len(result.Rows) != 2 {
		t.Fatalf("budget: rows=%d truncated=%v err=%v", len(result.Rows), result.Truncated, err)
	}
	err = s.write(ctx, "app", [16]byte{}, func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, "notes")
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE "+ident(n.physical, state.physical)+" SET body=repeat('x',33*1024*1024) WHERE id=1")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRows(ctx, "app", "notes", []int64{1}, nil, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("oversized first row: %v", err)
	}
}
