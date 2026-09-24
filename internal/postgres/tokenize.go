package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/store"
)

func (s *Store) Tokenize(ctx context.Context, ns, table, text string, expected store.Incarnation) ([]string, error) {
	if len(text) > store.MaxTokenizeBytes {
		return nil, invalidf("text is %d bytes; tokenize takes at most %d, which covers any query or field value worth checking", len(text), store.MaxTokenizeBytes)
	}
	out := []string{}
	err := s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, table)
		if err != nil {
			return err
		}
		if err := s.guardScope(ctx, tx, n, table, state, expected); err != nil {
			return err
		}
		if len(state.schema.FTSFields()) == 0 {
			return invalidf("table %s has no fulltext fields, so it has no index to tokenize for; mark a string or text field fulltext with migrate (set_fulltext)", table)
		}
		rows, err := tx.Query(ctx, `SELECT lexeme FROM unnest(to_tsvector('`+ftsConfig+`', $1)) AS t(lexeme, positions, weights), unnest(positions) AS p ORDER BY p`, text)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var term string
			if err := rows.Scan(&term); err != nil {
				return err
			}
			out = append(out, term)
		}
		return rows.Err()
	})
	return out, err
}
