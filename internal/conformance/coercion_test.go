package conformance

import (
	"testing"
)

// The typed-read coercion matrix: every declared field type round-trips with
// its documented JSON shape through every typed read path — query,
// search_fulltext, and search_vector — and the three paths cannot drift.
//
//	number: integral → integer, fractional → float
//	boolean: true/false (stored 0/1)
//	json: the decoded value (objects, arrays, nested)
//	vector: number array (stored float32)
//	timestamp: canonicalized string
//	NULL (omitted optional field): null
func TestTypedReadCoercionMatrix(t *testing.T) {
	h := newHarness(t)
	h.seedTable("coerce", "t", []map[string]any{
		{"name": "title", "type": "string", "fulltext": true},
		{"name": "body", "type": "text", "vectorize": true},
		{"name": "n_int", "type": "number"},
		{"name": "n_float", "type": "number"},
		{"name": "n_big", "type": "number"},
		{"name": "flag", "type": "boolean"},
		{"name": "off", "type": "boolean"},
		{"name": "obj", "type": "json"},
		{"name": "arr", "type": "json"},
		{"name": "nest", "type": "json"},
		{"name": "at_time", "type": "timestamp"},
		{"name": "v", "type": "vector", "dim": 4},
		// omitted remains entirely absent → reads as null
		{"name": "absent", "type": "string"},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "coerce", "table": "t",
		"records": []map[string]any{{
			"title":   "payment needle",
			"body":    "payment body",
			"n_int":   42,
			"n_float": 2.5,
			"n_big":   1e17, // integral but beyond float32 precision
			"flag":    true,
			"off":     false,
			"obj":     map[string]any{"k": "v", "n": 1},
			"arr":     []any{1, "two", true, nil},
			"nest":    map[string]any{"a": []any{map[string]any{"b": false}}},
			"at_time": "2026-09-03T10:11:12Z",
			"v":       []any{0.5, -0.25, 0.1, 1},
		}},
	})

	// rowFrom extracts the single row/result from any typed read.
	queryRow := func(sql string) map[string]any {
		data := h.mustHTTP("query", map[string]any{"namespace": "coerce", "sql": sql})
		rows := data["rows"].([]any)
		if len(rows) != 1 {
			t.Fatalf("query %q returned %d rows, want 1", sql, len(rows))
		}
		return rows[0].(map[string]any)
	}
	ftsRow := func() map[string]any {
		data := h.mustHTTP("search_fulltext", map[string]any{
			"namespace": "coerce", "table": "t", "query": "needle",
		})
		results := data["results"].([]any)
		if len(results) != 1 {
			t.Fatalf("fulltext returned %d results, want 1", len(results))
		}
		return results[0].(map[string]any)
	}
	vecRow := func() map[string]any {
		data := h.mustHTTP("search_vector", map[string]any{
			"namespace": "coerce", "table": "t", "column": "v", "vector": []any{0.5, -0.25, 0.1, 1},
		})
		results := data["results"].([]any)
		if len(results) != 1 {
			t.Fatalf("vector returned %d results, want 1", len(results))
		}
		return results[0].(map[string]any)
	}

	q, f, v := queryRow("SELECT * FROM t"), ftsRow(), vecRow()

	// The typed columns must agree across all three read paths.
	typedCols := []string{"n_int", "n_float", "n_big", "flag", "off", "obj", "arr", "nest", "at_time", "v", "absent"}
	for _, col := range typedCols {
		if _, ok := q[col]; !ok {
			t.Fatalf("query row missing typed column %q: %v", col, q)
		}
		assertJSONEqual(t, col+" across query/fulltext", f[col], q[col])
		assertJSONEqual(t, col+" across query/vector", v[col], q[col])
	}

	// The matrix itself, asserted once through the query path.
	assertJSONEqual(t, "integral number", q["n_int"], float64(42))
	assertJSONEqual(t, "integral number stays integer-shaped", int64val(t, "n_int", q["n_int"]), int64(42))
	assertJSONEqual(t, "fractional number", q["n_float"], 2.5)
	assertJSONEqual(t, "large integral number", q["n_big"], float64(1e17))
	assertJSONEqual(t, "true", q["flag"], true)
	assertJSONEqual(t, "false", q["off"], false)
	assertJSONEqual(t, "json object", q["obj"], map[string]any{"k": "v", "n": float64(1)})
	assertJSONEqual(t, "json array keeps null member", q["arr"], []any{float64(1), "two", true, nil})
	assertJSONEqual(t, "nested json", q["nest"], map[string]any{"a": []any{map[string]any{"b": false}}})
	assertJSONEqual(t, "timestamp string", q["at_time"], "2026-09-03T10:11:12Z")

	// vector reads as a number array with float32 storage precision: 0.1
	// comes back as the float64 of the stored float32.
	vec := q["v"].([]any)
	if len(vec) != 4 {
		t.Fatalf("vector length %d, want 4", len(vec))
	}
	if vec[0] != float64(float32(0.5)) || vec[1] != float64(float32(-0.25)) {
		t.Fatalf("vector values %v", vec)
	}
	if vec[2] != float64(float32(0.1)) {
		t.Fatalf("vector 0.1 must round through float32, got %v", vec[2])
	}

	// SQL NULL in an optional field reads as JSON null.
	assertJSONEqual(t, "null field", q["absent"], nil)
	nullRow := queryRow("SELECT absent FROM t")
	assertJSONEqual(t, "sql null", nullRow["absent"], nil)

	// Vector search rows carry _score; fulltext and query rows never do.
	if _, ok := f["_score"]; ok {
		t.Fatal("fulltext results must not carry _score")
	}
	if _, ok := q["_score"]; ok {
		t.Fatal("query rows must not carry _score")
	}
	if _, ok := v["_score"]; !ok {
		t.Fatal("vector results must carry _score")
	}
}

// Aliases coerce by their result-column label; labels that match no declared
// field, or that different tables declare with conflicting types, fall back to
// raw values (blobs as base64).
func TestTypedReadAliasesAndFallbacks(t *testing.T) {
	h := newHarness(t)
	h.seedTable("alias", "a", []map[string]any{
		{"name": "flag", "type": "boolean"},
		{"name": "off", "type": "boolean"},
		{"name": "n", "type": "number"},
	})
	h.seedTable("alias", "b", []map[string]any{
		{"name": "flag", "type": "string"},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "alias", "table": "a", "records": []map[string]any{{"flag": true, "off": false, "n": 7}},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "alias", "table": "b", "records": []map[string]any{{"flag": "yes"}},
	})

	// Coercion keys on the result-column label: aliasing a boolean column to
	// another declared boolean label still coerces; aliasing to an undeclared
	// label falls back to raw (covered below).
	row := h.mustHTTP("query", map[string]any{
		"namespace": "alias", "sql": "SELECT flag AS off FROM a",
	})["rows"].([]any)[0].(map[string]any)
	assertJSONEqual(t, "aliased onto a declared boolean label", row["off"], true)

	// An alias to an undeclared label reads raw even when the source column
	// is declared: the label, not the column, drives coercion.
	row = h.mustHTTP("query", map[string]any{
		"namespace": "alias", "sql": "SELECT flag AS indicator FROM a",
	})["rows"].([]any)[0].(map[string]any)
	assertJSONEqual(t, "alias to undeclared label stays raw", row["indicator"], float64(1))

	// An undeclared label reads raw.
	row = h.mustHTTP("query", map[string]any{
		"namespace": "alias", "sql": "SELECT n * 2 AS doubled FROM a",
	})["rows"].([]any)[0].(map[string]any)
	assertJSONEqual(t, "undeclared expression label", row["doubled"], float64(14))

	// A blob under an undeclared label reads as base64.
	row = h.mustHTTP("query", map[string]any{
		"namespace": "alias", "sql": "SELECT CAST('hello' AS BLOB) AS blobby FROM a",
	})["rows"].([]any)[0].(map[string]any)
	b64, ok := row["blobby"].(string)
	if !ok || b64 != "aGVsbG8=" {
		t.Fatalf("blob under undeclared label must read as base64, got %v", row["blobby"])
	}

	// The same label declared with different types across tables is
	// ambiguous and falls back to raw storage: boolean a.flag reads 1,
	// string b.flag reads "yes".
	rows := h.mustHTTP("query", map[string]any{
		"namespace": "alias", "sql": "SELECT flag FROM a UNION ALL SELECT flag FROM b",
	})["rows"].([]any)
	assertJSONEqual(t, "ambiguous label falls back raw (a)", rows[0].(map[string]any)["flag"], float64(1))
	assertJSONEqual(t, "ambiguous label falls back raw (b)", rows[1].(map[string]any)["flag"], "yes")
}

// The hidden _embedding column: stripped from SELECT * and search results,
// included when referenced explicitly in SQL, and included by searches that
// pass include_hidden.
func TestTypedReadEmbeddingHidden(t *testing.T) {
	h := newHarness(t)
	h.seedTable("hidden", "t", []map[string]any{
		{"name": "body", "type": "text", "vectorize": true},
		{"name": "title", "type": "string", "fulltext": true},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "hidden", "table": "t",
		"records": []map[string]any{{"title": "needle", "body": "embed me"}},
	})

	// Stripped from SELECT *.
	row := h.mustHTTP("query", map[string]any{
		"namespace": "hidden", "sql": "SELECT * FROM t",
	})["rows"].([]any)[0].(map[string]any)
	if _, ok := row["_embedding"]; ok {
		t.Fatalf("_embedding must be stripped from SELECT *: %v", row)
	}

	// Stripped from fulltext and vector search results.
	fts := h.mustHTTP("search_fulltext", map[string]any{
		"namespace": "hidden", "table": "t", "query": "needle",
	})["results"].([]any)[0].(map[string]any)
	if _, ok := fts["_embedding"]; ok {
		t.Fatal("_embedding must be stripped from search_fulltext results")
	}
	sv := h.mustHTTP("search_vector", map[string]any{
		"namespace": "hidden", "table": "t", "text": "embed",
	})["results"].([]any)[0].(map[string]any)
	if _, ok := sv["_embedding"]; ok {
		t.Fatal("_embedding must be stripped from search_vector results")
	}

	// include_hidden keeps it in searches, as a number array. Both search
	// ops forward the flag through separate dispatch paths.
	for _, search := range []struct {
		name string
		op   string
		body map[string]any
	}{
		{"fulltext", "search_fulltext", map[string]any{
			"namespace": "hidden", "table": "t", "query": "needle", "include_hidden": true,
		}},
		{"vector", "search_vector", map[string]any{
			"namespace": "hidden", "table": "t", "text": "embed", "include_hidden": true,
		}},
	} {
		row := h.mustHTTP(search.op, search.body)["results"].([]any)[0].(map[string]any)
		emb, ok := row["_embedding"].([]any)
		if !ok || len(emb) != 8 { // the fake provider embeds 8 dims
			t.Fatalf("include_hidden must return the embedding vector through %s search, got %v", search.name, row["_embedding"])
		}
	}

	// Explicitly referenced in SQL, it is included and typed as a vector.
	row = h.mustHTTP("query", map[string]any{
		"namespace": "hidden", "sql": "SELECT _embedding FROM t",
	})["rows"].([]any)[0].(map[string]any)
	if _, ok := row["_embedding"].([]any); !ok {
		t.Fatalf("explicit _embedding reference must return a number array, got %v", row["_embedding"])
	}
}
