package postgres

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

var errRollback = errors.New("rollback")

func TestPostgresConstraintsCheckTheTransactionCandidate(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	parents, err := s.CreateTable(ctx, "ns", "parents", []schema.Field{{Name: "slug", Type: schema.String}}, store.TableOpts{}, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	children, err := s.CreateTable(ctx, "ns", "children", []schema.Field{{Name: "project_id", Type: schema.Number}, {Name: "slug", Type: schema.String}, {Name: "serial", Type: schema.Number}, {Name: "active", Type: schema.Boolean}}, store.TableOpts{}, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "parents", []map[string]any{{"slug": "parent"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "children", []map[string]any{{"slug": "first", "project_id": int64(1), "serial": int64(math.MaxInt64), "active": true}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	parents.AcceptReferences = true
	children.Constraints = []schema.Constraint{
		{Name: "parent_ref", Kind: schema.ConstraintForeignKey, Field: "project_id", References: &schema.ConstraintReference{Table: "parents", Field: "id"}},
		{Name: "active_slug", Kind: schema.ConstraintUnique, Fields: []string{"slug", "serial"}, Where: "active = TRUE"},
	}
	for _, sc := range []*schema.TableSchema{parents, children} {
		raw, err := json.Marshal(sc)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, "UPDATE "+s.relation("tables")+" SET schema_json=$1 WHERE namespace='ns' AND name=$2 AND active", string(raw), sc.Name); err != nil {
			t.Fatal(err)
		}
	}
	err = s.write(ctx, "ns", [16]byte{}, func(tx pgx.Tx, n namespace) error {
		rel := func(table string) (string, map[string]string) {
			var physical, columns string
			if err := tx.QueryRow(ctx, "SELECT physical, columns_json FROM "+s.relation("tables")+" WHERE namespace=$1 AND name=$2 AND active", n.name, table).Scan(&physical, &columns); err != nil {
				t.Fatal(err)
			}
			var m map[string]string
			if err := json.Unmarshal([]byte(columns), &m); err != nil {
				t.Fatal(err)
			}
			return ident(n.physical, physical), m
		}
		child, col := rel("children")
		parent, _ := rel("parents")
		exec := func(sql string, args ...any) {
			t.Helper()
			if _, err := tx.Exec(ctx, sql, args...); err != nil {
				t.Fatal(err)
			}
		}
		check := func(table string, incoming bool) error {
			return s.checkConstraints(ctx, tx, n, table, incoming)
		}
		if err := check("children", false); err != nil {
			t.Fatalf("a valid candidate: %v", err)
		}
		cols := strings.Join([]string{ident(col["slug"]), ident(col["serial"]), ident(col["active"])}, ",")
		exec("INSERT INTO "+child+" ("+cols+") VALUES ($1, $2, true)", "first", "9223372036854775807")
		if err := check("children", false); !errors.Is(err, derr.ErrConflict) || !strings.Contains(err.Error(), "active_slug") {
			t.Fatalf("duplicate candidate: %v", err)
		}
		exec("UPDATE "+child+" SET "+ident(col["serial"])+" = $1 WHERE id = 2", "9223372036854775806")
		if err := check("children", false); err != nil {
			t.Fatalf("distinct int64 keys next to the boundary were rounded together: %v", err)
		}
		exec("UPDATE " + child + " SET " + ident(col["active"]) + " = NULL WHERE id = 2")
		exec("UPDATE "+child+" SET "+ident(col["serial"])+" = $1 WHERE id = 2", "9223372036854775807")
		if err := check("children", false); err != nil {
			t.Fatalf("a row whose predicate is NULL must not participate: %v", err)
		}
		exec("UPDATE " + child + " SET " + ident(col["project_id"]) + " = 999 WHERE id = 2")
		if err := check("children", false); !errors.Is(err, derr.ErrConflict) || !strings.Contains(err.Error(), "parent_ref") {
			t.Fatalf("orphan candidate: %v", err)
		}
		exec("UPDATE " + child + " SET " + ident(col["project_id"]) + " = NULL WHERE id = 2")
		exec("DELETE FROM " + parent + " WHERE id = 1")
		if err := check("parents", false); err != nil {
			t.Fatalf("an ordinary parent write must not consult incoming references: %v", err)
		}
		if err := check("parents", true); !errors.Is(err, derr.ErrConflict) || !strings.Contains(err.Error(), "parent_ref") {
			t.Fatalf("a parent delete ignored incoming references: %v", err)
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
}

func TestPostgresConstraintsSkipTablesThatCannotViolateOne(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	plain, err := s.CreateTable(ctx, "ns", "plain", []schema.Field{{Name: "slug", Type: schema.String}}, store.TableOpts{}, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "other", []schema.Field{{Name: "slug", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE "+s.relation("tables")+" SET columns_json='{' WHERE namespace='ns' AND name='other' AND active"); err != nil {
		t.Fatal(err)
	}
	err = s.write(ctx, "ns", [16]byte{}, func(tx pgx.Tx, n namespace) error {
		for _, incoming := range []bool{false, true} {
			if err := s.checkConstraints(ctx, tx, n, "plain", incoming); err != nil {
				t.Fatalf("a table with no constraints and no incoming references must not read the namespace snapshot (incoming=%v): %v", incoming, err)
			}
		}
		plain.AcceptReferences = true
		raw, _ := json.Marshal(plain)
		if _, err := tx.Exec(ctx, "UPDATE "+s.relation("tables")+" SET schema_json=$1 WHERE namespace='ns' AND name='plain' AND active", string(raw)); err != nil {
			t.Fatal(err)
		}
		if err := s.checkConstraints(ctx, tx, n, "plain", true); err == nil {
			t.Fatal("a delete from a table accepting references must read the snapshot")
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
}

func TestPostgresConstraintsSkipWritesThatChangeNoRow(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	notes, err := s.CreateTable(ctx, "ns", "notes", []schema.Field{{Name: "slug", Type: schema.String}}, store.TableOpts{}, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "other", []schema.Field{{Name: "slug", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	notes.Constraints = []schema.Constraint{{Name: "slug_key", Kind: schema.ConstraintUnique, Fields: []string{"slug"}}}
	raw, _ := json.Marshal(notes)
	for _, stmt := range []string{
		"UPDATE " + s.relation("tables") + " SET schema_json=$1 WHERE namespace='ns' AND name='notes' AND active",
		"UPDATE " + s.relation("tables") + " SET columns_json='{' WHERE namespace='ns' AND name='other' AND $1::text IS NOT NULL AND active",
	} {
		if _, err := s.pool.Exec(ctx, stmt, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	err = s.write(ctx, "ns", [16]byte{}, func(tx pgx.Tx, n namespace) error {
		noop := s.constrained(ctx, "notes", true, func(pgx.Tx, namespace) error { return nil })
		if err := noop(tx, n); err != nil {
			t.Fatalf("a write that changed no row must not read the namespace snapshot: %v", err)
		}
		wrote := s.constrained(ctx, "notes", false, func(tx pgx.Tx, n namespace) error {
			_, err := s.reserveChanges(ctx, tx, n, 1)
			return err
		})
		if err := wrote(tx, n); err == nil {
			t.Fatal("a write that changed rows must read the snapshot")
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
}
