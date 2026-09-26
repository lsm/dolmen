package postgres

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func spanAttr(s sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			v, _ := kv.Value.AsInterface().(string)
			return v
		}
	}
	return ""
}

func spanFor(t *testing.T, spans []sdktrace.ReadOnlySpan, name, namespace string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() != name || spanAttr(s, "db.namespace") != namespace {
			continue
		}
		if found != nil {
			t.Fatalf("more than one %q span for namespace %s", name, namespace)
		}
		found = s
	}
	if found == nil {
		t.Fatalf("no %q span for namespace %s", name, namespace)
	}
	return found
}

func wantSpanAttrs(t *testing.T, s sdktrace.ReadOnlySpan, parent trace.Span, want map[string]string) {
	t.Helper()
	if s.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Errorf("%s is not a child of the caller's span", s.Name())
	}
	if s.SpanKind() != trace.SpanKindClient {
		t.Errorf("%s kind = %v, want client", s.Name(), s.SpanKind())
	}
	for k, v := range want {
		if got := spanAttr(s, k); got != v {
			t.Errorf("%s: %s = %q, want %q", s.Name(), k, got, v)
		}
	}
}

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

func TestPostgresRecordsSpansForReadsAndSchemaLifecycle(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	cfg := testConfig(t)
	cfg.TracerProvider = tp
	s := openTest(t, cfg)
	ctx, parent := tp.Tracer("test").Start(context.Background(), "parent")
	if err := s.CreateNamespace(ctx, "life", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "life", "notes", []schema.Field{{Name: "n", Type: schema.Number}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "life", "notes", []map[string]any{{"n": 1}, {"n": 2}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNamespace(ctx, "errs", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "errs", "notes", []schema.Field{{Name: "n", Type: schema.Number}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	rec.Reset()
	if _, err := s.GetRows(ctx, "life", "notes", []int64{1}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(ctx, "life", "SELECT n FROM notes WHERE n > ?", []any{0}, [16]byte{}, store.Page{Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "life", "logs", []schema.Field{{Name: "msg", Type: schema.Text}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DropTable(ctx, "life", "notes", store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNamespace(ctx, "fresh", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DropNamespace(ctx, "fresh", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DropNamespace(ctx, "life", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRows(ctx, "errs", "gone", []int64{1}, nil, store.Incarnation{}); err == nil {
		t.Fatal("read_rows from a missing table succeeded")
	}
	if _, err := s.Query(ctx, "errs", "SELECT nosuchcol FROM notes", nil, [16]byte{}, store.Page{Limit: 5}); err == nil {
		t.Fatal("query on a missing column succeeded")
	}
	parent.End()

	spans := rec.Ended()
	rows := spanFor(t, spans, "SELECT notes", "life")
	wantSpanAttrs(t, rows, parent, map[string]string{
		"db.system.name": "postgresql", "db.operation.name": "SELECT", "db.collection.name": "notes",
	})
	created := spanFor(t, spans, "CREATE logs", "life")
	wantSpanAttrs(t, created, parent, map[string]string{
		"db.system.name": "postgresql", "db.operation.name": "CREATE", "db.collection.name": "logs",
	})
	dropped := spanFor(t, spans, "DROP notes", "life")
	wantSpanAttrs(t, dropped, parent, map[string]string{
		"db.system.name": "postgresql", "db.operation.name": "DROP", "db.collection.name": "notes",
	})
	if got := spanAttr(spanFor(t, spans, "CREATE", "fresh"), "db.collection.name"); got != "" {
		t.Errorf("create_namespace named a table: %q", got)
	}
	for _, ns := range []string{"fresh", "life"} {
		if got := spanAttr(spanFor(t, spans, "DROP", ns), "db.collection.name"); got != "" {
			t.Errorf("drop_namespace in %s named a table: %q", ns, got)
		}
	}
	for _, ns := range []string{"life", "errs"} {
		q := spanFor(t, spans, "SELECT", ns)
		wantSpanAttrs(t, q, parent, map[string]string{
			"db.system.name": "postgresql", "db.operation.name": "SELECT", "db.namespace": ns,
		})
		if got := spanAttr(q, "db.collection.name"); got != "" {
			t.Errorf("query in %s named a table: %q", ns, got)
		}
	}
	if q := spanFor(t, spans, "SELECT", "life"); q.Status().Code != codes.Unset {
		t.Errorf("a successful query span is %v", q.Status())
	}
	if rows := spanFor(t, spans, "SELECT gone", "errs"); rows.Status().Code != codes.Error || spanAttr(rows, "error.type") != "not_found" {
		t.Errorf("read_rows status = %v, error.type = %q", rows.Status(), spanAttr(rows, "error.type"))
	}
	if q := spanFor(t, spans, "SELECT", "errs"); q.Status().Code != codes.Error || spanAttr(q, "error.type") != "query_error" {
		t.Errorf("query status = %v, error.type = %q", q.Status(), spanAttr(q, "error.type"))
	}
	for _, sp := range spans {
		for _, a := range sp.Attributes() {
			v, _ := a.Value.AsInterface().(string)
			if strings.Contains(v, "n > ?") || strings.Contains(v, "nosuchcol") {
				t.Errorf("span %s leaks the query text in %s=%q", sp.Name(), a.Key, v)
			}
		}
		if strings.Contains(sp.Status().Description, "nosuchcol") {
			t.Errorf("span %s status leaks the error message", sp.Name())
		}
	}
}
