package store

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/lsm/dolmen/internal/telemetry/dbspan"

	"github.com/lsm/dolmen/internal/schema"
)

type startHook struct {
	sdktrace.SpanProcessor
	mu     sync.Mutex
	onName map[string]chan struct{}
}

func (h *startHook) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	h.mu.Lock()
	if ch, ok := h.onName[s.Name()]; ok {
		delete(h.onName, s.Name())
		close(ch)
	}
	h.mu.Unlock()
	h.SpanProcessor.OnStart(ctx, s)
}

func (h *startHook) when(name string) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan struct{})
	h.onName[name] = ch
	return ch
}

type traced struct {
	st   *Store
	rec  *tracetest.SpanRecorder
	hook *startHook
	tp   *sdktrace.TracerProvider
}

func openTraced(t *testing.T) traced {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	hook := &startHook{SpanProcessor: rec, onName: map[string]chan struct{}{}}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(hook))
	st, err := Open(t.TempDir(), WithTracerProvider(tp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "test", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTable(ctx, "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "alpha secret-title", "body": "first body", "score": 1, "emb": []any{1.0, 0.0, 0.0, 0.0}},
		{"title": "beta", "body": "second body", "score": 2, "emb": []any{0.0, 1.0, 0.0, 0.0}},
		{"title": "gamma", "body": "third body", "score": 3, "emb": []any{0.0, 0.0, 1.0, 0.0}},
	}, WriteOpts{}, testEmbed, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	rec.Reset()
	return traced{st: st, rec: rec, hook: hook, tp: tp}
}

func (tr traced) parent() (context.Context, trace.Span) {
	return tr.tp.Tracer("test").Start(context.Background(), "parent")
}

func spanAttrOf(s sdktrace.ReadOnlySpan, key string) any {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsInterface()
		}
	}
	return nil
}

func childrenOf(spans []sdktrace.ReadOnlySpan, parent sdktrace.ReadOnlySpan) []string {
	var names []string
	for _, s := range spans {
		if s.Parent().SpanID() == parent.SpanContext().SpanID() {
			names = append(names, s.Name())
		}
	}
	return names
}

func only(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name {
			if found != nil {
				t.Fatalf("more than one %q span", name)
			}
			found = s
		}
	}
	if found == nil {
		var names []string
		for _, s := range spans {
			names = append(names, s.Name())
		}
		t.Fatalf("no %q span among %v", name, names)
	}
	return found
}

func wantDBAttrs(t *testing.T, s sdktrace.ReadOnlySpan, op string) {
	t.Helper()
	wantDBAttrsFor(t, s, op, "notes")
}

func wantDBAttrsFor(t *testing.T, s sdktrace.ReadOnlySpan, op, table string) {
	t.Helper()
	want := map[string]any{"db.system.name": "sqlite", "db.operation.name": op, "db.namespace": "test", "db.collection.name": table}
	for k, v := range want {
		if got := spanAttrOf(s, k); got != v {
			t.Errorf("%s: %s = %v, want %v", s.Name(), k, got, v)
		}
	}
}

func wantDBAttrsForNS(t *testing.T, s sdktrace.ReadOnlySpan, op, namespace string) {
	t.Helper()
	want := map[string]any{"db.system.name": "sqlite", "db.operation.name": op, "db.namespace": namespace}
	for k, v := range want {
		if got := spanAttrOf(s, k); got != v {
			t.Errorf("%s: %s = %v, want %v", s.Name(), k, got, v)
		}
	}
	if got := spanAttrOf(s, "db.collection.name"); got != nil {
		t.Errorf("%s: db.collection.name = %v, want none", s.Name(), got)
	}
}

func noSpanCarries(t *testing.T, spans []sdktrace.ReadOnlySpan, needles ...string) {
	t.Helper()
	for _, s := range spans {
		for _, kv := range s.Attributes() {
			v, _ := kv.Value.AsInterface().(string)
			for _, needle := range needles {
				if strings.Contains(v, needle) {
					t.Errorf("%s leaks %q in %s = %q", s.Name(), needle, kv.Key, v)
				}
			}
		}
		if strings.Contains(s.Status().Description, needles[0]) {
			t.Errorf("%s status description leaks %q: %s", s.Name(), needles[0], s.Status().Description)
		}
	}
}

func underParent(t *testing.T, s sdktrace.ReadOnlySpan, parent trace.Span) {
	t.Helper()
	if s.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Fatalf("%s is not a child of the caller's span", s.Name())
	}
}

func TestStorageSpanInsert(t *testing.T) {
	tr := openTraced(t)
	ctx, p := tr.parent()
	if _, err := tr.st.Insert(ctx, "test", "notes", []map[string]any{{"title": "delta", "body": "fourth"}}, WriteOpts{}, testEmbed, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	p.End()
	spans := tr.rec.Ended()
	ins := only(t, spans, "INSERT notes")
	underParent(t, ins, p)
	wantDBAttrs(t, ins, "INSERT")
	if got := strings.Join(childrenOf(spans, ins), ","); got != "dolmen.writer.wait,dolmen.writer.wait,dolmen.transaction" {
		t.Fatalf("INSERT children = %s", got)
	}
	if tx := only(t, spans, "dolmen.transaction"); spanAttrOf(tx, "dolmen.tx.outcome") != "commit" {
		t.Fatalf("transaction outcome = %v", spanAttrOf(tx, "dolmen.tx.outcome"))
	}
}

func TestStorageSpanFilteredVectorSearch(t *testing.T) {
	tr := openTraced(t)
	for i, wantCache := range []string{"build", "hit"} {
		tr.rec.Reset()
		ctx, p := tr.parent()
		if _, err := tr.st.SearchVector(ctx, "test", "notes", VectorQuery{Column: "emb", Vec: []float32{1, 0, 0, 0}, Filter: "score >= ?", Args: []any{2}}, false, nil, Incarnation{}, Page{Limit: 5}); err != nil {
			t.Fatal(err)
		}
		p.End()
		spans := tr.rec.Ended()
		sel := only(t, spans, "SELECT notes")
		underParent(t, sel, p)
		wantDBAttrs(t, sel, "SELECT")
		if spanAttrOf(sel, "dolmen.search.kind") != "vector" {
			t.Fatalf("search kind = %v", spanAttrOf(sel, "dolmen.search.kind"))
		}
		if got := strings.Join(childrenOf(spans, sel), ","); got != "dolmen.vector.cache,dolmen.vector.score" {
			t.Fatalf("search %d children = %s", i, got)
		}
		if got := spanAttrOf(only(t, spans, "dolmen.vector.cache"), "dolmen.vector.cache"); got != wantCache {
			t.Fatalf("search %d cache = %v, want %s", i, got, wantCache)
		}
		score := only(t, spans, "dolmen.vector.score")
		if spanAttrOf(score, "dolmen.vector.candidates") != int64(2) || spanAttrOf(score, "dolmen.vector.rows_scored") != int64(2) {
			t.Fatalf("score attrs = %v", score.Attributes())
		}
	}
}

func TestStorageSpanVectorSearchCacheTooBig(t *testing.T) {
	tr := openTraced(t)
	tr.st.vcache.max = 1
	ctx, p := tr.parent()
	if _, err := tr.st.SearchVector(ctx, "test", "notes", VectorQuery{Column: "emb", Vec: []float32{1, 0, 0, 0}}, false, nil, Incarnation{}, Page{Limit: 5}); err != nil {
		t.Fatal(err)
	}
	p.End()
	spans := tr.rec.Ended()
	if got := spanAttrOf(only(t, spans, "dolmen.vector.cache"), "dolmen.vector.cache"); got != "too_big" {
		t.Fatalf("cache = %v", got)
	}
	score := only(t, spans, "dolmen.vector.score")
	if spanAttrOf(score, "dolmen.vector.rows_scored") != int64(3) || spanAttrOf(score, "dolmen.vector.candidates") != nil {
		t.Fatalf("score attrs = %v", score.Attributes())
	}
}

func TestStorageSpanFulltextSearch(t *testing.T) {
	tr := openTraced(t)
	ctx, p := tr.parent()
	if _, err := tr.st.SearchFulltext(ctx, "test", "notes", "alpha", "", nil, false, nil, Incarnation{}, Page{Limit: 5}); err != nil {
		t.Fatal(err)
	}
	p.End()
	sel := only(t, tr.rec.Ended(), "SELECT notes")
	underParent(t, sel, p)
	wantDBAttrs(t, sel, "SELECT")
	if spanAttrOf(sel, "dolmen.search.kind") != "fulltext" {
		t.Fatalf("search kind = %v", spanAttrOf(sel, "dolmen.search.kind"))
	}
}

func TestStorageSpanReadRows(t *testing.T) {
	tr := openTraced(t)
	ctx, p := tr.parent()
	if _, err := tr.st.GetRows(ctx, "test", "notes", []int64{1, 3}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	p.End()
	spans := tr.rec.Ended()
	sel := only(t, spans, "SELECT notes")
	underParent(t, sel, p)
	wantDBAttrs(t, sel, "SELECT")
	noSpanCarries(t, spans, "first body", "third body")
}

func TestStorageSpanQuery(t *testing.T) {
	tr := openTraced(t)
	const sqlText = "SELECT title FROM notes WHERE score >= ?"
	ctx, p := tr.parent()
	if _, err := tr.st.Query(ctx, "test", sqlText, []any{2}, [16]byte{}, Page{Limit: 5}); err != nil {
		t.Fatal(err)
	}
	p.End()
	spans := tr.rec.Ended()
	sel := only(t, spans, "SELECT")
	underParent(t, sel, p)
	wantDBAttrsForNS(t, sel, "SELECT", "test")
	noSpanCarries(t, spans, "score >=", "notes", "beta")
}

func TestStorageSpanTableLifecycle(t *testing.T) {
	tr := openTraced(t)
	ctx, p := tr.parent()
	if _, err := tr.st.CreateTable(ctx, "test", "logs", []schema.Field{{Name: "msg", Type: schema.Text}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if err := tr.st.DropTable(ctx, "test", "notes", Incarnation{}); err != nil {
		t.Fatal(err)
	}
	p.End()
	spans := tr.rec.Ended()
	cr := only(t, spans, "CREATE logs")
	underParent(t, cr, p)
	wantDBAttrsFor(t, cr, "CREATE", "logs")
	dr := only(t, spans, "DROP notes")
	underParent(t, dr, p)
	wantDBAttrsFor(t, dr, "DROP", "notes")
}

func TestStorageSpanNamespaceLifecycle(t *testing.T) {
	tr := openTraced(t)
	ctx, p := tr.parent()
	if err := tr.st.CreateNamespace(ctx, "fresh", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if err := tr.st.DropNamespace(ctx, "fresh", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	p.End()
	spans := tr.rec.Ended()
	cr := only(t, spans, "CREATE")
	underParent(t, cr, p)
	wantDBAttrsForNS(t, cr, "CREATE", "fresh")
	dr := only(t, spans, "DROP")
	underParent(t, dr, p)
	wantDBAttrsForNS(t, dr, "DROP", "fresh")
}

func TestStorageSpanMigrate(t *testing.T) {
	tr := openTraced(t)
	ctx, p := tr.parent()
	if _, err := tr.st.Migrate(ctx, "test", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "extra", Type: schema.String}},
		{Op: schema.OpDropField, Name: "tags"},
	}, testEmbed, Incarnation{Version: 1}); err != nil {
		t.Fatal(err)
	}
	p.End()
	spans := tr.rec.Ended()
	mig := only(t, spans, "MIGRATE notes")
	underParent(t, mig, p)
	wantDBAttrs(t, mig, "MIGRATE")
	children := childrenOf(spans, mig)
	if strings.Join(children, ",") != "dolmen.writer.wait,dolmen.transaction" {
		t.Fatalf("MIGRATE children = %v", children)
	}
	tx := only(t, spans, "dolmen.transaction")
	steps := childrenOf(spans, tx)
	if len(steps) < 2 {
		t.Fatalf("transaction children = %v, want one span per migration step", steps)
	}
	for _, s := range spans {
		if s.Name() == "dolmen.migrate.step" && spanAttrOf(s, "dolmen.migrate.step.kind") == nil {
			t.Fatalf("step without kind: %v", s.Attributes())
		}
	}
}

func TestStorageSpanVacuum(t *testing.T) {
	tr := openTraced(t)
	ctx, p := tr.parent()
	if _, err := tr.st.Vacuum(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	p.End()
	spans := tr.rec.Ended()
	v := only(t, spans, "VACUUM")
	underParent(t, v, p)
	if spanAttrOf(v, "db.namespace") != "test" || spanAttrOf(v, "db.operation.name") != "VACUUM" {
		t.Fatalf("vacuum attrs = %v", v.Attributes())
	}
	if got := strings.Join(childrenOf(spans, v), ","); got != "dolmen.writer.wait" {
		t.Fatalf("VACUUM children = %s", got)
	}
}

func TestStorageSpanWriterWaitUnderContention(t *testing.T) {
	tr := openTraced(t)
	n, err := tr.st.ns("test")
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()
	holder, err := n.rw.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	waiting := tr.hook.when("dolmen.writer.wait")
	done := make(chan error, 1)
	go func() {
		_, err := tr.st.Insert(context.Background(), "test", "notes", []map[string]any{{"title": "blocked"}}, WriteOpts{}, testEmbed, nil, Incarnation{})
		done <- err
	}()
	<-waiting
	const held = 50 * time.Millisecond
	time.Sleep(held)
	holder.Rollback()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var longest time.Duration
	for _, s := range tr.rec.Ended() {
		if s.Name() == "dolmen.writer.wait" {
			longest = max(longest, s.EndTime().Sub(s.StartTime()))
		}
	}
	if d := longest; d < held {
		t.Fatalf("writer wait lasted %v while the writer was held for %v", d, held)
	}
}

func TestStorageSpanErrorStatus(t *testing.T) {
	tr := openTraced(t)
	ctx, p := tr.parent()
	if _, err := tr.st.Insert(ctx, "test", "missing", []map[string]any{{"title": "x"}}, WriteOpts{}, testEmbed, nil, Incarnation{}); err == nil {
		t.Fatal("insert into a missing table succeeded")
	}
	if _, err := tr.st.SearchFulltext(ctx, "test", "notes", "alpha", "nosuchcol = ?", []any{"secret-arg"}, false, nil, Incarnation{}, Page{Limit: 5}); err == nil {
		t.Fatal("search with a bad filter succeeded")
	}
	if _, err := tr.st.GetRows(ctx, "test", "missing", []int64{1}, nil, Incarnation{}); err == nil {
		t.Fatal("read_rows from a missing table succeeded")
	}
	if _, err := tr.st.Query(ctx, "test", "SELECT nosuchcol FROM notes WHERE title = ?", []any{"secret-arg"}, [16]byte{}, Page{Limit: 5}); err == nil {
		t.Fatal("query on a missing column succeeded")
	}
	if _, err := tr.st.CreateTable(ctx, "test", "notes", []schema.Field{{Name: "x", Type: schema.Text}}, TableOpts{}, [16]byte{}); err == nil {
		t.Fatal("create_table over an existing table succeeded")
	}
	if err := tr.st.DropNamespace(ctx, "nope", [16]byte{}); err == nil {
		t.Fatal("drop_namespace on a missing namespace succeeded")
	}
	p.End()
	spans := tr.rec.Ended()
	ins := only(t, spans, "INSERT missing")
	if ins.Status().Code != codes.Error || spanAttrOf(ins, "error.type") != "not_found" {
		t.Fatalf("INSERT status = %v, error.type = %v", ins.Status(), spanAttrOf(ins, "error.type"))
	}
	sel := only(t, spans, "SELECT notes")
	if sel.Status().Code != codes.Error || spanAttrOf(sel, "error.type") != "query_error" {
		t.Fatalf("SELECT status = %v, error.type = %v", sel.Status(), spanAttrOf(sel, "error.type"))
	}
	rows := only(t, spans, "SELECT missing")
	if rows.Status().Code != codes.Error || spanAttrOf(rows, "error.type") != "not_found" {
		t.Fatalf("read_rows status = %v, error.type = %v", rows.Status(), spanAttrOf(rows, "error.type"))
	}
	query := only(t, spans, "SELECT")
	if query.Status().Code != codes.Error || spanAttrOf(query, "error.type") != "query_error" {
		t.Fatalf("query status = %v, error.type = %v", query.Status(), spanAttrOf(query, "error.type"))
	}
	cr := only(t, spans, "CREATE notes")
	if cr.Status().Code != codes.Error || spanAttrOf(cr, "error.type") != "invalid_request" {
		t.Fatalf("create_table status = %v, error.type = %v", cr.Status(), spanAttrOf(cr, "error.type"))
	}
	dr := only(t, spans, "DROP")
	if dr.Status().Code != codes.Error || spanAttrOf(dr, "error.type") != "not_found" {
		t.Fatalf("drop_namespace status = %v, error.type = %v", dr.Status(), spanAttrOf(dr, "error.type"))
	}
	if got := spanAttrOf(dr, "db.namespace"); got != "nope" {
		t.Fatalf("drop_namespace db.namespace = %v", got)
	}
	for _, s := range spans {
		if strings.Contains(s.Status().Description, "nosuchcol") {
			t.Fatalf("%s status leaks the error message", s.Name())
		}
		for _, kv := range s.Attributes() {
			if v := kv.Value.Emit(); strings.Contains(v, "secret") || strings.Contains(v, "nosuchcol") {
				t.Fatalf("%s leaks user data in %s", s.Name(), kv.Key)
			}
		}
	}
}

func TestStorageSpansOffByDefault(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.tr.On() {
		t.Fatal("tracing must be off without a provider")
	}
}

func TestTheTransactionSpanRecordsWhatHappenedToTheTransaction(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	st, err := Open(t.TempDir(), WithTracerProvider(tp))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mustNS(t, legacy(st), "tx")
	ctx := context.Background()
	n, err := st.ns("tx")
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()

	_, tx, w, err := st.beginWrite(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	st.endWrite(tx, w)

	_, tx, w, err = st.beginWrite(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitWrite(tx, w); err != nil {
		t.Fatal(err)
	}
	st.endWrite(tx, w)

	_, tx, w, err = st.beginWrite(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	st.endWrite(tx, w)

	var outcomes []string
	for _, s := range rec.Ended() {
		if s.Name() != dbspan.Transaction {
			continue
		}
		for _, a := range s.Attributes() {
			if string(a.Key) == string(dbspan.TxOutcomeKey) {
				outcomes = append(outcomes, a.Value.Emit())
			}
		}
	}
	if strings.Join(outcomes, ",") != "rollback,commit,rollback" {
		t.Fatalf("an explicit rollback, a commit and an abandoned transaction must be traced as rollback, commit, rollback; traced %v", outcomes)
	}
}

func TestEveryCommittedWriteIsTracedAsACommit(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	st, err := Open(t.TempDir(), WithTracerProvider(tp))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := legacy(st)
	mustNS(t, l, "cw")
	ctx := context.Background()
	if _, err := l.CreateTable(ctx, "cw", "t", []schema.Field{{Name: "k", Type: schema.Number}}); err != nil {
		t.Fatal(err)
	}
	rec.Reset()
	if _, err := l.Insert(ctx, "cw", "t", []map[string]any{{"k": 1}, {"k": 2}}, testEmbed); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Update(ctx, "cw", "t", "k = ?", []any{1}, map[string]any{"k": 3}, testEmbed); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := l.UpsertByKey(ctx, "cw", "t", []string{"k"}, []map[string]any{{"k": 9}}, testEmbed); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Delete(ctx, "cw", "t", "k = ?", []any{2}, DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Migrate(ctx, "cw", "t", []schema.Change{{Op: schema.OpAddField, Field: &schema.Field{Name: "note", Type: schema.String}}}, testEmbed, 1); err != nil {
		t.Fatal(err)
	}
	var outcomes []string
	for _, s := range rec.Ended() {
		if s.Name() != dbspan.Transaction {
			continue
		}
		for _, a := range s.Attributes() {
			if string(a.Key) == string(dbspan.TxOutcomeKey) {
				outcomes = append(outcomes, a.Value.Emit())
			}
		}
	}
	if len(outcomes) < 5 {
		t.Fatalf("want a transaction span per write, got %v", outcomes)
	}
	for _, o := range outcomes {
		if o != "commit" {
			t.Fatalf("a successful write must be traced as a commit; traced %v", outcomes)
		}
	}
}
