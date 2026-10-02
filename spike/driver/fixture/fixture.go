package fixture

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/catalog"
	_ "github.com/apache/iceberg-go/catalog/sql"
	sqlcatalog "github.com/apache/iceberg-go/catalog/sql"
	"github.com/apache/iceberg-go/table"
	"github.com/parquet-go/parquet-go"
	_ "modernc.org/sqlite"
)

type Table struct {
	Catalog  *sqlcatalog.Catalog
	Ident    table.Identifier
	Table    *table.Table
	DataDir  string
	RowCount int
	Deleted  int
}

type Record struct {
	ID    int64
	Body  string
	Score float64
	Group string
	Live  bool
}

const (
	NSName    = "app"
	TableName = "events"
)

func ArrowSchema() *arrow.Schema {
	return arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64},
		{Name: "body", Type: arrow.BinaryTypes.String},
		{Name: "score", Type: arrow.PrimitiveTypes.Float64},
		{Name: "grp", Type: arrow.BinaryTypes.String},
		{Name: "live", Type: arrow.FixedWidthTypes.Boolean},
	}, nil)
}

func icebergSchema() *iceberg.Schema {
	return iceberg.NewSchema(0,
		iceberg.NestedField{ID: 1, Name: "id", Type: iceberg.PrimitiveTypes.Int64, Required: true},
		iceberg.NestedField{ID: 2, Name: "body", Type: iceberg.PrimitiveTypes.String},
		iceberg.NestedField{ID: 3, Name: "score", Type: iceberg.PrimitiveTypes.Float64},
		iceberg.NestedField{ID: 4, Name: "grp", Type: iceberg.PrimitiveTypes.String},
		iceberg.NestedField{ID: 5, Name: "live", Type: iceberg.PrimitiveTypes.Bool},
	)
}

func OpenCatalog(ctx context.Context, dataDir string) (*sqlcatalog.Catalog, error) {
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "catalog.db"))
	if err != nil {
		return nil, err
	}
	props := iceberg.Properties{"format-version": "2"}
	cat, err := sqlcatalog.NewCatalog("dolmen", db, sqlcatalog.SQLite, props)
	if err != nil {
		return nil, err
	}
	if err := cat.CreateNamespace(ctx, table.Identifier{NSName}, nil); err != nil {
		return nil, err
	}
	return cat, nil
}

func Write(ctx context.Context, dataDir string, rows int) (*Table, error) {
	cat, err := OpenCatalog(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	ident := table.Identifier{NSName, TableName}
	tbl, err := cat.CreateTable(ctx, ident, icebergSchema(),
		catalog.WithLocation(TableDataDir(dataDir)),
		catalog.WithProperties(iceberg.Properties{"format-version": "2"}))
	if err != nil {
		return nil, fmt.Errorf("create table: %w", err)
	}

	recs := MakeRecords(rows)
	tbl, err = tbl.Append(ctx, recordReader(recs), nil)
	if err != nil {
		return nil, fmt.Errorf("append 1: %w", err)
	}
	late := []Record{{ID: int64(rows), Body: "late arrival", Score: -1.5, Group: "zeta", Live: false}}
	tbl, err = tbl.Append(ctx, recordReader(late), nil)
	if err != nil {
		return nil, fmt.Errorf("append 2: %w", err)
	}

	deleted := []int64{2, 5}
	if err := ApplyPositionDeletes(ctx, tbl, deleted); err != nil {
		return nil, err
	}
	tbl, err = cat.LoadTable(ctx, ident)
	if err != nil {
		return nil, fmt.Errorf("reload after delete: %w", err)
	}

	return &Table{
		Catalog:  cat,
		Ident:    ident,
		Table:    tbl,
		DataDir:  TableDataDir(dataDir),
		RowCount: rows + 1,
		Deleted:  len(deleted),
	}, nil
}

func dataFileHolding(location string, wantID int64) (string, error) {
	matches, err := filepath.Glob(filepath.Join(location, "data", "*.parquet"))
	if err != nil {
		return "", err
	}
	sort.Strings(matches)
	for _, m := range matches {
		ids, err := idsInParquet(m)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", m, err)
		}
		for _, id := range ids {
			if id == wantID {
				return m, nil
			}
		}
	}
	return "", fmt.Errorf("no data file under %s holds id %d, so the position delete would target nothing", location, wantID)
}

type idRow struct {
	ID int64 `parquet:"id"`
}

func idsInParquet(path string) ([]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := parquet.NewGenericReader[idRow](f)
	rows := make([]idRow, 0, 64)
	for {
		batch := make([]idRow, 64)
		n, err := r.Read(batch)
		for i := 0; i < n; i++ {
			rows = append(rows, batch[i])
		}
		if err != nil || n == 0 {
			break
		}
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids, nil
}

type posDelete struct {
	FilePath string `parquet:"file_path"`
	Pos      int64  `parquet:"pos"`
}

func ApplyPositionDeletes(ctx context.Context, tbl *table.Table, positions []int64) error {
	target := int64(0)
	if len(positions) > 0 {
		target = positions[0]
	}
	dataFile, err := dataFileHolding(tbl.Location(), target)
	if err != nil {
		return err
	}
	dir := filepath.Join(tbl.Location(), "deletes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "pos-del-0001.parquet")
	rows := make([]posDelete, 0, len(positions))
	for _, p := range positions {
		rows = append(rows, posDelete{FilePath: dataFile, Pos: p})
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := parquet.NewGenericWriter[posDelete](f)
	if _, err := w.Write(rows); err != nil {
		f.Close()
		return fmt.Errorf("write pos delete: %w", err)
	}
	if err := w.Close(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	df, err := iceberg.NewDataFileBuilder(
		*iceberg.UnpartitionedSpec, iceberg.EntryContentPosDeletes,
		path, iceberg.ParquetFile, nil, nil, nil, int64(len(positions)), 512)
	if err != nil {
		return err
	}

	tx := tbl.NewTransaction()
	if err := tx.NewRowDelta(nil).AddDeletes(df.Build()).Commit(ctx); err != nil {
		return fmt.Errorf("row delta: %w", err)
	}
	if _, err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit row delta: %w", err)
	}
	return nil
}

func MakeRecords(rows int) []Record {
	groups := []string{"alpha", "beta", "gamma", "delta"}
	out := make([]Record, 0, rows)
	for i := 0; i < rows; i++ {
		out = append(out, Record{
			ID:    int64(i),
			Body:  fmt.Sprintf("event body %d", i),
			Score: float64(i) * 1.25,
			Group: groups[i%len(groups)],
			Live:  i%3 != 0,
		})
	}
	return out
}

func recordReader(recs []Record) array.RecordReader {
	sch := ArrowSchema()
	b := array.NewRecordBuilder(memory.NewGoAllocator(), sch)
	defer b.Release()
	for _, r := range recs {
		b.Field(0).(*array.Int64Builder).Append(r.ID)
		b.Field(1).(*array.StringBuilder).Append(r.Body)
		b.Field(2).(*array.Float64Builder).Append(r.Score)
		b.Field(3).(*array.StringBuilder).Append(r.Group)
		b.Field(4).(*array.BooleanBuilder).Append(r.Live)
	}
	arr := b.NewRecord()
	rdr, _ := array.NewRecordReader(sch, []arrow.Record{arr})
	return rdr
}

func TableDataDir(dataDir string) string {
	return filepath.Join(dataDir, NSName, TableName)
}

func SnapshotIDs(t *Table) []int64 {
	snaps := t.Table.Metadata().Snapshots()
	ids := make([]int64, 0, len(snaps))
	for _, s := range snaps {
		ids = append(ids, s.SnapshotID)
	}
	return ids
}

func CurrentSnapshotID(t *Table) int64 {
	cur := t.Table.CurrentSnapshot()
	if cur == nil {
		return 0
	}
	return cur.SnapshotID
}

func ExpectedLiveRows(rows int, deleted []int64) int {
	return rows + 1 - len(deleted)
}

func DeletedPositions() []int64 { return []int64{2, 5} }

func LiveRowsBeforeDelete(rows int) int {
	notLive := (rows + 2) / 3
	return rows - notLive
}

func DeletedLiveRows() int {
	n := 0
	for _, p := range DeletedPositions() {
		if p%3 != 0 {
			n++
		}
	}
	return n
}

func WriteSecret(dir, name, body string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, name)
	return p, os.WriteFile(p, []byte(body), 0o644)
}

func WriteBatched(ctx context.Context, dataDir string, rows, batch int) (*Table, error) {
	cat, err := OpenCatalog(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	ident := table.Identifier{NSName, TableName}
	tbl, err := cat.CreateTable(ctx, ident, icebergSchema(),
		catalog.WithLocation(TableDataDir(dataDir)),
		catalog.WithProperties(iceberg.Properties{"format-version": "2"}))
	if err != nil {
		return nil, fmt.Errorf("create table: %w", err)
	}
	recs := MakeRecords(rows)
	for start := 0; start < len(recs); start += batch {
		end := start + batch
		if end > len(recs) {
			end = len(recs)
		}
		chunk := recs[start:end]
		recs[start] = Record{}
		recs[end-1] = Record{}
		tbl, err = tbl.Append(ctx, recordReader(chunk), nil)
		if err != nil {
			return nil, fmt.Errorf("append at %d: %w", start, err)
		}
	}
	return &Table{
		Catalog:  cat,
		Ident:    ident,
		Table:    tbl,
		DataDir:  TableDataDir(dataDir),
		RowCount: rows,
	}, nil
}
