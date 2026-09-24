package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"sync"
	"unicode/utf8"
)

const MaxTokenizeRunes = 64 << 10

var tokenizeOptRe = regexp.MustCompile(`(?i)tokenize\s*=\s*'([^']*)'`)

type tokenizer struct {
	once sync.Once
	db   *sql.DB
	err  error
	mu   sync.Mutex
}

func (t *tokenizer) open() error {
	t.once.Do(func() {
		t.db, t.err = sql.Open("sqlite", "file::memory:?mode=memory")
		if t.err == nil {
			t.db.SetMaxOpenConns(1)
		}
	})
	return t.err
}

func (t *tokenizer) terms(ctx context.Context, spec, text string) ([]string, error) {
	if err := t.open(); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	sum := sha256.Sum256([]byte(spec))
	name := "tok_" + hex.EncodeToString(sum[:8])
	for _, ddl := range []string{
		fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS %s USING fts5(t, tokenize='%s')`, name, spec),
		fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS %s_v USING fts5vocab(%s, 'instance')`, name, name),
	} {
		if _, err := t.db.ExecContext(ctx, ddl); err != nil {
			return nil, err
		}
	}
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(t) VALUES (?)`, name), text); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT term FROM %s_v ORDER BY offset`, name))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var term string
		if err := rows.Scan(&term); err != nil {
			return nil, err
		}
		out = append(out, term)
	}
	return out, rows.Err()
}

func (s *Store) Tokenize(ctx context.Context, nsName, table, text string, inc Incarnation) ([]string, error) {
	if n := utf8.RuneCountInString(text); n > MaxTokenizeRunes {
		return nil, invalidf("text is %d characters; tokenize takes at most %d, which covers any query or field value worth checking", n, MaxTokenizeRunes)
	}
	n, err := s.ns(nsName)
	if err != nil {
		return nil, err
	}
	defer n.unpin()
	sc, err := loadSchema(ctx, n.ro, nsName, table)
	if err != nil {
		return nil, err
	}
	if err := checkScopeIncarnation(ctx, n.ro, nsName, table, inc); err != nil {
		return nil, err
	}
	if len(sc.FTSFields()) == 0 {
		return nil, invalidf("table %s has no fulltext fields, so it has no index to tokenize for; mark a string or text field fulltext with migrate (set_fulltext)", table)
	}
	var ddl string
	if err := n.ro.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE name = ?`, ftsTable(table)).Scan(&ddl); err != nil {
		return nil, err
	}
	spec := "unicode61"
	if m := tokenizeOptRe.FindStringSubmatch(ddl); m != nil {
		spec = m[1]
	}
	return s.tok.terms(ctx, spec, text)
}
