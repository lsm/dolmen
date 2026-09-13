package dolmen

import (
	"context"
	"errors"
	"testing"
)

type staticProvider struct {
	identity string
}

func (p *staticProvider) Identity() string { return p.identity }

func (p *staticProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 1, 1, 1}
	}
	return out, nil
}

func (p *staticProvider) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return []float32{1, 1, 1, 1}, nil
}

func TestTableLifecycle(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	sc, err := st.CreateTable(ctx, "App", "Notes", []Field{
		{Name: "title", Type: String, Fulltext: true, Enum: []string{"a", "b"}, Default: "a"},
		{Name: "body", Type: Text},
		{Name: "score", Type: Number, Required: true},
		{Name: "emb", Type: Vector, Dim: 4},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sc.Namespace != "app" || sc.Name != "notes" || sc.Version != 1 || len(sc.Fields) != 4 {
		t.Fatalf("unexpected schema %+v", sc)
	}
	if sc.Fields[0].Name != "title" || sc.Fields[0].Type != String || !sc.Fields[0].Fulltext {
		t.Fatalf("field must round-trip verbatim, got %+v", sc.Fields[0])
	}
	if len(sc.Fields[0].Enum) != 2 || sc.Fields[0].Enum[1] != "b" || sc.Fields[0].Default != "a" {
		t.Fatalf("enum and default must round-trip, got %+v", sc.Fields[0])
	}
	if !sc.Fields[2].Required || sc.Fields[3].Dim != 4 {
		t.Fatalf("required and dim must round-trip, got %+v %+v", sc.Fields[2], sc.Fields[3])
	}

	tables, err := st.ListTables(ctx, "app")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tables) != 1 || tables[0] != "notes" {
		t.Fatalf("expected [notes], got %v", tables)
	}

	described, count, err := st.DescribeTable(ctx, "app", "notes")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if count != 0 || described.Name != "notes" || described.Version != 1 || len(described.Fields) != 4 {
		t.Fatalf("unexpected description %+v (%d rows)", described, count)
	}

	if err := st.DropTable(ctx, "app", "notes"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	tables, err = st.ListTables(ctx, "app")
	if err != nil {
		t.Fatalf("list after drop: %v", err)
	}
	if len(tables) != 0 {
		t.Fatalf("expected no tables after drop, got %v", tables)
	}
}

func TestCreateTableDuplicateKeepsWireClassification(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "app", "notes", []Field{{Name: "title", Type: String}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = st.CreateTable(ctx, "app", "notes", []Field{{Name: "title", Type: String}})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("duplicate table must stay invalid_request on the wire and facade, got %v", err)
	}
}

func TestDescribeMissingTableIsNotFound(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	_, _, err = st.DescribeTable(context.Background(), "app", "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing table must be not_found, got %v", err)
	}
}

func TestCreateTableVectorizeRequiresProvider(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	_, err = st.CreateTable(ctx, "app", "docs", []Field{{Name: "body", Type: Text, Vectorize: true}})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("vectorize without a provider must be rejected, got %v", err)
	}
	tables, err := st.ListTables(ctx, "app")
	if err != nil || len(tables) != 0 {
		t.Fatalf("a rejected vectorize create must leave no table behind, got %v (%v)", tables, err)
	}
	_, err = st.CreateTable(ctx, "app", "docs", []Field{{Name: "9bad", Type: Text, Vectorize: true}})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("an invalid field must be rejected before the provider message, got %v", err)
	}

	emb, err := Open(t.TempDir(), WithEmbedding(&staticProvider{identity: "fake|v1"}))
	if err != nil {
		t.Fatalf("open with provider: %v", err)
	}
	defer emb.Close()
	sc, err := emb.CreateTable(ctx, "app", "docs", []Field{{Name: "body", Type: Text, Vectorize: true}})
	if err != nil {
		t.Fatalf("vectorize with a provider must succeed, got %v", err)
	}
	if sc.EmbedSpace != "" || sc.EmbedDim != 0 {
		t.Fatalf("embedding identity is pinned at the first vectorized write, not at create, got %+v", sc)
	}

	silent, err := Open(t.TempDir(), WithEmbedding(&staticProvider{identity: ""}))
	if err != nil {
		t.Fatalf("open with silent provider: %v", err)
	}
	defer silent.Close()
	if _, err := silent.CreateTable(ctx, "app", "docs", []Field{{Name: "body", Type: Text, Vectorize: true}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a provider without identity is not usable for vectorize, got %v", err)
	}
}

func TestTableOperationsAfterCloseReturnErrClosed(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "app", "notes", []Field{{Name: "title", Type: String}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := st.CreateTable(ctx, "app", "other", []Field{{Name: "title", Type: String}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("create after close must return ErrClosed, got %v", err)
	}
	if _, err := st.ListTables(ctx, "app"); !errors.Is(err, ErrClosed) {
		t.Fatalf("list after close must return ErrClosed, got %v", err)
	}
	if _, _, err := st.DescribeTable(ctx, "app", "notes"); !errors.Is(err, ErrClosed) {
		t.Fatalf("describe after close must return ErrClosed, got %v", err)
	}
	if err := st.DropTable(ctx, "app", "notes"); !errors.Is(err, ErrClosed) {
		t.Fatalf("drop after close must return ErrClosed, got %v", err)
	}
}

func TestTableOperationsHonorCanceledContexts(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.CreateTable(ctx, "app", "notes", []Field{{Name: "body", Type: Text}}); !errors.Is(err, ErrCanceled) {
		t.Fatalf("create table on a canceled context must classify canceled, got %v", err)
	}
	if _, err := st.ListTables(ctx, "app"); !errors.Is(err, ErrCanceled) {
		t.Fatalf("list tables on a canceled context must classify canceled, got %v", err)
	}
	if _, _, err := st.DescribeTable(ctx, "app", "notes"); !errors.Is(err, ErrCanceled) {
		t.Fatalf("describe table on a canceled context must classify canceled, got %v", err)
	}
	if err := st.DropTable(ctx, "app", "notes"); !errors.Is(err, ErrCanceled) {
		t.Fatalf("drop table on a canceled context must classify canceled, got %v", err)
	}
	live, err := st.ListNamespaces(context.Background(), ListNamespacesOptions{})
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("a canceled call must not leave a namespace behind, got %v", live)
	}
}
