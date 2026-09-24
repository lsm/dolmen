package api

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

func TestDebugLogsOneLinePerOperationWithoutPayloads(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := New(st, fakeEmb{})
	ctx := WithRequestID(context.Background(), "req-123")
	if _, err := s.Dispatch(ctx, "create_namespace", []byte(`{"namespace":"secretns"}`)); err != nil {
		t.Fatal(err)
	}
	s.Dispatch(ctx, "query", []byte(`{"namespace":"secretns","sql":"SELECT 'top-secret-literal' FROM nope"}`))
	out := buf.String()
	for _, want := range []string{"op=create_namespace outcome=ok status=200", "op=query outcome=", "request_id=req-123"} {
		if !strings.Contains(out, want) {
			t.Fatalf("access log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "top-secret-literal") || strings.Contains(out, "SELECT") {
		t.Fatalf("the access log must not carry request payloads:\n%s", out)
	}
}

func TestInfoLevelLogsNoAccessLines(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	New(st, fakeEmb{}).Dispatch(context.Background(), "list_namespaces", []byte(`{}`))
	if strings.Contains(buf.String(), "op=list_namespaces") {
		t.Fatalf("access lines are debug-only:\n%s", buf.String())
	}
}
