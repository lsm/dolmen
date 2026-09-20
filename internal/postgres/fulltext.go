package postgres

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/schema"
)

const ftsColumn = "_fts"

func fulltextFields(fields []schema.Field) []schema.Field {
	out := []schema.Field{}
	for _, f := range fields {
		if f.Fulltext {
			out = append(out, f)
		}
	}
	return out
}

func ftsExpression(fields []schema.Field, columns map[string]string) string {
	parts := []string{}
	for _, f := range fulltextFields(fields) {
		parts = append(parts, "coalesce("+ident(columns[f.Name])+",'')")
	}
	if len(parts) == 0 {
		return ""
	}
	return "to_tsvector('" + ftsConfig + "'," + strings.Join(parts, "||' '||") + ")"
}

func ftsColumnDDL(fields []schema.Field, columns map[string]string) string {
	expression := ftsExpression(fields, columns)
	if expression == "" {
		return ""
	}
	return ident(ftsColumn) + " tsvector GENERATED ALWAYS AS (" + expression + ") STORED"
}

func ftsIndexDDL(ctx context.Context, tx pgx.Tx, n namespace, table string) (string, error) {
	name, err := physicalRelation(ctx, tx, n, table+"_fts")
	if err != nil {
		return "", err
	}
	return "CREATE INDEX " + ident(name) + " ON " + ident(n.physical, table) + " USING GIN (" + ident(ftsColumn) + ")", nil
}
