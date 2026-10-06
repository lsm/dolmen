package lakehouse

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func WithSharedFilter(on bool) OpenOption {
	return func(s *Store) { s.sharedFilter = on }
}

func sharedArg(v any) any {
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return i
		}
		f, _ := n.Float64()
		return f
	}
	return v
}

func sharedValue(f *schema.Field, v any) any {
	if v == nil {
		return nil
	}
	if f != nil && f.Type == schema.Boolean {
		if b, ok := v.(bool); ok {
			if b {
				return int64(1)
			}
			return int64(0)
		}
	}
	return v
}

func (s *Store) sharedMatch(ctx context.Context, state tableState, filter string, args []any, scope *store.RowScope) ([]int64, error) {
	rows, err := scanRows(ctx, state, nil)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file::memory:?mode=memory")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	sc := state.schema
	cols := []string{quoteIdent("id") + " INTEGER", quoteIdent("created_at") + " TEXT"}
	names := []string{"id", "created_at"}
	for _, f := range sc.Fields {
		cols = append(cols, quoteIdent(f.Name)+" "+schema.SQLType(f))
		names = append(names, f.Name)
	}
	if sc.HasOwner {
		cols = append(cols, quoteIdent(schema.OwnerColumn)+" TEXT")
		names = append(names, schema.OwnerColumn)
	}
	table := quoteIdent(sc.Name)
	if _, err := db.ExecContext(ctx, "CREATE TABLE "+table+" ("+strings.Join(cols, ", ")+")"); err != nil {
		return nil, err
	}
	quoted := make([]string, len(names))
	marks := make([]string, len(names))
	for i, name := range names {
		quoted[i] = quoteIdent(name)
		marks[i] = "?"
	}
	insert := "INSERT INTO " + table + " (" + strings.Join(quoted, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")"
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, insert)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	values := make([]any, len(names))
	for _, row := range rows {
		if !inScope(sc, scope, row) {
			continue
		}
		for i, name := range names {
			values[i] = sharedValue(sc.Field(name), row[name])
		}
		if _, err := stmt.ExecContext(ctx, values...); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	query := "SELECT " + quoteIdent("id") + " FROM " + table + " WHERE (" + filter + ")"
	bound := make([]any, 0, len(args)+1)
	for _, a := range args {
		bound = append(bound, sharedArg(a))
	}
	if scope != nil && scope.Owner != "" && sc.HasOwner {
		query += " AND " + quoteIdent(schema.OwnerColumn) + " = ?"
		bound = append(bound, scope.Owner)
	}
	result, err := db.QueryContext(ctx, query+" ORDER BY "+quoteIdent("id"), bound...)
	if err != nil {
		return nil, store.NewFilterError(filter, err)
	}
	defer result.Close()
	ids := []int64{}
	for result.Next() {
		var id int64
		if err := result.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := result.Err(); err != nil {
		return nil, store.NewFilterError(filter, err)
	}
	return ids, nil
}
