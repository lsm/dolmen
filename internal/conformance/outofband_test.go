package conformance

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

// sqlDB is a direct connection to a namespace file, used exclusively to plant
// fixtures the API refuses to produce itself (corrupt stored vectors). It is
// the "out-of-band writer" the skipped_vectors contract is written for.
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

func (s *sqlDB) Close() error { return s.db.Close() }
