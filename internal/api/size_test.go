package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

func TestAWritePastTheNamespaceSizeLimitAnswers507(t *testing.T) {
	st, err := store.Open(t.TempDir(), store.WithMaxNamespaceSize(512<<10))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := httptest.NewServer(New(st, fakeEmb{}).Handler())
	defer srv.Close()
	post(t, srv.URL, "create_table", map[string]any{"namespace": "cap", "table": "t", "fields": []map[string]any{{"name": "body", "type": "text"}}})
	chunk := strings.Repeat("x", 32<<10)
	for i := 0; i < 64; i++ {
		status, out := post(t, srv.URL, "insert", map[string]any{"namespace": "cap", "table": "t", "records": []map[string]any{{"body": chunk}}})
		if status == 200 {
			continue
		}
		errObj, _ := out["error"].(map[string]any)
		msg, _ := errObj["message"].(string)
		if status != 507 || !strings.Contains(msg, "-max-namespace-size") {
			t.Fatalf("got %d %v", status, errObj)
		}
		return
	}
	t.Fatal("the size limit never refused a write")
}
