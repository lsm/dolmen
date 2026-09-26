package api

import (
	"errors"
	"testing"
)

type decodeTarget struct {
	op  string
	new func() any
}

func decodeTargets() []decodeTarget {
	noFields := func() any { return new(struct{}) }
	grantList := func() any {
		return new(struct {
			Subject *struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"subject"`
			Object *struct {
				Namespace string `json:"namespace"`
				Table     string `json:"table"`
			} `json:"object"`
		})
	}
	return []decodeTarget{
		{op: "list_tables", new: func() any { return new(nsReq) }},
		{op: "list_namespaces", new: func() any { return new(listNamespacesReq) }},
		{op: "create_namespace", new: func() any { return new(nsReq) }},
		{op: "drop_namespace", new: func() any { return new(dropNamespaceReq) }},
		{op: "vacuum", new: func() any { return new(nsReq) }},
		{op: "drop_table", new: func() any { return new(dropTableReq) }},
		{op: "describe_server", new: noFields},
		{op: "capabilities", new: noFields},
		{op: "describe_table", new: func() any { return new(tableReq) }},
		{op: "create_table", new: func() any { return new(createTableReq) }},
		{op: "create_table_auth", new: func() any { return new(createTableAuthReq) }},
		{op: "infer_schema", new: func() any { return new(inferReq) }},
		{op: "insert", new: func() any { return new(insertReq) }},
		{op: "upsert_by_key", new: func() any { return new(upsertReq) }},
		{op: "read_rows", new: func() any { return new(readRowsReq) }},
		{op: "query", new: func() any { return new(queryReq) }},
		{op: "search_fulltext", new: func() any { return new(ftsReq) }},
		{op: "search_vector", new: func() any { return new(vecReq) }},
		{op: "tokenize", new: func() any {
			return new(struct {
				Namespace string `json:"namespace"`
				Table     string `json:"table"`
				Text      string `json:"text"`
			})
		}},
		{op: "changes_since", new: func() any { return new(changesSinceReq) }},
		{op: "wait_for", new: func() any { return new(waitForReq) }},
		{op: "delete", new: func() any { return new(deleteReq) }},
		{op: "update", new: func() any { return new(updateReq) }},
		{op: "upsert", new: func() any { return new(updateReq) }},
		{op: "migrate", new: func() any { return new(migrateReq) }},
		{op: "list_migrations", new: func() any { return new(tableReq) }},
		{op: "rotate_secret_key", new: func() any { return new(rotateReq) }},
		{op: "whoami", new: noFields},
		{op: "grant", new: func() any { return new(grantRequest) }},
		{op: "revoke", new: func() any { return new(grantRequest) }},
		{op: "list_grants", new: grantList},
		{op: "create_key", new: func() any {
			return new(struct {
				Name      string   `json:"name"`
				Principal string   `json:"principal"`
				Groups    []string `json:"groups"`
			})
		}},
		{op: "list_keys", new: noFields},
		{op: "revoke_key", new: func() any {
			return new(struct {
				ID string `json:"id"`
			})
		}},
		{op: "rotate_signing_key", new: func() any {
			return new(struct {
				RetirePrevious bool `json:"retire_previous"`
			})
		}},
	}
}

var decoderCorpus = []string{
	`{"namespace":"app","table":"docs","records":[{"title":"a","body":"b"}]}`,
	`{"namespace":"app","table":"docs","sql":"SELECT id FROM docs WHERE n > ?","args":[1],"limit":10,"offset":0}`,
	`{"namespace":"app","table":"docs","query":"payment","filter":"n = ?","args":[1]}`,
	`{"namespace":"app","table":"docs","vector":[0.1,0.2],"filter":"","args":[]}`,
	`{"namespace":"app","table":"docs","ids":[1,2,3],"reveal":["token"]}`,
	`{"namespace":"app","table":"docs","fields":[{"name":"a","type":"string","default":null}],"row_access":"owner"}`,
	`{"namespace":"app","table":"docs","changes":[{"op":"add_field","field":{"name":"a","type":"string"}}],"expected_version":1,"dry_run":true}`,
	`{"samples":[{"a":1,"b":"x","c":[1,2]},{"a":2,"c":null}]}`,
	`{"namespace":"app","table":"docs","on":["k"],"records":[{"k":1}]}`,
	`{"namespace":"app","table":"docs","where":"n = ?","args":[1],"set":{"n":2},"filter":"n = ?"}`,
	`{"namespace":"app","prefix":"a/","limit":5}`,
	`{"table":"docs","text":"payment"}`,
	`{"namespace":"app","table":"docs","cursor":"abc","timeout_ms":10}`,
	`{"name":"ci","principal":"p","groups":["g"]}`,
	`{"subject":{"type":"principal","id":"p"},"object":{"namespace":"*","table":"*"}}`,
	`{"id":"key-1"}`,
	`{"retire_previous":true}`,
	`{"namespace":"app","confirm":"drop docs"}`,
	`{"namespace":"app"}`,
	`{}`,
	``,
	` `,
	`null`,
	`[1,2]`,
	`"string"`,
	`3`,
	`true`,
	`{"namespace":1}`,
	`{"namespace":"app","table":null}`,
	`{"Namespace":"app"}`,
	`{"namespace":"app","bogus":1}`,
	`{"sql":1,"bogus":2}`,
	`{"namespace":"app","records":"nope"}`,
	`{"namespace":"app","table":"docs","fields":"nope"}`,
	`{"namespace":"app","table":"docs","fields":[{"name":"a","type":true}]}`,
	`{"namespace":"app","sql":`,
	`{"namespace":"app"} trailing`,
	`{"namespace":"app","args":null}`,
	`{"namespace":"app","args":[null,1,"x",{"a":1}]}`,
	`{"namespace":"app","records":[[]]}`,
	`{"namespace":"app","table":"docs","ids":"1"}`,
	`{"namespace":"app","table":"docs","ids":[1.5]}`,
	`{"namespace":"app","table":"docs","offset":-1,"limit":-1}`,
	`{"namespace":"app","expected_incarnation":"x","expected_version":"1"}`,
	`{"namespace":"app","prefix":{}}`,
	`{"namespace":"app","table":"docs","records":[{"body":"\ud800"}]}`,
	`{"namespace":"app","table":"docs","records":[{"body":"é中文"}]}`,
	`{"namespace":"app","table":"docs","records":[{"n":1e400}]}`,
	`{"namespace":"app","table":"docs","records":[{"n":-0.0,"big":123456789012345678901234567890}]}`,
	`{"namespace":"app","table":"docs","vector":[1e308,-1e308,0]}`,
	`{"namespace":"app","table":"docs","changes":[{"op":"add_field","field":null}]}`,
	`{"namespace":"app","table":"docs","changes":[{"op":"bogus"}]}`,
}

func TestEveryOperationHasADecodeTarget(t *testing.T) {
	targets := decodeTargets()
	covered := map[string]bool{}
	for _, target := range targets {
		if covered[target.op] {
			t.Errorf("%s is listed twice in decodeTargets", target.op)
		}
		covered[target.op] = true
	}
	for op := range Ops {
		if !covered[op] {
			t.Errorf("operation %q has no decode target, so the request decoder is not fuzzed for it", op)
		}
	}
	for op := range authOps {
		if !covered[op] {
			t.Errorf("auth operation %q has no decode target, so the request decoder is not fuzzed for it", op)
		}
	}
	if covered["create_table_auth"] && !covered["create_table"] {
		t.Error("create_table must keep both request shapes: the auth-on one and the auth-off one")
	}
}

func FuzzTheRequestDecoder(f *testing.F) {
	for _, body := range decoderCorpus {
		f.Add(body)
	}
	targets := decodeTargets()
	decoders := []struct {
		name string
		fn   func([]byte, any) error
	}{
		{"decode", decode},
		{"decodeData", decodeData},
		{"decodeExactBody", decodeExactBody},
		{"decodeAllowNullArgs", decodeAllowNullArgs},
	}
	f.Fuzz(func(t *testing.T, body string) {
		raw := []byte(body)
		for _, target := range targets {
			for _, decoder := range decoders {
				err := decoder.fn(raw, target.new())
				if err == nil {
					continue
				}
				var apiErr *Error
				if !errors.As(err, &apiErr) {
					t.Fatalf("%s on %s: %v is not a classified api error", decoder.name, target.op, err)
				}
				if apiErr.Status < 400 || apiErr.Status >= 500 {
					t.Fatalf("%s on %s: status %d, want a 4xx: %v", decoder.name, target.op, apiErr.Status, err)
				}
				if apiErr.Code == ErrCodeInternal {
					t.Fatalf("%s on %s: a request body must never be internal_error: %v", decoder.name, target.op, err)
				}
				if apiErr.Status == 0 {
					t.Fatalf("%s on %s: the error carries no HTTP status: %+v", decoder.name, target.op, apiErr)
				}
			}
		}
	})
}
