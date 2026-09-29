package conformance

import (
	"strings"
	"testing"
)

func seedBatchGrantHarness(t *testing.T) *harness {
	t.Helper()
	h := seedGrantHarness(t)
	h.seedTable("acme", "ledger", []map[string]any{{"name": "title", "type": "string"}})
	return h
}

func batchErrMessage(t *testing.T, out map[string]any) string {
	t.Helper()
	errEnv, _ := out["error"].(map[string]any)
	if errEnv == nil {
		t.Fatalf("no error envelope in %v", out)
	}
	msg, _ := errEnv["message"].(string)
	return msg
}

func TestBatchAuthorizesEachWriteAgainstItsOwnTable(t *testing.T) {
	h := seedBatchGrantHarness(t)
	grantTo(t, h, "principal", "alice", "acme", "docs", "create")
	grantTo(t, h, "principal", "alice", "acme", "ledger", "create")

	res, out := h.asAlice(t, "batch", `{"namespace":"acme","writes":[
		{"kind":"insert","table":"docs","records":[{"title":"a"}]},
		{"kind":"delete","table":"ledger","filter":"1=1"}
	]}`)
	assertForbiddenEnvelope(t, "a delete with no delete grant", res, out)
	msg := batchErrMessage(t, out)
	if !strings.Contains(msg, "writes[1]") {
		t.Fatalf("error %q does not name the refused write by index", msg)
	}
	if !strings.Contains(msg, "no grant") {
		t.Fatalf("error %q does not carry the grant remediation", msg)
	}
}

func TestBatchRefusesTheWholeThingBeforeWritingAnything(t *testing.T) {
	h := seedBatchGrantHarness(t)
	grantTo(t, h, "principal", "alice", "acme", "docs", "create")
	grantTo(t, h, "principal", "alice", "acme", "ledger", "create")

	res, out := h.asAlice(t, "batch", `{"namespace":"acme","writes":[
		{"kind":"insert","table":"docs","records":[{"title":"a"}]},
		{"kind":"insert","table":"ledger","records":[{"title":"b"}]},
		{"kind":"delete","table":"docs","filter":"1=1"}
	]}`)
	assertForbiddenEnvelope(t, "a batch with an unauthorized final write", res, out)

	rows := h.mustHTTP("query", map[string]any{"namespace": "acme", "sql": "SELECT count(*) AS n FROM docs"})
	list, _ := rows["rows"].([]any)
	if len(list) != 1 {
		t.Fatalf("count query returned %v", list)
	}
	row, _ := list[0].(map[string]any)
	if n, _ := row["n"].(float64); n != 0 {
		t.Fatalf("a refused batch left %d rows, so a check ran after a write", int(n))
	}
}

func TestBatchRefusesAnUnknownKindNamingTheIndex(t *testing.T) {
	h := seedBatchGrantHarness(t)
	grantTo(t, h, "principal", "alice", "acme", "docs", "create")

	res, out := h.asAlice(t, "batch", `{"namespace":"acme","writes":[
		{"kind":"insert","table":"docs","records":[{"title":"a"}]},
		{"kind":"grant","table":"docs"}
	]}`)
	if res.StatusCode == 200 {
		t.Fatalf("an unknown write kind was accepted: %v", out)
	}
	if msg := batchErrMessage(t, out); !strings.Contains(msg, "writes[1]") {
		t.Fatalf("error %q does not name the write carrying the unknown kind", msg)
	}
}

func TestBatchRefusesAWriteWithNoTable(t *testing.T) {
	h := seedBatchGrantHarness(t)
	grantTo(t, h, "principal", "alice", "acme", "", "create")

	res, out := h.asAlice(t, "batch", `{"namespace":"acme","writes":[
		{"kind":"insert","records":[{"title":"a"}]}
	]}`)
	if res.StatusCode == 200 {
		t.Fatalf("a write with no table was accepted: %v", out)
	}
	msg := batchErrMessage(t, out)
	if !strings.Contains(msg, "writes[0]") || !strings.Contains(msg, "table") {
		t.Fatalf("error %q does not name the write and the missing field", msg)
	}
}

func TestBatchIsNotAuthorizedByTheNamespaceAlone(t *testing.T) {
	h := seedBatchGrantHarness(t)
	grantTo(t, h, "principal", "alice", "acme", "", "create")
	grantTo(t, h, "principal", "alice", "acme", "docs", "create")

	res, out := h.asAlice(t, "batch", `{"namespace":"acme","writes":[
		{"kind":"insert","table":"ledger","records":[{"title":"a"}]}
	]}`)
	assertForbiddenEnvelope(t, "a namespace-wide create grant used on a table the caller has no grant for", res, out)
}
