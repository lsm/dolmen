package inprocess

import (
	"os"

	"github.com/parquet-go/parquet-go"
)

type ParquetRow struct {
	ID    int64   `parquet:"id"`
	Body  string  `parquet:"body"`
	Score float64 `parquet:"score"`
	Flag  bool    `parquet:"flag"`
}

func readParquet(path string) ([]ParquetRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return parquet.Read[ParquetRow](f, info.Size())
}

func writeParquet(path string, rows []ParquetRow) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := parquet.NewGenericWriter[ParquetRow](f)
	if _, err := w.Write(rows); err != nil {
		f.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return f.Close()
}

func (n *Namespace) createParquetTable(name, path string) error {
	rows, err := readParquet(path)
	if err != nil {
		return err
	}
	data := make([][]any, 0, len(rows))
	for _, r := range rows {
		flag := int8(0)
		if r.Flag {
			flag = 1
		}
		data = append(data, []any{r.ID, r.Body, r.Score, flag})
	}
	return n.CreateTable(TableSpec{
		Name:    name,
		Columns: []Column{{Name: "id", Type: Int64()}, {Name: "body", Type: Text()}, {Name: "score", Type: Float64()}, {Name: "flag", Type: Bool()}},
		Rows:    data,
	})
}
