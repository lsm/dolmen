package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func scrape(t *testing.T, base string) string {
	t.Helper()
	res, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("metrics answered %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestMetricsCountOperationsByOutcomeWithoutLeakingNames(t *testing.T) {
	srv := newTestServer(t)
	post(t, srv.URL, "create_namespace", map[string]any{"namespace": "secretns"})
	post(t, srv.URL, "create_table", map[string]any{"namespace": "secretns", "table": "secrettable", "fields": []map[string]any{{"name": "n", "type": "number"}}})
	post(t, srv.URL, "insert", map[string]any{"namespace": "secretns", "table": "secrettable", "records": []map[string]any{{"n": 1}}})
	post(t, srv.URL, "read_rows", map[string]any{"namespace": "secretns", "table": "missingtable"})
	post(t, srv.URL, "no_such_op_secret", map[string]any{})
	out := scrape(t, srv.URL)
	for _, want := range []string{
		`dolmen_operations_total{op="insert",outcome="ok"} 1`,
		`dolmen_operations_total{op="read_rows",outcome="invalid_request"} 1`,
		`dolmen_operation_duration_seconds_count{op="insert"} 1`,
		`dolmen_operation_duration_seconds_bucket{op="insert",le="+Inf"} 1`,
		"dolmen_operations_in_flight 0",
		"dolmen_subscriptions_active 0",
		"dolmen_build_info{version=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{"secret", "missingtable", "no_such_op"} {
		if strings.Contains(out, leak) {
			t.Errorf("metrics leak %q:\n%s", leak, out)
		}
	}
}

func TestMetricsRefuseWrites(t *testing.T) {
	srv := newTestServer(t)
	res, err := http.Post(srv.URL+"/metrics", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /metrics answered %d", res.StatusCode)
	}
}
