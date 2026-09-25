package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

const (
	CatalogFormat = 3

	CatalogMinReader = 3

	catalogFormatKey    = "catalog_format"
	catalogMinReaderKey = "catalog_min_reader"
)

var ErrCatalogTooNew = errors.New("namespace catalog is newer than this binary supports")

var ErrCatalogCorrupt = errors.New("corrupt catalog metadata")

type CatalogVersionError struct {
	Namespace string
	Format    int
	MinReader int
	Supported int
}

func (e *CatalogVersionError) Error() string {
	return fmt.Sprintf(
		"namespace %s was written by a newer dolmen: its catalog is format %d and requires a binary supporting format %d or higher, but this binary supports format %d; upgrade dolmen to open this data directory, or point -data at a directory this binary wrote",
		e.Namespace, e.Format, e.MinReader, e.Supported)
}

func (e *CatalogVersionError) Is(target error) bool {
	return target == ErrCatalogTooNew
}

func refuseNewerCatalog(ctx context.Context, rw *sql.DB, nsName string) error {
	var metaTables int
	if err := rw.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = '_dolmen_meta'`).Scan(&metaTables); err != nil {
		return err
	}
	if metaTables == 0 {
		return nil
	}
	format, minReader, err := readCatalogVersion(ctx, rw)
	if err != nil {
		return err
	}
	if minReader > CatalogFormat {
		return &CatalogVersionError{Namespace: nsName, Format: format, MinReader: minReader, Supported: CatalogFormat}
	}
	return nil
}

func ensureCatalogVersion(ctx context.Context, rw *sql.DB, nsName string) error {
	tx, err := rw.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	format, haveFormat, err := readCatalogInt(ctx, tx, catalogFormatKey)
	if err != nil {
		return err
	}
	minReader, haveMinReader, err := readCatalogInt(ctx, tx, catalogMinReaderKey)
	if err != nil {
		return err
	}
	storedMinReader := minReader

	if !haveFormat {
		format = CatalogFormat
	}
	if !haveMinReader || minReader < CatalogMinReader {
		minReader = CatalogMinReader
	}

	if minReader > CatalogFormat {
		return &CatalogVersionError{Namespace: nsName, Format: format, MinReader: minReader, Supported: CatalogFormat}
	}

	if !haveFormat || !haveMinReader || format < CatalogFormat || minReader > storedMinReader {
		stamp := format
		if stamp < CatalogFormat {
			stamp = CatalogFormat
		}
		if err := writeCatalogInt(ctx, tx, catalogFormatKey, stamp); err != nil {
			return err
		}
		if err := writeCatalogInt(ctx, tx, catalogMinReaderKey, minReader); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func truncateStamp(raw []byte) string {
	const max = 32
	if len(raw) <= max {
		return string(raw)
	}
	return string(raw[:max]) + "..."
}

func readCatalogInt(ctx context.Context, tx *sql.Tx, key string) (int, bool, error) {
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT value FROM _dolmen_meta WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	n, convErr := strconv.Atoi(string(raw))
	if convErr != nil || n < 1 {
		return 0, false, fmt.Errorf("%w: %s = %q is not a positive integer", ErrCatalogCorrupt, key, truncateStamp(raw))
	}
	return n, true, nil
}

func writeCatalogInt(ctx context.Context, tx *sql.Tx, key string, value int) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO _dolmen_meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, []byte(strconv.Itoa(value)))
	return err
}

func readCatalogVersion(ctx context.Context, db rowQuerier) (format int, minReader int, err error) {
	format, minReader = CatalogFormat, CatalogMinReader
	for key, dst := range map[string]*int{catalogFormatKey: &format, catalogMinReaderKey: &minReader} {
		var raw []byte
		scanErr := db.QueryRowContext(ctx, `SELECT value FROM _dolmen_meta WHERE key = ?`, key).Scan(&raw)
		if errors.Is(scanErr, sql.ErrNoRows) {
			continue
		}
		if scanErr != nil {
			return 0, 0, scanErr
		}
		n, convErr := strconv.Atoi(string(raw))
		if convErr != nil || n < 1 {
			return 0, 0, fmt.Errorf("%w: %s = %q is not a positive integer", ErrCatalogCorrupt, key, truncateStamp(raw))
		}
		*dst = n
	}
	return format, minReader, nil
}
