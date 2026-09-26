package postgres

import (
	"context"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestPostgresRecordsASpanPerStorageOperation(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	cfg := testConfig(t)
	cfg.TracerProvider = tp
	s := openTest(t, cfg)
	ctx := context.Background()
	if err := s.CreateNamespace(ctx, "tr", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "tr", "notes", []schema.Field{
		{Name: "body", Type: schema.Text, Fulltext: true},
		{Name: "n", Type: schema.Number},
		{Name: "emb", Type: schema.Vector, Dim: 3},
	}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "tr", "notes", []map[string]any{{"body": "private words", "n": 1, "emb": []float32{1, 0, 0}}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, "tr", "notes", "n = ?", []any{1}, map[string]any{"n": 2}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SearchFulltext(ctx, "tr", "notes", "private", "", nil, false, nil, store.Incarnation{}, store.Page{Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SearchVector(ctx, "tr", "notes", store.VectorQuery{Column: "emb", Vec: []float32{1, 0, 0}}, false, nil, store.Incarnation{}, store.Page{Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete(ctx, "tr", "notes", "n = ?", []any{2}, store.DeleteOpts{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, sp := range rec.Ended() {
		names = append(names, sp.Name())
		attrs := map[string]string{}
		for _, a := range sp.Attributes() {
			attrs[string(a.Key)] = a.Value.Emit()
			if strings.Contains(a.Value.Emit(), "private words") || strings.Contains(a.Value.Emit(), "n = ?") {
				t.Errorf("span %s leaks %s=%s", sp.Name(), a.Key, a.Value.Emit())
			}
		}
		if attrs["db.system.name"] != "" && attrs["db.system.name"] != "postgresql" {
			t.Errorf("span %s has db.system.name=%s", sp.Name(), attrs["db.system.name"])
		}
	}
	joined := strings.Join(names, " ")
	for _, want := range []string{"INSERT notes", "UPDATE notes", "SELECT notes", "DELETE notes"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the postgres engine must record %q; recorded %v", want, names)
		}
	}
}
