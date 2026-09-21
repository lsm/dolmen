package conformance

import (
	"net/http"
	"testing"
)

var sharedFilterList = []struct {
	function string
	filter   string
}{
	{"abs", "abs(id) = id"},
	{"round", "round(id) = id"},
	{"length", "length(body) >= 0"},
	{"lower", "lower(body) = lower(body)"},
	{"upper", "upper(body) = upper(body)"},
	{"substr", "substr(body, 1, 2) = substr(body, 1, 2)"},
	{"trim", "trim(body) = trim(body)"},
	{"trim with a strip set", "trim(body, 'ab') = trim(body, 'ab')"},
	{"ltrim", "ltrim(body, 'ab') = ltrim(body, 'ab')"},
	{"rtrim", "rtrim(body, 'ab') = rtrim(body, 'ab')"},
	{"replace", "replace(body, 'a', 'b') = replace(body, 'a', 'b')"},
	{"instr", "instr(body, 'a') >= 0"},
	{"coalesce", "coalesce(body, '') = coalesce(body, '')"},
	{"ifnull", "ifnull(body, '') = ifnull(body, '')"},
	{"nullif", "nullif(body, '') IS NOT NULL"},
	{"iif", "iif(id > 0, 1, 0) = 1"},
	{"date", "date(created_at) = date(created_at)"},
	{"time", "time(created_at) = time(created_at)"},
	{"datetime", "datetime(created_at) = datetime(created_at)"},
	{"julianday", "julianday(created_at) > 0"},
	{"strftime", "strftime('%Y', created_at) = strftime('%Y', created_at)"},
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

func TestEveryEngineEvaluatesTheSharedFilterList(t *testing.T) {
	for _, tc := range sharedFilterList {
		t.Run(tc.function, func(t *testing.T) {
			h := seedScopedFilterRow(t)
			body := `{"namespace":"acme","table":"notes","filter":"` + tc.filter + `","dry_run":true}`
			res, out := h.asIdentity(t, "alice", "", "delete", body)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("the API layer accepts %s from the shared allowlist, so the engine owes it an evaluation rather than a refusal: %d %v", tc.function, res.StatusCode, out)
			}
		})
	}
}
