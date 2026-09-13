package dolmen_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lsm/dolmen"
)

type exampleProvider struct{}

func (exampleProvider) Identity() string { return "example|provider|4" }

func (exampleProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 1, 1, 1}
	}
	return out, nil
}

func (exampleProvider) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return []float32{1, 1, 1, 1}, nil
}

func runExampleFlow() error {
	dir, err := os.MkdirTemp("", "dolmen-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	st, err := dolmen.Open(filepath.Join(dir, "data"), dolmen.WithEmbedding(exampleProvider{}))
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "demo", "notes", []dolmen.Field{
		{Name: "body", Type: dolmen.Text, Fulltext: true, Vectorize: true},
	}); err != nil {
		return err
	}

	ins, err := st.Insert(ctx, "demo", "notes", []map[string]any{
		{"body": "hello world"},
	}, dolmen.InsertOptions{IdempotencyKey: "one"})
	if err != nil {
		return err
	}

	if _, err := st.Insert(ctx, "demo", "notes", []map[string]any{
		{"body": "something else"},
	}, dolmen.InsertOptions{IdempotencyKey: "one"}); !errors.Is(err, dolmen.ErrConflict) {
		return fmt.Errorf("key reuse: want ErrConflict, got %v", err)
	}

	rows, err := st.GetRows(ctx, "demo", "notes", ins.Ids)
	if err != nil {
		return err
	}
	if len(rows.Rows) != 1 || rows.Truncated {
		return fmt.Errorf("read: %d row(s), truncated %v", len(rows.Rows), rows.Truncated)
	}

	hits, err := st.SearchVector(ctx, "demo", "notes", dolmen.VectorQuery{Text: "hello"}, dolmen.SearchOptions{})
	if err != nil {
		return err
	}
	if len(hits.Rows) != 1 {
		return fmt.Errorf("vector hits: %d", len(hits.Rows))
	}
	return nil
}

func Example() {
	if err := runExampleFlow(); err != nil {
		fmt.Println("example:", err)
	}
}

func TestExampleFlowExecutes(t *testing.T) {
	if err := runExampleFlow(); err != nil {
		t.Fatalf("example flow: %v", err)
	}
}
