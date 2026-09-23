package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

const (
	DefaultSearchLimit = 10
	MaxSearchLimit     = 200
)

func searchLimit(n int) int {
	if n <= 0 {
		return DefaultSearchLimit
	}
	if n > MaxSearchLimit {
		return MaxSearchLimit
	}
	return n
}

func ftsWordByte(c byte) bool {
	switch c {
	case '"', '(', ')', '{', '}', ':', ',', '+', '-', '*', '^':
		return false
	}
	return c > ' '
}

var (
	ftsSyntaxNearRe = regexp.MustCompile(`^fts5: syntax error near "(.)"$`)
	ftsNoColumnRe   = regexp.MustCompile(`^column "([^"]+)" not found$`)
)

func ftsMatchError(match string, err error) error {
	redacted := NewRedactedSQLite(err)
	msg := redacted.Error()
	if m := ftsSyntaxNearRe.FindStringSubmatch(msg); m != nil && ftsPunctuation(m[1]) {
		return invalidf(`query %q: FTS5 reads %q as query syntax, not text, so a term that contains punctuation must be double-quoted (e.g. "don't", "v1.2", "c++"), or written without the punctuation`, match, m[1])
	}
	switch {
	case msg == "unterminated string":
		return invalidf(`query %q: a double-quoted phrase is never closed; add the closing " (a quote inside a phrase is written as two, "")`, match)
	case msg == `fts5: syntax error near ""`:
		return invalidf(`query %q: the query ends where FTS5 expects a term; drop a trailing AND, OR or NOT, or close an open parenthesis`, match)
	case strings.HasPrefix(msg, "unknown special query"):
		return invalidf(`query %q: FTS5 reads a query that starts with * as a special command; put * after a word to search by prefix (e.g. pay*)`, match)
	}
	if m := ftsNoColumnRe.FindStringSubmatch(msg); m != nil {
		return invalidf(`query %q: FTS5 reads a word before a colon as the name of a column to search, and this table has no full-text field named %q; double-quote the term to search for it as text, or put one of the table's full-text fields before the colon`, match, m[1])
	}
	return fmt.Errorf("%w: %w", ErrInvalid, redacted)
}

func ftsPunctuation(tok string) bool {
	if len(tok) != 1 {
		return false
	}
	c := tok[0]
	switch {
	case c <= ' ' || c >= 0x80, c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
		return false
	case c == '(' || c == ')' || c == '"':
		return false
	}
	return true
}

func bareHyphenTerm(match string) bool {
	inq := false
	for i := 0; i < len(match); i++ {
		switch c := match[i]; {
		case c == '"':
			inq = !inq
		case c == '-' && !inq:
			j := i + 1
			for j < len(match) && match[j] <= ' ' {
				j++
			}
			k := j
			if k < len(match) && match[k] == '{' {
				k++
				for k < len(match) && match[k] != '}' {
					k++
				}
				if k < len(match) {
					k++
				}
			} else if k < len(match) && match[k] == '"' {
				k++
				for k < len(match) && match[k] != '"' {
					k++
				}
				if k < len(match) {
					k++
				}
			} else {
				for k < len(match) && ftsWordByte(match[k]) {
					k++
				}
			}
			for k < len(match) && match[k] <= ' ' {
				k++
			}
			if k >= len(match) || match[k] != ':' {
				return true
			}
			i = k
		}
	}
	return false
}

func (s *Store) SearchFulltext(ctx context.Context, nsName, table, match string, filter string, args []any, includeHidden bool, scope *RowScope, scopeIncarnation Incarnation, page Page) (SearchResult, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return SearchResult{}, err
	}
	tx, done, err := beginCallerTx(ctx, n.ro, nil)
	if err != nil {
		return SearchResult{}, err
	}
	defer done()
	sc, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return SearchResult{}, err
	}
	if len(sc.FTSFields()) == 0 {
		return SearchResult{}, invalidf("table %s has no fulltext fields", table)
	}
	if bareHyphenTerm(match) {
		return SearchResult{}, invalidf(`query %q: FTS5 parses a bare "-" as a column filter, so a hyphenated term must be double-quoted (e.g. "money-back"); to exclude a term, write NOT between words`, match)
	}
	limit := searchLimit(page.Limit)
	offset := page.Offset
	if offset < 0 {
		return SearchResult{}, invalidf("offset must be non-negative")
	}
	filter = strings.TrimSpace(filter)
	if filter != "" {
		if strings.Contains(filter, ";") {
			return SearchResult{}, invalidf("multiple statements are not allowed in filter")
		}
		if len(args) > 100 {
			return SearchResult{}, invalidf("too many filter arguments")
		}
		for i, a := range args {
			args[i] = normalizeArg(a)
		}
	}

	if err := checkScopeIncarnation(ctx, tx, nsName, table, scopeIncarnation); err != nil {
		return SearchResult{}, err
	}
	if err := scopeUsable(scope, sc); err != nil {
		return SearchResult{}, err
	}
	stmt := fmt.Sprintf(`SELECT rowid FROM %s WHERE %s MATCH ? ORDER BY rank, rowid LIMIT ? OFFSET ?`,
		q(ftsTable(table)), ftsTable(table))
	qargs := []any{match, limit + 1, offset}
	if clause, sargs := scopeClause(scope, "b"); clause != "" {
		stmt = fmt.Sprintf(`SELECT rowid FROM %s WHERE EXISTS (SELECT 1 FROM %s b WHERE b.id = %s.rowid AND %s) AND %s MATCH ? ORDER BY rank, rowid LIMIT ? OFFSET ?`,
			q(ftsTable(table)), q(table), ftsTable(table), clause, ftsTable(table))
		qargs = append(append([]any(nil), sargs...), match, limit+1, offset)
	}

	classify := func(err error) error {
		if filter != "" {
			return NewFilterError(filter, err)
		}
		return ftsMatchError(match, err)
	}
	if filter != "" {
		prefix, source, scopeArgs := scopedSource(table, scope)
		probe, err := tx.QueryContext(ctx,
			fmt.Sprintf(`%sSELECT 1 FROM %s WHERE %s LIMIT 0`, prefix, source, filter),
			append(append(make([]any, 0, len(scopeArgs)+len(args)), scopeArgs...), args...)...)
		if err != nil {
			return SearchResult{}, NewFilterError(filter, err)
		}
		probe.Close()
		probe, err = tx.QueryContext(ctx,
			fmt.Sprintf(`SELECT rowid FROM %s WHERE %s MATCH ? LIMIT 1`, q(ftsTable(table)), ftsTable(table)), match)
		if err != nil {
			return SearchResult{}, ftsMatchError(match, err)
		}
		probe.Close()
		stmt = fulltextFilterStmt(table, filter, len(scopeArgs)+len(args), prefix, source)
		qargs = make([]any, 0, len(scopeArgs)+len(args)+3)
		qargs = append(qargs, scopeArgs...)
		qargs = append(qargs, args...)
		qargs = append(qargs, match, limit+1, offset)
	}
	rows, err := tx.QueryContext(ctx, stmt, qargs...)
	if err != nil {
		return SearchResult{}, classify(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return SearchResult{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return SearchResult{}, classify(err)
	}

	hasMore := len(ids) > limit
	if hasMore {
		ids = ids[:limit]
	}
	out, complete, err := fetchByIDs(ctx, tx, table, ids, projectionFromSchema(sc, includeHidden))
	if err != nil {
		return SearchResult{}, err
	}
	return SearchResult{Rows: out, Truncated: hasMore || !complete}, nil
}

func fulltextFilterStmt(table, filter string, nargs int, prefix, source string) string {
	return fmt.Sprintf(`%sSELECT rowid FROM %s WHERE EXISTS (SELECT 1 FROM %s WHERE %s.id = %s.rowid AND (%s)) AND %s MATCH ?%d ORDER BY rank, rowid LIMIT ?%d OFFSET ?%d`,
		prefix, q(ftsTable(table)), source, source, ftsTable(table), filter, ftsTable(table), nargs+1, nargs+2, nargs+3)
}

type dbQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func fetchByIDs(ctx context.Context, db dbQueryer, table string, ids []int64, proj *projection) ([]map[string]any, bool, error) {
	return fetchByIDsScoped(ctx, db, table, ids, proj, nil)
}

func fetchByIDsScoped(ctx context.Context, db dbQueryer, table string, ids []int64, proj *projection, scope *RowScope) ([]map[string]any, bool, error) {
	if len(ids) == 0 {
		return []map[string]any{}, true, nil
	}
	values := make([]string, len(ids))
	args := make([]any, 0, len(ids)*2)
	for i, id := range ids {
		values[i] = "(?, ?)"
		args = append(args, i, id)
	}
	source := q(table)
	if clause, sargs := scopeClause(scope, ""); clause != "" {
		source = fmt.Sprintf("(SELECT * FROM %s WHERE %s)", q(table), clause)
		args = append(args, sargs...)
	}
	rows, err := db.QueryContext(ctx,
		fmt.Sprintf(`WITH _ranked(pos, id) AS (VALUES %s) SELECT t.* FROM _ranked JOIN %s t ON t.id = _ranked.id ORDER BY _ranked.pos`,
			strings.Join(values, ", "), source), args...)
	if err != nil {
		return nil, false, storedTooBig(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, false, err
	}
	byID := map[int64]map[string]any{}
	labelBytes := 0
	for _, c := range cols {
		if proj.isHidden(c) {
			continue
		}
		labelBytes += encodedSize(c) + 16
	}
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
			if proj.isHidden(c) {
				continue
			}
			if err := checkRowValue(c, vals[i]); err != nil {
				return nil, false, err
			}
			if c == "id" {
				if v, ok := vals[i].(int64); ok {
					id = v
				}
			}
			if total+rowBytes+rawValSize(vals[i]) > MaxQueryBytes {
				if len(byID) == 0 {
					return nil, false, invalidf("search result exceeds the %d MiB response budget on its first row", MaxQueryBytes>>20)
				}
				complete = false
				break scan
			}
			v := proj.decodeColumn(c, vals[i])
			m[c] = v
			rowBytes += proj.presentedSize(c, vals[i], v)
			if total+rowBytes+labelBytes > MaxQueryBytes {
				if len(byID) == 0 {
					return nil, false, invalidf("search result exceeds the %d MiB response budget on its first row", MaxQueryBytes>>20)
				}
				complete = false
				break scan
			}
		}
		total += rowBytes + labelBytes
		byID[id] = m
	}
	if err := rows.Err(); err != nil {
		return nil, false, storedTooBig(err)
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			out = append(out, m)
		}
	}
	return out, complete, nil
}

const DefaultDeleteLimit = 1000

type DeleteOptions struct {
	DryRun  bool
	Limit   int
	Confirm bool
}

type DeleteResult struct {
	Matched int64
	Deleted int64
	Changes ChangeRange
}

func (s *Store) Delete(ctx context.Context, nsName, table, where string, args []any, opts DeleteOpts, scope *RowScope, scopeIncarnation Incarnation) (DeleteResult, error) {
	where = strings.TrimSpace(where)
	if where == "" {
		return DeleteResult{}, invalidf("filter is required (pass \"1=1\" to delete everything)")
	}
	if hasStatementSeparator(where) {
		return DeleteResult{}, invalidf("multiple statements are not allowed in filter")
	}
	for i, a := range args {
		args[i] = normalizeArg(a)
	}
	n, err := s.ns(nsName)
	if err != nil {
		return DeleteResult{}, err
	}

	if opts.DryRun {
		tx, done, err := beginCallerTx(ctx, n.ro, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return DeleteResult{}, err
		}
		defer done()
		if err := checkScopeIncarnation(ctx, tx, nsName, table, scopeIncarnation); err != nil {
			return DeleteResult{}, err
		}
		sc, err := loadSchema(ctx, tx, nsName, table)
		if err != nil {
			return DeleteResult{}, err
		}
		if err := scopeUsable(scope, sc); err != nil {
			return DeleteResult{}, err
		}
		prefix, source, scopeArgs := scopedSource(table, scope)
		var matched int64
		if err := tx.QueryRowContext(ctx,
			fmt.Sprintf(`%sSELECT count(*) FROM %s WHERE %s`, prefix, source, where),
			append(append(make([]any, 0, len(scopeArgs)+len(args)), scopeArgs...), args...)...).Scan(&matched); err != nil {
			return DeleteResult{}, NewFilterError(where, err)
		}
		return DeleteResult{Matched: matched, Deleted: 0}, nil
	}

	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return DeleteResult{}, err
	}
	defer tx.Rollback()

	if err := checkScopeIncarnation(ctx, tx, nsName, table, scopeIncarnation); err != nil {
		return DeleteResult{}, err
	}
	sc, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return DeleteResult{}, err
	}
	if err := scopeUsable(scope, sc); err != nil {
		return DeleteResult{}, err
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp._dolmen_delete_ids`); err != nil {
		return DeleteResult{}, err
	}
	prefix, source, scopeArgs := scopedSource(table, scope)
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`CREATE TEMP TABLE _dolmen_delete_ids AS %sSELECT id, %s AS owner FROM %s WHERE %s`, prefix, changeOwnerColumn(sc), source, where),
		append(append(make([]any, 0, len(scopeArgs)+len(args)), scopeArgs...), args...)...); err != nil {
		return DeleteResult{}, NewFilterError(where, err)
	}

	var matched int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM _dolmen_delete_ids`).Scan(&matched); err != nil {
		return DeleteResult{}, err
	}

	limit := int64(DefaultDeleteLimit)
	if opts.Limit > 0 {
		limit = int64(opts.Limit)
	}
	if matched > limit && !opts.Confirm {
		return DeleteResult{}, invalidf("filter matched %d rows, exceeding the delete limit of %d; pass confirm: true to proceed or dry_run: true to preview", matched, limit)
	}

	if len(sc.FTSFields()) > 0 {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE rowid IN (SELECT id FROM _dolmen_delete_ids)`, q(ftsTable(table)))); err != nil {
			return DeleteResult{}, err
		}
	}
	res, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE id IN (SELECT id FROM _dolmen_delete_ids)`, q(table)))
	if err != nil {
		return DeleteResult{}, err
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return DeleteResult{}, err
	}

	changes, err := mintChangesFromTemp(ctx, tx, table, ChangeDelete, `_dolmen_delete_ids`)
	if err != nil {
		return DeleteResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE _dolmen_delete_ids`); err != nil {
		return DeleteResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeleteResult{}, err
	}

	s.notifyCommitted(nsName, table, changes)
	return DeleteResult{Matched: matched, Deleted: deleted, Changes: changes}, nil
}

func storedTooBig(err error) error {
	if tooBigRe.MatchString(err.Error()) {
		return invalidf("a matching row holds a value larger than the %d MiB response budget", MaxQueryBytes>>20)
	}
	return err
}
