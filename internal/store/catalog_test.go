package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func catalogMeta(t *testing.T, dir, ns, key string) (string, bool) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(filepath.Join(dir, ns+".db"), false))
	if err != nil {
		t.Fatalf("open %s: %v", ns, err)
	}
	defer db.Close()
	var raw []byte
	err = db.QueryRow(`SELECT value FROM _dolmen_meta WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return string(raw), true
}

func setCatalogMeta(t *testing.T, dir, ns, key, value string) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(filepath.Join(dir, ns+".db"), false))
	if err != nil {
		t.Fatalf("open %s: %v", ns, err)
	}
	defer db.Close()
	if _, err := db.Exec(
		`INSERT INTO _dolmen_meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, []byte(value)); err != nil {
		t.Fatalf("write %s: %v", key, err)
	}
}

func seedNamespace(t *testing.T, dir, ns string) {
	t.Helper()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.CreateNamespace(context.Background(), ns, [16]byte{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

func TestCatalogVersionStampedOnFirstOpen(t *testing.T) {
	dir := t.TempDir()
	seedNamespace(t, dir, "cv")

	format, ok := catalogMeta(t, dir, "cv", catalogFormatKey)
	if !ok {
		t.Fatal("a new namespace must record its catalog format")
	}
	if format != strconv.Itoa(CatalogFormat) {
		t.Fatalf("catalog format = %q, want %d", format, CatalogFormat)
	}
	minReader, ok := catalogMeta(t, dir, "cv", catalogMinReaderKey)
	if !ok {
		t.Fatal("a new namespace must record its minimum reader format")
	}
	if minReader != strconv.Itoa(CatalogMinReader) {
		t.Fatalf("catalog min reader = %q, want %d", minReader, CatalogMinReader)
	}
}

func TestCatalogVersionStampIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	seedNamespace(t, dir, "cv")
	setCatalogMeta(t, dir, "cv", catalogMinReaderKey, "1")

	for i := 0; i < 3; i++ {
		st, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		if _, err := st.ListTables(context.Background(), "cv", nil); err != nil {
			t.Fatalf("list tables %d: %v", i, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}

	if format, _ := catalogMeta(t, dir, "cv", catalogFormatKey); format != strconv.Itoa(CatalogFormat) {
		t.Fatalf("repeated opens moved the catalog format to %q, want %d", format, CatalogFormat)
	}
}

func TestCatalogVersionAdoptsUnstampedNamespace(t *testing.T) {
	dir := t.TempDir()
	seedNamespace(t, dir, "legacy")

	db, err := sql.Open("sqlite", dsn(filepath.Join(dir, "legacy.db"), false))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM _dolmen_meta WHERE key IN (?, ?)`, catalogFormatKey, catalogMinReaderKey); err != nil {
		t.Fatalf("strip stamps: %v", err)
	}
	db.Close()

	st, err := Open(dir)
	if err != nil {
		t.Fatalf("a namespace written before the gate must still open: %v", err)
	}
	if _, err := st.ListTables(context.Background(), "legacy", nil); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if format, ok := catalogMeta(t, dir, "legacy", catalogFormatKey); !ok || format != strconv.Itoa(CatalogFormat) {
		t.Fatalf("opening an unstamped namespace must adopt it at the current format, got %q (present=%v)", format, ok)
	}
}

func TestCatalogVersionRejectsNewerStoreAtOpen(t *testing.T) {
	dir := t.TempDir()
	seedNamespace(t, dir, "future")
	setCatalogMeta(t, dir, "future", catalogFormatKey, strconv.Itoa(CatalogFormat+3))
	setCatalogMeta(t, dir, "future", catalogMinReaderKey, strconv.Itoa(CatalogFormat+2))

	st, err := Open(dir)
	if err == nil {
		st.Close()
		t.Fatal("a namespace demanding a newer reader must be refused before serving")
	}
	if !errors.Is(err, ErrCatalogTooNew) {
		t.Fatalf("open error = %v, want ErrCatalogTooNew", err)
	}
	var cve *CatalogVersionError
	if !errors.As(err, &cve) {
		t.Fatalf("open error %v does not carry a CatalogVersionError", err)
	}
	if cve.Namespace != "future" || cve.MinReader != CatalogFormat+2 || cve.Supported != CatalogFormat {
		t.Fatalf("diagnostic carries the wrong numbers: %+v", cve)
	}
	msg := cve.Error()
	for _, want := range []string{"future", "upgrade dolmen", strconv.Itoa(CatalogFormat + 2)} {
		if !strings.Contains(msg, want) {
			t.Fatalf("diagnostic %q does not mention %q", msg, want)
		}
	}
}

func TestCatalogVersionAllowsForwardCompatibleStore(t *testing.T) {
	dir := t.TempDir()
	seedNamespace(t, dir, "fwd")
	setCatalogMeta(t, dir, "fwd", catalogFormatKey, strconv.Itoa(CatalogFormat+5))
	setCatalogMeta(t, dir, "fwd", catalogMinReaderKey, strconv.Itoa(CatalogFormat))

	st, err := Open(dir)
	if err != nil {
		t.Fatalf("a newer catalog that still admits this reader must open: %v", err)
	}
	defer st.Close()
	if _, err := st.ListTables(context.Background(), "fwd", nil); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if format, _ := catalogMeta(t, dir, "fwd", catalogFormatKey); format != strconv.Itoa(CatalogFormat+5) {
		t.Fatalf("an older binary must not stamp the catalog backwards, got %q", format)
	}
}

func TestCatalogVersionRejectsCorruptStamp(t *testing.T) {
	dir := t.TempDir()
	seedNamespace(t, dir, "bad")
	setCatalogMeta(t, dir, "bad", catalogFormatKey, "not-a-number")

	st, err := Open(dir)
	if err != nil {
		t.Fatalf("a corrupt stamp must not fail the whole data directory at open: %v", err)
	}
	defer st.Close()
	if _, err := st.ListTables(context.Background(), "bad", nil); err == nil {
		t.Fatal("a corrupt catalog stamp must be reported when the namespace is opened")
	} else if !strings.Contains(err.Error(), "corrupt catalog metadata") {
		t.Fatalf("corrupt stamp error = %v, want it to name the corrupt catalog metadata", err)
	}
}

func TestCatalogVersionRefusesBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	seedNamespace(t, dir, "future")
	setCatalogMeta(t, dir, "future", catalogFormatKey, strconv.Itoa(CatalogFormat+3))
	setCatalogMeta(t, dir, "future", catalogMinReaderKey, strconv.Itoa(CatalogFormat+2))

	db, err := sql.Open("sqlite", dsn(filepath.Join(dir, "future.db"), false))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE _dolmen_migrations`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	db.Close()

	before, err := os.Stat(filepath.Join(dir, "future.db"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	st := &Store{dir: dir, mu: newCtxMutex(), nss: map[string]*nsDB{}, changeRetention: DefaultChangeRetention}
	if _, err := st.ns("future"); !errors.Is(err, ErrCatalogTooNew) {
		t.Fatalf("opening a too-new namespace = %v, want ErrCatalogTooNew", err)
	}

	db, err = sql.Open("sqlite", dsn(filepath.Join(dir, "future.db"), true))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = '_dolmen_migrations'`).Scan(&n); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if n != 0 {
		t.Fatal("a refused namespace must not have its registry recreated: the newer format may have dropped or renamed that table deliberately")
	}
	after, err := os.Stat(filepath.Join(dir, "future.db"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("a refused namespace must not be written to at all")
	}
}

func TestCorruptCatalogIsATypedError(t *testing.T) {
	dir := t.TempDir()
	seedNamespace(t, dir, "bad")
	setCatalogMeta(t, dir, "bad", catalogFormatKey, "not-a-number")

	st := &Store{dir: dir, mu: newCtxMutex(), nss: map[string]*nsDB{}, changeRetention: DefaultChangeRetention}
	_, err := st.ns("bad")
	if !errors.Is(err, ErrCatalogCorrupt) {
		t.Fatalf("a corrupt stamp must be matchable with errors.Is, not by message text, got %v", err)
	}
}

func TestCatalogGateToleratesAPreMetaNamespace(t *testing.T) {
	dir := t.TempDir()
	seedNamespace(t, dir, "legacy")

	db, err := sql.Open("sqlite", dsn(filepath.Join(dir, "legacy.db"), false))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE _dolmen_meta`); err != nil {
		t.Fatalf("drop meta: %v", err)
	}
	db.Close()

	st := &Store{dir: dir, mu: newCtxMutex(), nss: map[string]*nsDB{}, changeRetention: DefaultChangeRetention}
	if _, err := st.ns("legacy"); err != nil {
		t.Fatalf("a namespace with no meta table at all must still open: %v", err)
	}
	if format, ok := catalogMeta(t, dir, "legacy", catalogFormatKey); !ok || format != strconv.Itoa(CatalogFormat) {
		t.Fatalf("it must then be adopted at the current format, got %q (present=%v)", format, ok)
	}
}
