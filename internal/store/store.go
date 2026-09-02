package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/lsm/dolmen/internal/schema"

	_ "modernc.org/sqlite"
)

type EmbedFn func(ctx context.Context, texts []string) ([][]float32, error)

type Embedder struct {
	Embed    EmbedFn
	Identity string
}

var ErrNotFound = errors.New("not found")

var ErrInvalid = errors.New("invalid request")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

var nsRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

const MaxRecordsPerInsert = 1000

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

func normalizeArg(v any) any {
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return i
		}
		f, _ := n.Float64()
		return f
	}
	return v
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
	rows, err := n.rw.QueryContext(ctx, `SELECT name FROM _dolmen_tables ORDER BY name`)
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
	sc, err := loadSchema(ctx, n.rw, nsName, table)
	if err != nil {
		return nil, 0, err
	}
	var count int64
	if err := n.rw.QueryRowContext(ctx,
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

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`CREATE TABLE %s (id INTEGER PRIMARY KEY, created_at TEXT NOT NULL DEFAULT (strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ','now'))`, q(table)))
	for _, f := range fields {
		sb.WriteString(fmt.Sprintf(`, %s %s`, q(f.Name), schema.SQLType(f)))
	}
	if vecField := vectorizeField(fields); vecField != nil {
		sb.WriteString(`, "_embedding" BLOB`)
	}
	sb.WriteString(`)`)

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
	if _, err := tx.ExecContext(ctx, sb.String()); err != nil {
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

func (s *Store) Insert(ctx context.Context, nsName, table string, records []map[string]any, emb Embedder) ([]int64, error) {
	if len(records) == 0 {
		return nil, invalidf("no records given")
	}
	if len(records) > MaxRecordsPerInsert {
		return nil, invalidf("too many records: %d > %d per call", len(records), MaxRecordsPerInsert)
	}
	n, err := s.ns(nsName)
	if err != nil {
		return nil, err
	}
	normalized := make([]map[string]any, len(records))
	for i, rec := range records {
		nr := make(map[string]any, len(rec))
		for k, v := range rec {
			lk := strings.ToLower(k)
			if _, exists := nr[lk]; exists {
				return nil, invalidf("record %d: fields %q and its case variant collapse to %q; use one spelling", i, k, lk)
			}
			nr[lk] = v
		}
		normalized[i] = nr
	}
	records = normalized

	for attempt := 0; ; attempt++ {
		if attempt >= 3 {
			return nil, invalidf("table schema changed concurrently; retry the insert")
		}
		ids, done, err := s.insertAttempt(ctx, n, nsName, table, records, emb)
		if done {
			return ids, err
		}
	}
}

func (s *Store) insertAttempt(ctx context.Context, n *nsDB, nsName, table string, records []map[string]any, emb Embedder) (ids []int64, done bool, err error) {
	sc, err := loadSchema(ctx, n.rw, nsName, table)
	if err != nil {
		return nil, true, err
	}
	persistMeta := sc.EmbedSpace == "" || sc.EmbedDim == 0
	origEmbedSpace := sc.EmbedSpace
	origEmbedDim := sc.EmbedDim
	for _, rec := range records {
		for k := range rec {
			if sc.Field(k) == nil {
				return nil, true, invalidf("unknown field %q on table %s (see describe_table)", k, table)
			}
		}
		for _, f := range sc.Fields {
			v, present := rec[f.Name]
			if present && v != nil {
				continue
			}
			if f.Required && (!present || v == nil) {
				return nil, true, invalidf("field %q is required", f.Name)
			}
		}
	}

	type coercedRow struct {
		cols []string
		vals []any
		rec  map[string]any
	}
	coercedRows := make([]coercedRow, len(records))
	for i, rec := range records {
		var cols []string
		var vals []any
		for _, f := range sc.Fields {
			v, present := rec[f.Name]
			if !present {
				continue
			}
			cv, err := coerceValue(f, v)
			if err != nil {
				return nil, true, fmt.Errorf("%w: %w", ErrInvalid, err)
			}
			cols = append(cols, q(f.Name))
			vals = append(vals, cv)
		}
		coercedRows[i] = coercedRow{cols: cols, vals: vals, rec: rec}
	}

	embFor := map[int][]float32{}
	if vf := sc.VectorizeField(); vf != nil {
		var texts []string
		var idx []int
		for i, rec := range records {
			if t, ok := rec[vf.Name].(string); ok && t != "" {
				texts = append(texts, t)
				idx = append(idx, i)
			}
		}
		if len(texts) > 0 {
			if emb.Embed == nil {
				return nil, true, invalidf("table %s uses vectorize but no embedding provider is configured", table)
			}
			if sc.EmbedSpace != "" && emb.Identity != "" && sc.EmbedSpace != emb.Identity {
				return nil, true, invalidf("embedding provider changed: table rows were embedded by %q but the active provider is %q; re-embed via migrate (set_vectorize off, then on)", sc.EmbedSpace, emb.Identity)
			}
			vecs, err := emb.Embed(ctx, texts)
			if err != nil {
				return nil, true, fmt.Errorf("embedding failed: %w", err)
			}
			if len(vecs) != len(texts) {
				return nil, true, fmt.Errorf("embedding provider returned %d vectors for %d texts", len(vecs), len(texts))
			}
			for _, v := range vecs {
				if sc.EmbedDim == 0 {
					sc.EmbedDim = len(v)
				} else if len(v) != sc.EmbedDim {
					return nil, true, invalidf("embedding provider returned %d-dimensional vectors but table %s stores %d-dimensional embeddings; re-embed via migrate (set_vectorize off, then on) if the provider changed", len(v), table, sc.EmbedDim)
				}
			}
			for k, i := range idx {
				embFor[i] = vecs[k]
			}
		}
	}

	fts := sc.FTSFields()
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, true, err
	}
	defer tx.Rollback()

	scTx, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return nil, true, err
	}
	if scTx.Version != sc.Version || scTx.EmbedSpace != origEmbedSpace || scTx.EmbedDim != origEmbedDim {
		return nil, false, nil
	}

	ids = make([]int64, 0, len(records))
	for i, row := range coercedRows {
		rec := row.rec
		cols := row.cols
		vals := row.vals
		if ev, ok := embFor[i]; ok {
			cols = append(cols, `"_embedding"`)
			vals = append(vals, schema.EncodeVector(ev))
		}
		var stmt string
		if len(cols) == 0 {
			stmt = fmt.Sprintf(`INSERT INTO %s DEFAULT VALUES`, q(table))
		} else {
			ph := strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")
			stmt = fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, q(table), strings.Join(cols, ", "), ph)
		}
		res, err := tx.ExecContext(ctx, stmt, vals...)
		if err != nil {
			return nil, true, fmt.Errorf("insert into %s: %w", table, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, true, err
		}
		ids = append(ids, id)

		if len(fts) > 0 {
			fcols := make([]string, len(fts))
			fvals := make([]any, len(fts)+1)
			fvals[0] = id
			for j, f := range fts {
				fcols[j] = q(f.Name)
				fvals[j+1] = ftsText(rec[f.Name])
			}
			fph := strings.TrimSuffix(strings.Repeat("?,", len(fts)+1), ",")
			fstmt := fmt.Sprintf(`INSERT INTO %s (rowid, %s) VALUES (%s)`,
				q(ftsTable(table)), strings.Join(fcols, ", "), fph)
			if _, err := tx.ExecContext(ctx, fstmt, fvals...); err != nil {
				return nil, true, fmt.Errorf("update search index for %s: %w", table, err)
			}
		}
	}
	if len(embFor) > 0 && persistMeta {
		if sc.EmbedSpace == "" && emb.Identity != "" {
			sc.EmbedSpace = emb.Identity
		}
		if sc.EmbedDim == 0 {
			for _, v := range embFor {
				sc.EmbedDim = len(v)
				break
			}
		}
		raw, err := json.Marshal(sc)
		if err != nil {
			return nil, true, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE _dolmen_tables SET schema_json = ? WHERE name = ?`, string(raw), table); err != nil {
			return nil, true, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, true, err
	}
	return ids, true, nil
}

func ftsText(v any) any {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func coerceValue(f schema.Field, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch f.Type {
	case schema.Number:
		switch n := v.(type) {
		case float64:
			return n, nil
		case int:
			return int64(n), nil
		case int64:
			return n, nil
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return i, nil
			}
			fErrName := f.Name
			f, err := n.Float64()
			if err != nil {
				return nil, fmt.Errorf("field %q: expected a number", fErrName)
			}
			return f, nil
		default:
			return nil, fmt.Errorf("field %q: expected a number", f.Name)
		}
	case schema.Boolean:
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("field %q: expected a boolean", f.Name)
		}
		if b {
			return int64(1), nil
		}
		return int64(0), nil
	case schema.Vector:
		var floats []float64
		switch arr := v.(type) {
		case []any:
			floats = make([]float64, len(arr))
			for i, x := range arr {
				switch n := x.(type) {
				case float64:
					floats[i] = n
				case int:
					floats[i] = float64(n)
				case int64:
					floats[i] = float64(n)
				case json.Number:
					fv, err := n.Float64()
					if err != nil {
						return nil, fmt.Errorf("field %q: vector entries must be numbers", f.Name)
					}
					floats[i] = fv
				default:
					return nil, fmt.Errorf("field %q: vector entries must be numbers", f.Name)
				}
			}
		case []float64:
			floats = arr
		case []float32:
			floats = make([]float64, len(arr))
			for i, x := range arr {
				floats[i] = float64(x)
			}
		default:
			return nil, fmt.Errorf("field %q: expected an array of numbers", f.Name)
		}
		if len(floats) != f.Dim {
			return nil, fmt.Errorf("field %q: vector has %d entries, expected dim %d", f.Name, len(floats), f.Dim)
		}
		out := make([]float32, len(floats))
		for i, x := range floats {
			if math.IsNaN(x) || math.Abs(x) > math.MaxFloat32 {
				return nil, fmt.Errorf("field %q: vector entry %d is outside the float32 range", f.Name, i)
			}
			out[i] = float32(x)
		}
		return schema.EncodeVector(out), nil
	case schema.JSON:
		switch j := v.(type) {
		case string:
			if json.Valid([]byte(j)) {
				return j, nil
			}
			b, err := json.Marshal(j)
			if err != nil {
				return nil, fmt.Errorf("field %q: cannot marshal JSON: %w", f.Name, err)
			}
			return string(b), nil
		case map[string]any, []any, bool, float64, int, int64, json.Number:
			b, err := json.Marshal(j)
			if err != nil {
				return nil, fmt.Errorf("field %q: cannot marshal JSON: %w", f.Name, err)
			}
			return string(b), nil
		default:
			return nil, fmt.Errorf("field %q: expected a JSON value", f.Name)
		}
	default:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("field %q: expected a string", f.Name)
		}
		return s, nil
	}
}

var queryStartRe = regexp.MustCompile(`(?i)\A\s*(select|with)\b`)

func (s *Store) Query(ctx context.Context, nsName, query string, args []any) ([]map[string]any, bool, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(query), ";")
	if !queryStartRe.MatchString(strings.TrimSpace(query)) {
		return nil, false, invalidf("only read-only SELECT/WITH statements are allowed")
	}
	if strings.Contains(trimmed, ";") {
		return nil, false, invalidf("multiple statements are not allowed")
	}
	if len(args) > 100 {
		return nil, false, invalidf("too many query parameters")
	}
	for i, a := range args {
		args[i] = normalizeArg(a)
	}
	n, err := s.ns(nsName)
	if err != nil {
		return nil, false, err
	}
	rows, err := n.ro.QueryContext(ctx, trimmed, args...)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, false, fmt.Errorf("%w: %w", ErrNotFound, err)
		}
		return nil, false, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	defer rows.Close()
	return rowsToMaps(rows)
}

const MaxQueryRows = 1000

const MaxQueryBytes = 32 << 20

func rowsToMaps(rows *sql.Rows) ([]map[string]any, bool, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, false, err
	}
	seen := map[string]bool{}
	labelBytes := 0
	for _, c := range cols {
		if seen[c] {
			return nil, false, invalidf("duplicate column label %q in query result; use AS aliases", c)
		}
		if len(c) > 4096 {
			return nil, false, invalidf("column label exceeds 4096 bytes; use a shorter AS alias")
		}
		seen[c] = true
		labelBytes += len(c) + 16
	}
	out := []map[string]any{}
	total := 0
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, false, err
		}
		m := make(map[string]any, len(cols))
		rowBytes := 0
		for i, c := range cols {
			if err := checkRowValue(c, vals[i]); err != nil {
				return nil, false, err
			}
			v := normalizeVal(vals[i])
			m[c] = v
			rowBytes += approxSize(v)
		}
		if len(out) >= MaxQueryRows {
			return out, true, wrapStepErr(rows.Err())
		}
		if total+rowBytes+labelBytes > MaxQueryBytes {
			if len(out) == 0 {
				return nil, false, invalidf("query result exceeds the %d MiB response budget on its first row; select fewer or smaller columns", MaxQueryBytes>>20)
			}
			return out, true, wrapStepErr(rows.Err())
		}
		total += rowBytes + labelBytes
		out = append(out, m)
	}
	return out, false, wrapStepErr(rows.Err())
}

func wrapStepErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrInvalid, err)
}

func checkRowValue(col string, v any) error {
	switch t := v.(type) {
	case []byte:
		if len(t) > MaxQueryBytes {
			return invalidf("column %q exceeds the %d MiB response budget; select fewer or smaller columns", col, MaxQueryBytes>>20)
		}
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			return invalidf("column %q produced a non-finite value", col)
		}
	}
	return nil
}

func approxSize(v any) int {
	switch t := v.(type) {
	case string:
		return encodedSize(t)
	case []byte:
		return len(t)
	default:
		return 16
	}
}

func encodedSize(s string) int {
	n := len(s)
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == '"' || s[i] == '\\' {
			n += 5
		}
	}
	if strings.Contains(s, "\u2028") || strings.Contains(s, "\u2029") {
		n += 3 * (strings.Count(s, "\u2028") + strings.Count(s, "\u2029"))
	}
	if !utf8.ValidString(s) {
		for _, r := range s {
			if r == utf8.RuneError {
				n += 5
			}
		}
	}
	return n
}

func normalizeVal(v any) any {
	if b, ok := v.([]byte); ok {
		return base64.StdEncoding.EncodeToString(b)
	}
	return v
}

func (s *Store) SearchFulltext(ctx context.Context, nsName, table, query string, limit int) ([]map[string]any, bool, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return nil, false, err
	}
	sc, err := loadSchema(ctx, n.rw, nsName, table)
	if err != nil {
		return nil, false, err
	}
	if len(sc.FTSFields()) == 0 {
		return nil, false, invalidf("table %s has no fulltext fields", table)
	}
	rows, err := n.rw.QueryContext(ctx,
		fmt.Sprintf(`SELECT rowid FROM %s WHERE %s MATCH ? ORDER BY rank LIMIT ?`,
			q(ftsTable(table)), ftsTable(table)),
		query, limit)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, false, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	out, complete, err := fetchByIDs(ctx, n.rw, table, ids)
	if err != nil {
		return nil, false, err
	}
	return out, !complete, nil
}

func fetchByIDs(ctx context.Context, db *sql.DB, table string, ids []int64) ([]map[string]any, bool, error) {
	if len(ids) == 0 {
		return []map[string]any{}, true, nil
	}
	values := make([]string, len(ids))
	args := make([]any, 0, len(ids)*2)
	for i, id := range ids {
		values[i] = "(?, ?)"
		args = append(args, i, id)
	}
	rows, err := db.QueryContext(ctx,
		fmt.Sprintf(`WITH _ranked(pos, id) AS (VALUES %s) SELECT t.* FROM _ranked JOIN %s t ON t.id = _ranked.id ORDER BY _ranked.pos`,
			strings.Join(values, ", "), q(table)), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, false, err
	}
	byID := map[int64]map[string]any{}
	total := 0
	complete := true
scan:
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, false, err
		}
		m := make(map[string]any, len(cols))
		var id int64
		rowBytes := 0
		for i, c := range cols {
			if err := checkRowValue(c, vals[i]); err != nil {
				return nil, false, err
			}
			if c == "id" {
				if v, ok := vals[i].(int64); ok {
					id = v
				}
			}
			v := normalizeVal(vals[i])
			m[c] = v
			rowBytes += approxSize(v)
		}
		if total+rowBytes > MaxQueryBytes {
			if len(byID) == 0 {
				return nil, false, invalidf("search result exceeds the %d MiB response budget on its first row", MaxQueryBytes>>20)
			}
			complete = false
			break scan
		}
		total += rowBytes
		byID[id] = m
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			out = append(out, m)
		}
	}
	return out, complete, nil
}

func (s *Store) SearchVector(ctx context.Context, nsName, table, column string, vec []float32, embedModel string, limit int) ([]map[string]any, bool, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return nil, false, err
	}
	sc, err := loadSchema(ctx, n.rw, nsName, table)
	if err != nil {
		return nil, false, err
	}
	column, dim, err := resolveVectorColumn(sc, table, column, embedModel)
	if err != nil {
		return nil, false, err
	}
	if dim > 0 && len(vec) != dim {
		return nil, false, invalidf("query vector has %d entries, column %s expects dim %d", len(vec), column, dim)
	}

	rows, err := n.rw.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, %s FROM %s WHERE %s IS NOT NULL`, q(column), q(table), q(column)))
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	type hit struct {
		id    int64
		score float64
	}
	var hits []hit
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, false, err
		}
		stored, err := schema.DecodeVector(blob)
		if err != nil || len(stored) != len(vec) {
			continue
		}
		hits = append(hits, hit{id: id, score: cosine(vec, stored)})
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score == hits[j].score {
			return hits[i].id < hits[j].id
		}
		return hits[i].score > hits[j].score
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	ids := make([]int64, len(hits))
	scoreByID := make(map[int64]float64, len(hits))
	for i, h := range hits {
		ids[i] = h.id
		scoreByID[h.id] = h.score
	}
	out, complete, err := fetchByIDs(ctx, n.rw, table, ids)
	if err != nil {
		return nil, false, err
	}
	for _, row := range out {
		if id, ok := row["id"].(int64); ok {
			row["_score"] = scoreByID[id]
		}
		if str, ok := row[column].(string); ok {
			if raw, err := base64.StdEncoding.DecodeString(str); err == nil {
				if fv, err := schema.DecodeVector(raw); err == nil {
					floats := make([]float64, len(fv))
					for j, x := range fv {
						floats[j] = float64(x)
					}
					row[column] = floats
				}
			}
		}
	}
	return out, !complete, nil
}

func resolveVectorColumn(sc *schema.TableSchema, table, column, embedModel string) (string, int, error) {
	if column == "" {
		if vf := sc.VectorizeField(); vf != nil {
			column = "_embedding"
		} else if vfs := sc.VectorFields(); len(vfs) > 0 {
			column = vfs[0].Name
		} else {
			return "", 0, invalidf("table %s has no vector data (no vectorize field, no vector fields)", table)
		}
	}
	var dim int
	switch {
	case column == "_embedding":
		if sc.VectorizeField() == nil {
			return "", 0, invalidf("table %s has no vectorize field for _embedding", table)
		}
	case sc.Field(column) != nil && sc.Field(column).Type == schema.Vector:
		dim = sc.Field(column).Dim
	default:
		return "", 0, invalidf("column %q is not a vector column", column)
	}
	if column == "_embedding" {
		if sc.EmbedSpace != "" && embedModel != "" && sc.EmbedSpace != embedModel {
			return "", 0, invalidf("embedding model changed: table rows are embedded with %q but the provider now serves %q; re-embed via migrate (set_vectorize off, then on)", sc.EmbedSpace, embedModel)
		}
		dim = sc.EmbedDim
	}
	return column, dim, nil
}

func (s *Store) ValidateVectorSearch(ctx context.Context, nsName, table, column, embedIdentity string) error {
	n, err := s.ns(nsName)
	if err != nil {
		return err
	}
	sc, err := loadSchema(ctx, n.rw, nsName, table)
	if err != nil {
		return err
	}
	_, _, err = resolveVectorColumn(sc, table, column, embedIdentity)
	return err
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func (s *Store) Delete(ctx context.Context, nsName, table, where string, args []any) (int64, error) {
	where = strings.TrimSpace(where)
	if where == "" {
		return 0, invalidf("filter is required (pass \"1=1\" to delete everything)")
	}
	if strings.Contains(where, ";") {
		return 0, invalidf("multiple statements are not allowed in filter")
	}
	for i, a := range args {
		args[i] = normalizeArg(a)
	}
	n, err := s.ns(nsName)
	if err != nil {
		return 0, err
	}
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	sc, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp._dolmen_delete_ids`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`CREATE TEMP TABLE _dolmen_delete_ids AS SELECT id FROM %s WHERE %s`, q(table), where), args...); err != nil {
		return 0, fmt.Errorf("%w: filter error: %w", ErrInvalid, err)
	}
	if len(sc.FTSFields()) > 0 {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE rowid IN (SELECT id FROM _dolmen_delete_ids)`, q(ftsTable(table)))); err != nil {
			return 0, err
		}
	}
	res, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE id IN (SELECT id FROM _dolmen_delete_ids)`, q(table)))
	if err != nil {
		return 0, err
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE _dolmen_delete_ids`); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

func (s *Store) Migrate(ctx context.Context, nsName, table string, changes []schema.Change, emb Embedder) (*schema.TableSchema, error) {
	if len(changes) == 0 {
		return nil, invalidf("no changes given")
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

	old, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return nil, err
	}

	fields := make([]schema.Field, len(old.Fields))
	copy(fields, old.Fields)
	cur := &schema.TableSchema{Namespace: nsName, Name: table, Version: old.Version, Fields: fields, EmbedSpace: old.EmbedSpace, EmbedDim: old.EmbedDim}

	type step func(ctx context.Context, tx *sql.Tx) error
	var steps []step
	var rebuildFTSNeeded, vectorizeChanged bool
	var droppedFTSChange bool

	findField := func(name string) (*schema.Field, error) {
		for i := range cur.Fields {
			if cur.Fields[i].Name == name {
				return &cur.Fields[i], nil
			}
		}
		return nil, invalidf("field %q not found", name)
	}

	for _, ch := range changes {
		switch ch.Op {
		case schema.OpAddField:
			if ch.Field == nil {
				return nil, invalidf("add_field needs a field object")
			}
			f := schema.Normalize([]schema.Field{*ch.Field})[0]
			for _, ef := range cur.Fields {
				if ef.Name == f.Name {
					return nil, invalidf("field %q already exists", f.Name)
				}
			}
			cur.Fields = append(cur.Fields, f)
			if f.Fulltext {
				rebuildFTSNeeded = true
			}
			if f.Vectorize {
				vectorizeChanged = true
			}
			steps = append(steps, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, q(table), q(f.Name), schema.SQLType(f)))
				return err
			})
		case schema.OpRenameField:
			if !schema.ValidIdent(ch.To) {
				return nil, invalidf("invalid new field name %q", ch.To)
			}
			f, err := findField(ch.From)
			if err != nil {
				return nil, err
			}
			oldName := f.Name
			if ch.To != ch.From {
				for _, ef := range cur.Fields {
					if ef.Name == ch.To {
						return nil, invalidf("field %q already exists", ch.To)
					}
				}
			}
			f.Name = ch.To
			if f.Fulltext {
				rebuildFTSNeeded = true
			}
			steps = append(steps, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					fmt.Sprintf(`ALTER TABLE %s RENAME COLUMN %s TO %s`, q(table), q(oldName), q(ch.To)))
				return err
			})
		case schema.OpDropField:
			f, err := findField(ch.Name)
			if err != nil {
				return nil, err
			}
			if f.Vectorize {
				vectorizeChanged = true
			}
			if f.Fulltext {
				rebuildFTSNeeded = true
				droppedFTSChange = true
			}
			idx := -1
			for i := range cur.Fields {
				if cur.Fields[i].Name == ch.Name {
					idx = i
					break
				}
			}
			cur.Fields = append(cur.Fields[:idx], cur.Fields[idx+1:]...)
			steps = append(steps, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					fmt.Sprintf(`ALTER TABLE %s DROP COLUMN %s`, q(table), q(ch.Name)))
				return err
			})
		case schema.OpSetFulltext:
			f, err := findField(ch.Name)
			if err != nil {
				return nil, err
			}
			if ch.Value && f.Type != schema.String && f.Type != schema.Text {
				return nil, invalidf("field %q: fulltext is only allowed on string or text fields", f.Name)
			}
			if f.Fulltext != ch.Value {
				f.Fulltext = ch.Value
				rebuildFTSNeeded = true
			}
		case schema.OpSetVectorize:
			f, err := findField(ch.Name)
			if err != nil {
				return nil, err
			}
			if ch.Value {
				if f.Type != schema.String && f.Type != schema.Text {
					return nil, invalidf("field %q: vectorize is only allowed on string or text fields", f.Name)
				}
				for i := range cur.Fields {
					if cur.Fields[i].Vectorize && &cur.Fields[i] != f {
						return nil, invalidf("at most one vectorized field per table (already %q)", cur.Fields[i].Name)
					}
				}
			}
			if f.Vectorize != ch.Value {
				f.Vectorize = ch.Value
				vectorizeChanged = true
			}
		default:
			return nil, invalidf("unknown migration op %q (valid: add_field, rename_field, drop_field, set_fulltext, set_vectorize)", ch.Op)
		}
	}
	if err := schema.Validate(cur.Fields); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if len(ftsFields(cur.Fields)) == 0 && len(ftsFields(old.Fields)) > 0 {
		rebuildFTSNeeded = true
		droppedFTSChange = true
	}
	_ = droppedFTSChange

	for _, st := range steps {
		if err := st(ctx, tx); err != nil {
			return nil, fmt.Errorf("migration step failed: %w", err)
		}
	}

	if rebuildFTSNeeded {
		if err := dropFTS(ctx, tx, table); err != nil {
			return nil, err
		}
		if fts := ftsFields(cur.Fields); len(fts) > 0 {
			if err := createFTS(ctx, tx, table, fts); err != nil {
				return nil, err
			}
		}
	}

	if vectorizeChanged {
		newVec := vectorizeField(cur.Fields)
		if newVec != nil {
			modelChanged := cur.EmbedSpace != "" && emb.Identity != "" && cur.EmbedSpace != emb.Identity
			if old.VectorizeField() != nil || modelChanged {
				if _, err := tx.ExecContext(ctx,
					fmt.Sprintf(`UPDATE %s SET "_embedding" = NULL`, q(table))); err != nil {
					return nil, err
				}
			}
			cur.EmbedDim = 0
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf(`ALTER TABLE %s ADD COLUMN "_embedding" BLOB`, q(table))); err != nil {
				if !strings.Contains(err.Error(), "duplicate column") {
					return nil, fmt.Errorf("add _embedding column: %w", err)
				}
			}
			if emb.Embed == nil {
				return nil, invalidf("vectorize requires an embedding provider (set DOLMEN_EMBED_PROVIDER)")
			}
			for {
				rows, err := tx.QueryContext(ctx,
					fmt.Sprintf(`SELECT id, %s FROM %s WHERE "_embedding" IS NULL AND %s IS NOT NULL AND %s != '' ORDER BY id LIMIT 128`,
						q(newVec.Name), q(table), q(newVec.Name), q(newVec.Name)))
				if err != nil {
					return nil, err
				}
				type pending struct {
					id   int64
					text string
				}
				var batch []pending
				for rows.Next() {
					var p pending
					if err := rows.Scan(&p.id, &p.text); err != nil {
						rows.Close()
						return nil, err
					}
					batch = append(batch, p)
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					return nil, err
				}
				if len(batch) == 0 {
					break
				}
				texts := make([]string, len(batch))
				for i, p := range batch {
					texts[i] = p.text
				}
				vecs, err := emb.Embed(ctx, texts)
				if err != nil {
					return nil, fmt.Errorf("backfill embedding failed: %w", err)
				}
				for _, v := range vecs {
					if cur.EmbedDim == 0 {
						cur.EmbedDim = len(v)
					} else if len(v) != cur.EmbedDim {
						return nil, invalidf("embedding provider returned %d-dimensional vectors mid-backfill (expected %d)", len(v), cur.EmbedDim)
					}
				}
				for i, p := range batch {
					if _, err := tx.ExecContext(ctx,
						fmt.Sprintf(`UPDATE %s SET "_embedding" = ? WHERE id = ?`, q(table)),
						schema.EncodeVector(vecs[i]), p.id); err != nil {
						return nil, err
					}
				}
			}
			if emb.Identity != "" {
				cur.EmbedSpace = emb.Identity
			}
			if cur.EmbedDim == 0 {
				var dim int
				if err := tx.QueryRowContext(ctx,
					fmt.Sprintf(`SELECT length("_embedding") / 4 FROM %s WHERE "_embedding" IS NOT NULL LIMIT 1`, q(table))).Scan(&dim); err == nil && dim > 0 {
					cur.EmbedDim = dim
				}
			}
		} else if old.VectorizeField() != nil {
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf(`UPDATE %s SET "_embedding" = NULL`, q(table))); err != nil {
				return nil, err
			}
		}
	}

	cur.Version = old.Version + 1
	if err := saveSchemaTx(ctx, tx, nsName, cur, old.Version, changes); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return cur, nil
}
