package store

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

func TestSQLiteConstraintsReadTheTransactionCandidate(t *testing.T) {
	s := openRowAccessStore(t)
	ctx := t.Context()
	parents, err := s.CreateTable(ctx, "ns", "parents", []schema.Field{{Name: "slug", Type: schema.String}}, TableOpts{}, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	children, err := s.CreateTable(ctx, "ns", "children", []schema.Field{{Name: "project_id", Type: schema.Number}, {Name: "slug", Type: schema.String}, {Name: "serial", Type: schema.Number}, {Name: "active", Type: schema.Boolean}}, TableOpts{}, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "parents", []map[string]any{{"slug": "parent"}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "children", []map[string]any{{"slug": "first", "project_id": int64(1), "serial": int64(math.MaxInt64), "active": true}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	parents.AcceptReferences = true
	children.Constraints = []schema.Constraint{
		{Name: "parent_ref", Kind: schema.ConstraintForeignKey, Field: "project_id", References: &schema.ConstraintReference{Table: "parents", Field: "id"}},
		{Name: "active_slug", Kind: schema.ConstraintUnique, Fields: []string{"slug", "serial"}, Where: "active = TRUE"},
	}
	n, err := s.ns("ns")
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()
	for _, sc := range []*schema.TableSchema{parents, children} {
		raw, err := json.Marshal(sc)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := n.rw.ExecContext(ctx, `UPDATE _dolmen_tables SET schema_json=? WHERE name=?`, string(raw), sc.Name); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := checkSQLiteConstraints(ctx, tx, children, false); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO children(slug,serial,active) VALUES(?,?,1)`, "first", int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	if err := checkSQLiteConstraints(ctx, tx, children, false); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("duplicate candidate: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE children SET serial=? WHERE id=2`, float64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	if err := checkSQLiteConstraints(ctx, tx, children, false); err != nil {
		t.Fatalf("distinct int64/float values were rounded together: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE children SET project_id=999 WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	if err := checkSQLiteConstraints(ctx, tx, children, false); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("orphan candidate: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE children SET project_id=NULL WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM parents WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := checkSQLiteConstraints(ctx, tx, parents, true); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("incoming references ignored: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := n.rw.QueryRowContext(ctx, `SELECT count(*) FROM children`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("candidate leaked after rollback: %d, %v", count, err)
	}
}

func TestSQLiteConstraintsSkipTablesThatCannotViolateOne(t *testing.T) {
	s := openRowAccessStore(t)
	ctx := t.Context()
	plain, err := s.CreateTable(ctx, "ns", "plain", []schema.Field{{Name: "slug", Type: schema.String}}, TableOpts{}, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.ns("ns")
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO _dolmen_tables(name, version, schema_json) VALUES('broken', 1, '{')`); err != nil {
		t.Fatal(err)
	}
	for _, incoming := range []bool{false, true} {
		if err := checkSQLiteConstraints(ctx, tx, plain, incoming); err != nil {
			t.Fatalf("a table with no constraints and no incoming references must not read the namespace snapshot (incoming=%v): %v", incoming, err)
		}
	}
	plain.AcceptReferences = true
	if err := checkSQLiteConstraints(ctx, tx, plain, true); err == nil {
		t.Fatal("a delete from a table accepting references must read the snapshot")
	}
}
