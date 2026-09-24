package schema

import (
	"bytes"
	"encoding/json"
	"testing"
)

func FuzzDecodeVector(f *testing.F) {
	f.Add(EncodeVector([]float32{1, -2.5, 0}))
	f.Add([]byte{})
	f.Add([]byte{1, 2, 3})
	f.Fuzz(func(t *testing.T, b []byte) {
		v, err := DecodeVector(b)
		if err != nil {
			return
		}
		if !bytes.Equal(EncodeVector(v), b) {
			t.Fatalf("decoded %d bytes into %d floats that do not encode back", len(b), len(v))
		}
	})
}

func FuzzInferSchema(f *testing.F) {
	f.Add(`[{"title":"a","n":1,"tags":["x"],"at":"2026-09-23T10:00:00Z"}]`)
	f.Add(`[{"select":1,"ID":2,"id":3},{"":null}]`)
	f.Add(`[{"v":[0.1,0.2]},{"v":"text"}]`)
	f.Fuzz(func(t *testing.T, raw string) {
		var samples []map[string]any
		dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
		dec.UseNumber()
		if dec.Decode(&samples) != nil || len(samples) == 0 || len(samples) > 50 {
			return
		}
		inf := InferSchema(samples)
		if len(inf.Fields) == 0 {
			return
		}
		if err := Validate(inf.Fields); err != nil {
			t.Fatalf("infer_schema proposed fields create_table would refuse: %v\n%s", err, raw)
		}
	})
}

func FuzzValidateTableName(f *testing.F) {
	for _, s := range []string{"notes", "select", "a__fts", "sqlite_x", "Notes", "", "a-b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if ValidateTableName(name) == nil && !ValidIdentSyntax(name) {
			t.Fatalf("accepted table name %q outside the identifier grammar", name)
		}
	})
}
