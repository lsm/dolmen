package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

const (
	rowCountInsertTrigger = "dolmen_count_insert"
	rowCountDeleteTrigger = "dolmen_count_delete"
)

type rowCountKey struct {
	namespace string
	table     string
	gen       int64
}

func sqlLiteral(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

func (s *Store) rowCountCatalogDDL() []string {
	counts := s.relation("row_counts")
	owner := ident(schema.OwnerColumn)
	return []string{
		"CREATE TABLE IF NOT EXISTS " + counts + ` (
 namespace text COLLATE "C" NOT NULL REFERENCES ` + s.relation("namespaces") + `(name) ON DELETE CASCADE,
 table_name text COLLATE "C" NOT NULL, drop_generation bigint NOT NULL,
 scoped boolean NOT NULL, owner text COLLATE "C" NOT NULL DEFAULT '',
 tracks_owners boolean NOT NULL DEFAULT false, n bigint NOT NULL,
 PRIMARY KEY(namespace,table_name,drop_generation,scoped,owner))`,
		"CREATE OR REPLACE FUNCTION " + s.relation("count_rows") + `() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $f$
DECLARE
 sign bigint := CASE WHEN TG_OP = 'INSERT' THEN 1 ELSE -1 END;
 rel text := CASE WHEN TG_OP = 'INSERT' THEN 'dolmen_new_rows' ELSE 'dolmen_old_rows' END;
 delta bigint;
BEGIN
 EXECUTE 'SELECT count(*) FROM ' || rel INTO delta;
 IF delta = 0 THEN RETURN NULL; END IF;
 INSERT INTO ` + counts + ` AS c (namespace,table_name,drop_generation,scoped,owner,tracks_owners,n)
  VALUES (TG_ARGV[1], TG_ARGV[2], TG_ARGV[3]::bigint, false, '', TG_ARGV[0] = 'owner', sign * delta)
  ON CONFLICT (namespace,table_name,drop_generation,scoped,owner) DO UPDATE SET n = c.n + EXCLUDED.n;
 IF TG_ARGV[0] = 'owner' THEN
  EXECUTE 'INSERT INTO ` + counts + ` AS c (namespace,table_name,drop_generation,scoped,owner,tracks_owners,n) SELECT $1, $2, $3, true, ` + owner + `, true, $4 * count(*) FROM ' || rel ||
   ' WHERE ` + owner + ` IS NOT NULL GROUP BY ` + owner + ` ORDER BY ` + owner + ` ON CONFLICT (namespace,table_name,drop_generation,scoped,owner) DO UPDATE SET n = c.n + EXCLUDED.n'
   USING TG_ARGV[1], TG_ARGV[2], TG_ARGV[3]::bigint, sign;
 END IF;
 RETURN NULL;
END
$f$`,
	}
}

func (s *Store) rowCountSteps(relation string, key rowCountKey, hasOwner bool) []migrationStep {
	counts := s.relation("row_counts")
	variant := "plain"
	if hasOwner {
		variant = "owner"
	}
	args := strings.Join([]string{sqlLiteral(variant), sqlLiteral(key.namespace), sqlLiteral(key.table), sqlLiteral(strconv.FormatInt(key.gen, 10))}, ",")
	fn := s.relation("count_rows") + "(" + args + ")"
	steps := []migrationStep{
		{sql: "DROP TRIGGER IF EXISTS " + rowCountInsertTrigger + " ON " + relation},
		{sql: "DROP TRIGGER IF EXISTS " + rowCountDeleteTrigger + " ON " + relation},
		{sql: "CREATE TRIGGER " + rowCountInsertTrigger + " AFTER INSERT ON " + relation + " REFERENCING NEW TABLE AS dolmen_new_rows FOR EACH STATEMENT EXECUTE FUNCTION " + fn},
		{sql: "CREATE TRIGGER " + rowCountDeleteTrigger + " AFTER DELETE ON " + relation + " REFERENCING OLD TABLE AS dolmen_old_rows FOR EACH STATEMENT EXECUTE FUNCTION " + fn},
		{sql: "DELETE FROM " + counts + " WHERE namespace = $1 AND table_name = $2", args: []any{key.namespace, key.table}},
		{sql: "INSERT INTO " + counts + " (namespace,table_name,drop_generation,scoped,owner,tracks_owners,n) SELECT $1, $2, $3, false, '', $4, count(*) FROM " + relation, args: []any{key.namespace, key.table, key.gen, hasOwner}},
	}
	if hasOwner {
		owner := ident(schema.OwnerColumn)
		steps = append(steps, migrationStep{
			sql:  "INSERT INTO " + counts + " (namespace,table_name,drop_generation,scoped,owner,tracks_owners,n) SELECT $1, $2, $3, true, " + owner + ", true, count(*) FROM " + relation + " WHERE " + owner + " IS NOT NULL GROUP BY " + owner,
			args: []any{key.namespace, key.table, key.gen},
		})
	}
	return steps
}

func (s *Store) installRowCount(ctx context.Context, tx pgx.Tx, relation string, key rowCountKey, hasOwner bool) error {
	for _, step := range s.rowCountSteps(relation, key, hasOwner) {
		if _, err := tx.Exec(ctx, step.sql, step.args...); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) backfillRowCounts(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT n.name, t.name, t.drop_generation, n.physical, t.physical, t.schema_json FROM `+s.relation("tables")+` t JOIN `+s.relation("namespaces")+` n ON n.name = t.namespace
 WHERE t.active AND NOT EXISTS (
  SELECT 1 FROM pg_catalog.pg_trigger g JOIN pg_catalog.pg_class c ON c.oid = g.tgrelid JOIN pg_catalog.pg_namespace s ON s.oid = c.relnamespace
  WHERE s.nspname = n.physical AND c.relname = t.physical AND g.tgname = $1)
 ORDER BY n.name, t.name`, rowCountInsertTrigger)
	if err != nil {
		return err
	}
	type pending struct {
		key      rowCountKey
		relation string
		hasOwner bool
	}
	var todo []pending
	for rows.Next() {
		var p pending
		var nsPhysical, tablePhysical, raw string
		if err := rows.Scan(&p.key.namespace, &p.key.table, &p.key.gen, &nsPhysical, &tablePhysical, &raw); err != nil {
			rows.Close()
			return err
		}
		var sc schema.TableSchema
		if err := json.Unmarshal([]byte(raw), &sc); err != nil {
			rows.Close()
			return fmt.Errorf("postgres: corrupt table schema: %w", err)
		}
		p.relation = ident(nsPhysical, tablePhysical)
		p.hasOwner = sc.HasOwner
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range todo {
		if err := s.installRowCount(ctx, tx, p.relation, p.key, p.hasOwner); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) readRowCount(ctx context.Context, tx pgx.Tx, key rowCountKey, scope *store.RowScope) (int64, bool, error) {
	counts := s.relation("row_counts")
	var total int64
	var tracksOwners bool
	err := tx.QueryRow(ctx, "SELECT n, tracks_owners FROM "+counts+" WHERE namespace = $1 AND table_name = $2 AND drop_generation = $3 AND NOT scoped AND owner = ''", key.namespace, key.table, key.gen).Scan(&total, &tracksOwners)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if scope == nil {
		return total, true, nil
	}
	if !tracksOwners {
		return 0, false, nil
	}
	var n int64
	err = tx.QueryRow(ctx, "SELECT n FROM "+counts+" WHERE namespace = $1 AND table_name = $2 AND drop_generation = $3 AND scoped AND owner = $4", key.namespace, key.table, key.gen, scope.Owner).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, true, nil
	}
	return n, err == nil, err
}
