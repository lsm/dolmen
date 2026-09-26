package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/embed"
	"github.com/lsm/dolmen/internal/telemetry"
)

func TestTheServerRecordsStorageSpansUnderItsOperations(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	cfg := &config{DataDir: filepath.Join(t.TempDir(), "data"), MaxOpenNamespaces: 8, Sync: "full", VectorCacheSize: 1 << 20}
	st, err := openStore(cfg, tp)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := api.New(st, mustNoneProvider(t), api.WithTracing(telemetry.New(tp, nil, false)))
	ctx := context.Background()
	for _, c := range []struct{ op, body string }{
		{"create_namespace", `{"namespace":"t"}`},
		{"create_table", `{"namespace":"t","table":"notes","fields":[{"name":"body","type":"string","fulltext":true}]}`},
		{"insert", `{"namespace":"t","table":"notes","records":[{"body":"hello there"}]}`},
		{"search_fulltext", `{"namespace":"t","table":"notes","query":"hello"}`},
	} {
		if _, err := srv.Dispatch(ctx, c.op, []byte(c.body)); err != nil {
			t.Fatalf("%s: %v", c.op, err)
		}
	}
	var names []string
	for _, s := range rec.Ended() {
		names = append(names, s.Name())
		for _, a := range s.Attributes() {
			if strings.Contains(a.Value.Emit(), "hello") {
				t.Errorf("span %s leaks row data in %s", s.Name(), a.Key)
			}
		}
	}
	joined := strings.Join(names, " ")
	for _, want := range []string{"dolmen.op insert", "INSERT", "SELECT"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the server must record %q; recorded %v", want, names)
		}
	}
}

func mustNoneProvider(t *testing.T) embed.Provider {
	t.Helper()
	p, err := embed.NewProvider("none", "", "", "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}
