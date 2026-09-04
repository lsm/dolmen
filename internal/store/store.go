package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	if err := validateNS(name); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.nss[name]; ok {
		return n, nil
	}
	path := s.nsPath(name)
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
	`CREATE TABLE IF NOT EXISTS _dolmen_idempotency(
		table_name TEXT NOT NULL,
		key TEXT NOT NULL,
		payload_hash TEXT NOT NULL,
		ids_json TEXT NOT NULL,
		at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		PRIMARY KEY(table_name, key)
	)`,
	// _dolmen_drop_gen counts drops per table name and survives recreation:
	// writers compare it around their embedding pause so a write validated
	// against a dropped table cannot commit into a same-named successor
	// (whose version-1 schema the version compare alone cannot distinguish
	// from the original's). See tableGen and DropTable.
	`CREATE TABLE IF NOT EXISTS _dolmen_drop_gen(
		table_name TEXT PRIMARY KEY,
		gen INTEGER NOT NULL
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
		// Writes take the lock at BEGIN, not at the first write: with a
		// deferred transaction, a writer in another process that commits
		// between our reads and our first write fails with SQLITE_BUSY_SNAPSHOT
		// (not retried by busy_timeout). Immediate locking serializes writers
		// up front, so read-then-write plans — like an idempotent insert's
		// key lookup followed by its row inserts — see a stable snapshot and
		// cross-process retries dedupe instead of erroring.
		q.Add("_txlock", "immediate")
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
	// Do not validate the table name here. Existing tables created before the
	// keyword restriction (or with legacy names) must remain loadable so that
	// describe, insert, search, update, delete, and migrate keep working.
	// CreateTable still enforces ValidateTableName for new tables.
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
	// UseNumber keeps declared numeric defaults exact across the schema_json
	// round-trip: a number field's default above JSON's safe-integer range must
	// survive to later inserts and describe_table, not collapse to the nearest
	// float64.
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&sc); err != nil {
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

// rowsQuerier is the read shape shared by *sql.DB and *sql.Tx so registry
// reads and statement execution can share one snapshot.
type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// registeredTables returns the names recorded in the namespace's table
// registry, so the query guard can allow tables whose names predate current
// reservation rules (e.g. pragma_* or dbstat) while still rejecting the
// internal tables those names collide with.
func registeredTables(ctx context.Context, db rowsQuerier) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM _dolmen_tables`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
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

// Migration is one recorded schema transition from _dolmen_migrations.
type Migration struct {
	ID          int64           `json:"id"`
	FromVersion int             `json:"from_version"`
	ToVersion   int             `json:"to_version"`
	Changes     []schema.Change `json:"changes"`
	At          string          `json:"at"`
}

// ListMigrations returns a table's migration history, newest first, with each
// transition's recorded changes decoded. Creating the table is version 1 and is
// not part of the log, so the newest entry's to_version is the current version.
func (s *Store) ListMigrations(ctx context.Context, nsName, table string) ([]Migration, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return nil, err
	}
	if _, err := loadSchema(ctx, n.ro, nsName, table); err != nil {
		return nil, err
	}
	rows, err := n.ro.QueryContext(ctx,
		`SELECT id, from_version, to_version, changes_json, at FROM _dolmen_migrations WHERE table_name = ? ORDER BY id DESC`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Migration{}
	for rows.Next() {
		var m Migration
		var cj string
		if err := rows.Scan(&m.ID, &m.FromVersion, &m.ToVersion, &cj, &m.At); err != nil {
			return nil, err
		}
		// UseNumber keeps recorded numeric defaults exact: a default above
		// JSON's safe-integer range must survive the audit round-trip, not
		// collapse to the nearest float64.
		dec := json.NewDecoder(strings.NewReader(cj))
		dec.UseNumber()
		if err := dec.Decode(&m.Changes); err != nil {
			return nil, fmt.Errorf("corrupt migration record %d for %s.%s: %w", m.ID, nsName, table, err)
		}
		// Histories written before value became set_*-only still carry an
		// inert "value": false on non-flag changes. Drop it on read so every
		// entry matches the current contract and stays replayable through
		// migrate, which now rejects values on non-flag ops.
		for j := range m.Changes {
			if m.Changes[j].Op != schema.OpSetFulltext && m.Changes[j].Op != schema.OpSetVectorize {
				m.Changes[j].Value = nil
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

const MaxFieldsPerTable = 100

func (s *Store) CreateTable(ctx context.Context, nsName, table string, fields []schema.Field) (*schema.TableSchema, error) {
	if err := schema.ValidateTableName(table); err != nil {
		return nil, invalidf("%s", err)
	}
	if len(fields) > MaxFieldsPerTable {
		return nil, invalidf("too many fields: %d (max %d; SQLite caps tables at 2000 columns including the implicit id, created_at, and _embedding)", len(fields), MaxFieldsPerTable)
	}
	fields = schema.Normalize(fields)
	if err := schema.Validate(fields); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := validateFieldDefaults(fields); err != nil {
		return nil, err
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

// validateFieldDefaults checks that each declared default coerces through its
// field's type at create time, so a mismatched default fails here instead of
// at the first insert that omits the field. The raw declared value is what the
// schema persists; inserts coerce it through the same path on every use.
func validateFieldDefaults(fields []schema.Field) error {
	for _, f := range fields {
		if f.Default == nil {
			continue
		}
		cv, err := coerceValue(f, f.Default)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		// Non-finite floats cannot arrive through JSON but can through direct
		// store use; they have no honest stored meaning — reject them outright.
		if fv, isFloat := cv.(float64); isFloat && (math.IsNaN(fv) || math.IsInf(fv, 0)) {
			return invalidf("field %q: default must be a finite number", f.Name)
		}
		// Same rule as add_field defaults: a NUL byte cannot appear in stored
		// SQL text and would confuse FTS — reject it regardless of how the
		// default was declared.
		if sv, isStr := cv.(string); isStr && strings.ContainsRune(sv, 0) {
			return invalidf("field %q: default must not contain NUL bytes", f.Name)
		}
	}
	return nil
}

func tableDDL(table string, fields []schema.Field) string {
	var sb strings.Builder
	// AUTOINCREMENT keeps ids monotonic and never reused: without it SQLite
	// assigns max(id)+1, so deleting every row lets fresh rows collide with
	// ids agents may still be keying off.
	sb.WriteString(fmt.Sprintf(`CREATE TABLE %s (id INTEGER PRIMARY KEY AUTOINCREMENT, created_at TEXT NOT NULL DEFAULT (strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ','now'))`, q(table)))
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

// createFTS builds the shadow FTS5 index with the porter stemming wrapper over
// unicode61: case and diacritic folding are unchanged, while English suffixes
// collapse to stems so inflected queries ("payments") match indexed forms
// ("payment") without a prefix wildcard. Stemming applies to queries and the
// index alike — phrases and prefix terms operate on stems, and it is
// English-focused (CJK runs pass through untouched; see #106).
func createFTS(ctx context.Context, tx *sql.Tx, table string, fts []schema.Field) error {
	cols := make([]string, len(fts))
	for i, f := range fts {
		cols[i] = q(f.Name)
	}
	ddl := fmt.Sprintf(`CREATE VIRTUAL TABLE %s USING fts5(%s, tokenize='porter unicode61')`, q(ftsTable(table)), strings.Join(cols, ", "))
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
