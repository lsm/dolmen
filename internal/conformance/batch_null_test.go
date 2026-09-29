package conformance

import (
	"strings"
	"testing"
)

func TestBatchOptionalFieldsDecideNull(t *testing.T) {
	h := newHarness(t)
	batchDocs(t, h)
	one := `{"namespace":"acme","writes":[{"kind":"insert","table":"docs","records":[{"title":"a","body":"b"}]}`

	cases := []struct {
		name string
		body string
		want string
	}{
		{"idempotency_key null", one + `,"idempotency_key":null}`, "idempotency_key"},
		{"idempotency_key empty", one + `,"idempotency_key":""}`, "idempotency_key"},
		{"limit null", one + `,"limit":null}`, "limit"},
		{"limit zero", one + `,"limit":0}`, "limit"},
		{"limit negative", one + `,"limit":-3}`, "limit"},
		{"confirm null", one + `,"confirm":null}`, "confirm"},
		{"limit not a number", one + `,"limit":"five"}`, "limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, out := batchError(t, h, tc.body)
			if !strings.Contains(out, tc.want) {
				t.Fatalf("error %v does not name %s", out, tc.want)
			}
		})
	}
}

func TestBatchOptionalFieldsAcceptOmitted(t *testing.T) {
	h := newHarness(t)
	batchDocs(t, h)
	body := `{"namespace":"acme","writes":[
		{"kind":"insert","table":"docs","records":[{"title":"a","body":"b"}]},
		{"kind":"delete","table":"docs","filter":"title = 'a'"}
	]}`
	results := batchResults(t, h, body)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	second, _ := results[1].(map[string]any)
	if matched, _ := second["matched"].(float64); matched != 1 {
		t.Fatalf("delete result = %v, want matched 1", second)
	}
}

func TestBatchPerWriteOptionalFieldsAreRefusedByIndex(t *testing.T) {
	h := newHarness(t)
	batchDocs(t, h)

	cases := []struct {
		name  string
		field string
	}{
		{"idempotency_key", `"idempotency_key":null`},
		{"dry_run", `"dry_run":null`},
		{"limit", `"limit":5`},
		{"confirm", `"confirm":true`},
		{"namespace", `"namespace":"other"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"namespace":"acme","writes":[
				{"kind":"insert","table":"docs","records":[{"title":"a","body":"b"}]},
				{"kind":"delete","table":"docs","filter":"1=1",` + tc.field + `}
			]}`
			_, msg := batchError(t, h, body)
			if !strings.Contains(msg, "writes[1]") {
				t.Fatalf("error %q does not name the second write", msg)
			}
			if !strings.Contains(msg, tc.name) {
				t.Fatalf("error %q does not name the field", msg)
			}
		})
	}
}

func TestBatchNullRecordIsRefusedByIndex(t *testing.T) {
	h := newHarness(t)
	batchDocs(t, h)

	_, msg := batchError(t, h, `{"namespace":"acme","writes":[
		{"kind":"insert","table":"docs","records":[{"title":"a","body":"b"}]},
		{"kind":"insert","table":"docs","records":[{"title":"c","body":"d"},null]}
	]}`)
	if !strings.Contains(msg, "writes[1]") || !strings.Contains(msg, "records[1]") {
		t.Fatalf("error %q does not name the write and the record", msg)
	}
}

func TestBatchErrorTextNamesTopLevelFieldsWithoutAWriteIndex(t *testing.T) {
	h := newHarness(t)
	batchDocs(t, h)

	_, msg := batchError(t, h, `{"namespace":"acme","writes":[{"kind":"insert","table":"docs","records":[{"title":"a"}]}],"limit":0}`)
	if strings.Contains(msg, "writes[") {
		t.Fatalf("a top-level field error carries a write index: %q", msg)
	}
	if !strings.Contains(msg, "limit") {
		t.Fatalf("error %q does not name the top-level field", msg)
	}
}
