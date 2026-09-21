package conformance

import (
	"net/http"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

var sharedFilterList = []struct {
	function string
	filter   string
}{
	{"abs", "abs(id) = 1"},
	{"round", "round(id) = 1"},
	{"length", "length(body) = 17"},
	{"lower", "lower(body) = 'a note from alice'"},
	{"upper", "upper(body) = 'A NOTE FROM ALICE'"},
	{"substr", "substr(body, 3, 4) = 'note'"},
	{"trim", "trim(body) = body"},
	{"trim with a strip set", "trim(body, 'ae') = ' note from alic'"},
	{"ltrim", "ltrim(body, 'a') = ' note from alice'"},
	{"rtrim", "rtrim(body, 'e') = 'a note from alic'"},
	{"replace", "replace(body, 'alice', 'bob') = 'a note from bob'"},
	{"instr", "instr(body, 'note') = 3"},
	{"coalesce", "coalesce(body, 'x') = body"},
	{"ifnull", "ifnull(body, 'x') = body"},
	{"nullif", "nullif(body, 'x') = body"},
	{"iif", "iif(id = 1, 'y', 'n') = 'y'"},
	{"date", "length(date(created_at)) = 10"},
	{"time", "length(time(created_at)) = 8"},
	{"datetime", "length(datetime(created_at)) = 19"},
	{"julianday", "julianday(created_at) > 2400000"},
	{"strftime", "length(strftime('%Y', created_at)) = 4"},
}

var pinnedFilterSemantics = []struct {
	rule   string
	filter string
}{
	{"LIKE is ASCII-case-insensitive", "body LIKE 'A NOTE%'"},
	{"string comparison is BINARY byte-wise", "'a' > 'B'"},
	{"integer division truncates toward zero", "-7 / 2 = -3"},
	{"round goes half away from zero", "round(-2.5) = -3"},
}

var notYetEvaluatedByAdapterTwo = map[string]bool{
	"instr": true, "ifnull": true, "iif": true,
	"date": true, "time": true, "datetime": true, "julianday": true, "strftime": true,
}

var notYetPinnedByAdapterTwo = map[string]bool{
	"LIKE is ASCII-case-insensitive":        true,
	"string comparison is BINARY byte-wise": true,
}

func seedScopedFilterRow(t *testing.T) *harness {
	t.Helper()
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create", "delete")
	res, out := h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"a note from alice"}]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("seed insert: status %d %v", res.StatusCode, out)
	}
	return h
}

func mustMatchTheOwnRow(t *testing.T, filter string) {
	t.Helper()
	h := seedScopedFilterRow(t)
	res, out := h.asIdentity(t, "alice", "", "delete",
		`{"namespace":"acme","table":"notes","filter":"`+filter+`","dry_run":true}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the API layer accepted this filter from the shared allowlist, so the engine owes it an evaluation rather than a refusal: %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if matched, _ := data["matched"].(float64); matched != 1 {
		t.Fatalf("the filter is true of the caller's one row, so it must match it: matched %v: %v", data["matched"], out)
	}
}

func TestEveryEngineEvaluatesTheSharedFilterList(t *testing.T) {
	for _, tc := range sharedFilterList {
		t.Run(tc.function, func(t *testing.T) {
			if testEngine(t) == store.EnginePostgres && notYetEvaluatedByAdapterTwo[tc.function] {
				t.Skipf("adapter #2 validates %s through the shared allowlist but has no evaluation for it; see #388", tc.function)
			}
			mustMatchTheOwnRow(t, tc.filter)
		})
	}
}

func TestEveryEngineEvaluatesAScopedFilterWithSQLitesSemantics(t *testing.T) {
	for _, tc := range pinnedFilterSemantics {
		t.Run(tc.rule, func(t *testing.T) {
			if testEngine(t) == store.EnginePostgres && notYetPinnedByAdapterTwo[tc.rule] {
				t.Skipf("adapter #2 answers this filter with its own semantics and returns a different row set rather than an error; see #388")
			}
			mustMatchTheOwnRow(t, tc.filter)
		})
	}
}
