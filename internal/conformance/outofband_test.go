package conformance

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

type sqlDB struct {
	db *sql.DB
}

func openSQL(path string) (*sqlDB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw")
	if err != nil {
		return nil, err
	}
	return &sqlDB{db: db}, nil
}

func (s *sqlDB) Exec(query string, args ...any) (sql.Result, error) {
	return s.db.Exec(query, args...)
}

func (s *sqlDB) QueryRow(query string, args ...any) *sql.Row {
	return s.db.QueryRow(query, args...)
}

func (s *sqlDB) Close() error { return s.db.Close() }
