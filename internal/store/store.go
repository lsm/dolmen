package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/lsm/dolmen/internal/schema"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

var ErrInvalid = errors.New("invalid request")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

var nsRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type Store struct {
	dir string
	mu  sync.Mutex
	nss map[string]*nsDB
}

type nsDB struct {
	rw *sql.DB
	ro *sql.DB
}

func Open(dir string) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(abs, 0o700); err != nil {
		return nil, fmt.Errorf("cannot secure data directory %s (owner-only permissions): %w", abs, err)
	}
	return &Store{dir: abs, nss: map[string]*nsDB{}}, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	for name, n := range s.nss {
		if err := n.rw.Close(); err != nil && first == nil {
			first = err
		}
		if err := n.ro.Close(); err != nil && first == nil {
			first = err
		}
		delete(s.nss, name)
	}
	return first
}

func (s *Store) ns(name string) (*nsDB, error) {
	if !nsRe.MatchString(name) {
		return nil, invalidf("invalid namespace %q: must match ^[a-z0-9][a-z0-9_-]{0,63}$", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.nss[name]; ok {
		return n, nil
	}
	path := filepath.Join(s.dir, name+".db")
	rw, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	rw.SetMaxOpenConns(1)
	for _, ddl := range registryDDL {
		if _, err := rw.Exec(ddl); err != nil {
			rw.Close()
			return nil, fmt.Errorf("init namespace %s: %w", name, err)
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		rw.Close()
		return nil, fmt.Errorf("cannot secure namespace db %s (owner-only permissions): %w", path, err)
	}
	for _, side := range []string{path + "-wal", path + "-shm"} {
		if _, statErr := os.Stat(side); statErr == nil {
			if err := os.Chmod(side, 0o600); err != nil {
				rw.Close()
				return nil, fmt.Errorf("cannot secure %s (owner-only permissions): %w", side, err)
			}
		}
	}
	ro, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		rw.Close()
		return nil, err
	}
	n := &nsDB{rw: rw, ro: ro}
	s.nss[name] = n
	return n, nil
}

var registryDDL = []string{
	`CREATE TABLE IF NOT EXISTS _dolmen_tables(
		name TEXT PRIMARY KEY,
		version INTEGER NOT NULL,
		schema_json TEXT NOT NULL,
		updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
	)`,
	`CREATE TABLE IF NOT EXISTS _dolmen_migrations(
		id INTEGER PRIMARY KEY,
		table_name TEXT NOT NULL,
		from_version INTEGER NOT NULL,
		to_version INTEGER NOT NULL,
		changes_json TEXT NOT NULL,
		at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
	)`,
}

func dsn(path string, readonly bool) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	if readonly {
		q.Add("mode", "ro")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func q(name string) string {
	return `"` + name + `"`
}

func ftsTable(table string) string {
	return table + "__fts"
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func loadSchema(ctx context.Context, db rowQuerier, nsName, table string) (*schema.TableSchema, error) {
	if !schema.ValidIdent(table) {
		return nil, invalidf("invalid table name %q", table)
	}
	var raw string
	err := db.QueryRowContext(ctx,
		`SELECT schema_json FROM _dolmen_tables WHERE name = ?`, table).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: table %s.%s", ErrNotFound, nsName, table)
	}
	if err != nil {
		return nil, err
	}
	var sc schema.TableSchema
	if err := json.Unmarshal([]byte(raw), &sc); err != nil {
		return nil, fmt.Errorf("corrupt schema for %s.%s: %w", nsName, table, err)
	}
	return &sc, nil
}

func saveSchemaTx(ctx context.Context, tx *sql.Tx, nsName string, sc *schema.TableSchema, fromVersion int, changes any) error {
	raw, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE _dolmen_tables SET version = ?, schema_json = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE name = ?`,
		sc.Version, string(raw), sc.Name); err != nil {
		return err
	}
	cj, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO _dolmen_migrations(table_name, from_version, to_version, changes_json) VALUES(?,?,?,?)`,
		sc.Name, fromVersion, sc.Version, string(cj))
	return err
}

func (s *Store) ListTables(ctx context.Context, nsName string) ([]string, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return nil, err
	}
	rows, err := n.ro.QueryContext(ctx, `SELECT name FROM _dolmen_tables ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func (s *Store) DescribeTable(ctx context.Context, nsName, table string) (*schema.TableSchema, int64, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return nil, 0, err
	}
	sc, err := loadSchema(ctx, n.ro, nsName, table)
	if err != nil {
		return nil, 0, err
	}
	var count int64
	if err := n.ro.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s`, q(table))).Scan(&count); err != nil {
		return nil, 0, err
	}
	return sc, count, nil
}

func (s *Store) CreateTable(ctx context.Context, nsName, table string, fields []schema.Field) (*schema.TableSchema, error) {
	if !schema.ValidTableName(table) {
		return nil, invalidf("invalid table name %q: must match ^[a-z][a-z0-9_]{0,63}$, not be reserved, and not end with __fts (reserved for search indexes)", table)
	}
	fields = schema.Normalize(fields)
	if err := schema.Validate(fields); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	n, err := s.ns(nsName)
	if err != nil {
		return nil, err
	}

	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := loadSchema(ctx, tx, nsName, table); err == nil {
		return nil, invalidf("table %s.%s already exists", nsName, table)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, tableDDL(table, fields)); err != nil {
		return nil, err
	}
	if fts := ftsFields(fields); len(fts) > 0 {
		if err := createFTS(ctx, tx, table, fts); err != nil {
			return nil, err
		}
	}
	sc := &schema.TableSchema{Namespace: nsName, Name: table, Version: 1, Fields: fields}
	raw, _ := json.Marshal(sc)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO _dolmen_tables(name, version, schema_json) VALUES(?,?,?)`,
		table, 1, string(raw)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return sc, nil
}

func tableDDL(table string, fields []schema.Field) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`CREATE TABLE %s (id INTEGER PRIMARY KEY, created_at TEXT NOT NULL DEFAULT (strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ','now'))`, q(table)))
	for _, f := range fields {
		sb.WriteString(fmt.Sprintf(`, %s %s`, q(f.Name), schema.SQLType(f)))
		if f.Required {
			sb.WriteString(` NOT NULL`)
		}
	}
	if vecField := vectorizeField(fields); vecField != nil {
		sb.WriteString(`, "_embedding" BLOB`)
	}
	sb.WriteString(`)`)
	return sb.String()
}

func ftsFields(fields []schema.Field) []schema.Field {
	var out []schema.Field
	for _, f := range fields {
		if f.Fulltext {
			out = append(out, f)
		}
	}
	return out
}

func vectorizeField(fields []schema.Field) *schema.Field {
	for i := range fields {
		if fields[i].Vectorize {
			return &fields[i]
		}
	}
	return nil
}

func createFTS(ctx context.Context, tx *sql.Tx, table string, fts []schema.Field) error {
	cols := make([]string, len(fts))
	for i, f := range fts {
		cols[i] = q(f.Name)
	}
	ddl := fmt.Sprintf(`CREATE VIRTUAL TABLE %s USING fts5(%s)`, q(ftsTable(table)), strings.Join(cols, ", "))
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return err
	}
	return repopulateFTS(ctx, tx, table, fts)
}

func repopulateFTS(ctx context.Context, tx *sql.Tx, table string, fts []schema.Field) error {
	cols := make([]string, len(fts))
	sel := make([]string, len(fts))
	where := make([]string, len(fts))
	for i, f := range fts {
		cols[i] = q(f.Name)
		sel[i] = q(f.Name)
		where[i] = fmt.Sprintf(`%s IS NOT NULL`, q(f.Name))
	}
	stmt := fmt.Sprintf(`INSERT INTO %s(rowid, %s) SELECT id, %s FROM %s WHERE %s`,
		q(ftsTable(table)), strings.Join(cols, ", "), strings.Join(sel, ", "), q(table), strings.Join(where, " OR "))
	_, err := tx.ExecContext(ctx, stmt)
	return err
}

func dropFTS(ctx context.Context, tx *sql.Tx, table string) error {
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, q(ftsTable(table))))
	return err
}
