package postgres

import (
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestPostgresIdempotencyCatalogRetiresTheOwnerlessRelation(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "body", Type: schema.Text}}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	record := []map[string]any{{"body": "first"}}
	first, err := s.Insert(ctx, "app", "notes", record, store.WriteOpts{IdempotencyKey: "k"}, store.Embedder{}, nil, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}

	for _, stmt := range []string{
		"CREATE TABLE " + s.relation("idempotency") + ` (
 namespace text NOT NULL REFERENCES ` + s.relation("namespaces") + `(name) ON DELETE CASCADE,
 table_name text NOT NULL, drop_generation bigint NOT NULL, key text NOT NULL,
 payload_hash text NOT NULL, result_json text NOT NULL,
 PRIMARY KEY(namespace,table_name,drop_generation,key))`,
		"INSERT INTO " + s.relation("idempotency") +
			" (namespace,table_name,drop_generation,key,payload_hash,result_json)" +
			" SELECT namespace,table_name,drop_generation,key,payload_hash,result_json FROM " +
			s.relation("idempotency_owned"),
		"DROP TABLE " + s.relation("idempotency_owned"),
		"UPDATE " + s.relation("version") + " SET version = 5",
	} {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("shape the catalog as version 5 (%s): %v", stmt, err)
		}
	}

	if err := s.bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap over a version 5 idempotency table: %v", err)
	}

	var version int
	if err := s.pool.QueryRow(ctx, "SELECT version FROM "+s.relation("version")).Scan(&version); err != nil || version != catalogVersion {
		t.Fatalf("catalog version %d after bootstrap, want %d: %v", version, catalogVersion, err)
	}
	var legacy *string
	if err := s.pool.QueryRow(ctx, "SELECT to_regclass($1)::text", s.relation("idempotency")).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy != nil {
		t.Fatalf("the ownerless relation %s outlived the migration, so a process still holding it reads a key across every owner", *legacy)
	}
	var owner string
	if err := s.pool.QueryRow(ctx, "SELECT owner FROM "+s.relation("idempotency_owned")+" WHERE key='k'").Scan(&owner); err != nil {
		t.Fatalf("the record written before owners existed: %v", err)
	}
	if owner != store.LegacyIdempotencyOwner {
		t.Fatalf("a record written before owners existed landed in domain %q, want the legacy one", owner)
	}

	replay, err := s.Insert(ctx, "app", "notes", record, store.WriteOpts{IdempotencyKey: "k"}, store.Embedder{}, nil, store.Incarnation{})
	if err != nil || !replay.Replayed {
		t.Fatalf("an unauthenticated writer must still replay its own legacy record: %+v %v", replay, err)
	}

	scoped, err := s.Insert(ctx, "app", "notes", record, store.WriteOpts{IdempotencyKey: "k", Owner: "alice"}, store.Embedder{}, nil, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if scoped.Replayed {
		t.Fatal("alice replayed a record she did not write; the legacy domain is not hers to read without table-wide read")
	}
	if len(scoped.Ids) != 1 || scoped.Ids[0] == first.Ids[0] {
		t.Fatalf("alice's insert should have written a new row, got %+v", scoped)
	}

	wide, err := s.Insert(ctx, "app", "notes", record, store.WriteOpts{IdempotencyKey: "k", Owner: "carol", TableWideRead: true}, store.Embedder{}, nil, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if !wide.Replayed {
		t.Fatalf("a table-wide reader must replay the legacy record rather than duplicate it: %+v", wide)
	}
}
