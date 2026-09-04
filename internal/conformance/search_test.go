package conformance

import (
	"math"
	"testing"
)

// FTS5 syntax invariants: the documented accept/reject set from the README's
// full-text search section. Accepted expressions return ok; rejected ones
// fail with the fts5 syntax error class.
func TestSearchFulltextSyntaxAcceptReject(t *testing.T) {
	h := newHarness(t)
	h.seedTable("fts", "t", []map[string]any{
		{"name": "title", "type": "string", "fulltext": true},
		{"name": "body", "type": "text", "fulltext": true},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "fts", "table": "t",
		"records": []map[string]any{
			{"title": "payment gateway", "body": "card charged twice"},
			{"title": "refund issued", "body": "payment was refunded"},
			{"title": "can't login", "body": "session token expired"},
		},
	})

	accept := map[string]string{
		"single token":           "payment",
		"implicit AND":           "payment gateway",
		"explicit OR":            "payment OR refund",
		"NOT":                    "payment NOT refund",
		"field-scoped":           "title:payment",
		"field group":            "{title body}:payment",
		"phrase":                 `"payment gateway"`,
		"quoted punctuation":     `"can't"`,
		"quoted hyphenated term": `"foo-bar"`,
		"prefix":                 "pay*",
		"NEAR group":             "NEAR(payment refund)",
		"uppercase keywords":     "payment AND refund",
		"case-insensitive match": "PAYMENT",
		"diacritic-insensitive":  "cafe",
	}
	for name, q := range accept {
		t.Run("accept: "+name, func(t *testing.T) {
			data := h.mustHTTP("search_fulltext", map[string]any{
				"namespace": "fts", "table": "t", "query": q,
			})
			if data["truncated"] == nil {
				t.Fatalf("response must carry truncated: %v", data)
			}
		})
	}

	// "cafe" must actually match a stored diacritic (café) row.
	h.mustHTTP("insert", map[string]any{
		"namespace": "fts", "table": "t",
		"records": []map[string]any{{"title": "café latte", "body": "diacritics"}},
	})
	data := h.mustHTTP("search_fulltext", map[string]any{
		"namespace": "fts", "table": "t", "query": "cafe",
	})
	if len(data["results"].([]any)) != 1 {
		t.Fatalf("diacritic-insensitive match: %v", data["results"])
	}

	reject := map[string]string{
		"bare hyphenated term":  "foo-bar",
		"bare apostrophe":       "can't",
		"trailing operator":     "payment AND",
		"leading operator":      "OR payment",
		"unbalanced phrase":     `"payment`,
		"bare single quotes":    "'payment'",
		"unknown field scoping": "nocol:payment",
		"empty-ish":             " ",
	}
	for name, q := range reject {
		t.Run("reject: "+name, func(t *testing.T) {
			status, body := h.httpCall("search_fulltext", map[string]any{
				"namespace": "fts", "table": "t", "query": q,
			})
			if status != 400 {
				t.Fatalf("status %d, want 400: %v", status, body)
			}
			errObj := envelopeOf(t, body)
			if errObj["code"] != "invalid_request" {
				t.Fatalf("code %v, want invalid_request: %v", errObj["code"], errObj)
			}
		})
	}

	// Ranking: BM25 relevance with stable id tie-breaking; more relevant
	// documents (more matches in the indexed fields) come first.
	data = h.mustHTTP("search_fulltext", map[string]any{
		"namespace": "fts", "table": "t", "query": "payment OR refund",
	})
	results := data["results"].([]any)
	if len(results) < 2 {
		t.Fatalf("expected matches, got %v", results)
	}
	// "refund issued"/"payment was refunded" both contain refund; the row
	// containing both tokens ranks first.
	first := results[0].(map[string]any)
	if first["title"] != "refund issued" && first["body"] != "payment was refunded" {
		t.Fatalf("dual-token row must rank first, got %v", first)
	}
	// Deterministic ordering: same query twice returns the same order.
	again := h.mustHTTP("search_fulltext", map[string]any{
		"namespace": "fts", "table": "t", "query": "payment OR refund",
	})
	assertJSONEqual(t, "stable ordering", again["results"], data["results"])
}

// Vector-search invariants: _score equals the locally computed cosine of the
// query against each stored vector (float32 math), ordering is by descending
// score with stable id tie-breaking, and identical vectors score 1.
func TestSearchVectorScoreIsLocalCosine(t *testing.T) {
	h := newHarness(t)
	h.seedTable("vec", "t", []map[string]any{
		{"name": "name", "type": "string"},
		{"name": "v", "type": "vector", "dim": 4},
	})
	vectors := map[string][]float32{
		"same":     {1, 0, 0, 0},
		"half":     {1, 1, 0, 0},
		"ortho":    {0, 0, 1, 0},
		"opposite": {-1, 0, 0, 0},
	}
	recs := make([]map[string]any, 0, len(vectors))
	for name, v := range vectors {
		vv := v // copy for the closure-free literal below
		arr := make([]any, len(vv))
		for i, x := range vv {
			arr[i] = float64(x)
		}
		recs = append(recs, map[string]any{"name": name, "v": arr})
	}
	h.mustHTTP("insert", map[string]any{"namespace": "vec", "table": "t", "records": recs})

	query := []any{1.0, 0.0, 0.0, 0.0}
	data := h.mustHTTP("search_vector", map[string]any{
		"namespace": "vec", "table": "t", "column": "v", "vector": query,
	})
	results := data["results"].([]any)
	if len(results) != 4 {
		t.Fatalf("all four rows score, got %d: %v", len(results), results)
	}
	if int64val(t, "skipped", data["skipped_vectors"]) != 0 {
		t.Fatalf("skipped_vectors %v, want 0", data["skipped_vectors"])
	}

	cosine := func(a, b []float32) float64 {
		var dot, na, nb float64
		for i := range a {
			dot += float64(a[i]) * float64(b[i])
			na += float64(a[i]) * float64(a[i])
			nb += float64(b[i]) * float64(b[i])
		}
		return dot / (math.Sqrt(na) * math.Sqrt(nb))
	}

	prev := math.Inf(1)
	prevID := int64(-1)
	byName := map[string]float64{}
	for _, r := range results {
		row := r.(map[string]any)
		score := float(t, "_score", row["_score"])
		id := int64val(t, "id", row["id"])
		name := row["name"].(string)
		byName[name] = score

		want := cosine([]float32{1, 0, 0, 0}, vectors[name])
		if math.Abs(score-want) > 1e-6 {
			t.Fatalf("%s _score %v, want local cosine %v", name, score, want)
		}
		// Descending score, then ascending id on ties.
		if score > prev+1e-12 || (score == prev && id < prevID) {
			t.Fatalf("ordering violated at %s: score %v after %v", name, score, prev)
		}
		prev, prevID = score, id
	}
	if math.Abs(byName["same"]-1.0) > 1e-6 {
		t.Fatalf("identical vector must score 1, got %v", byName["same"])
	}
	if math.Abs(byName["opposite"]-(-1.0)) > 1e-6 {
		t.Fatalf("opposite vector must score -1, got %v", byName["opposite"])
	}
	if math.Abs(byName["ortho"]) > 1e-6 {
		t.Fatalf("orthogonal vector must score 0, got %v", byName["ortho"])
	}

	// min_score drops lower-similarity rows before ranking and limit.
	data = h.mustHTTP("search_vector", map[string]any{
		"namespace": "vec", "table": "t", "column": "v", "vector": query, "min_score": 0.5,
	})
	names := []string{}
	for _, r := range data["results"].([]any) {
		names = append(names, r.(map[string]any)["name"].(string))
	}
	if len(names) != 2 { // same (1.0) and half (~0.707)
		t.Fatalf("min_score 0.5 must keep 2 rows, got %v", names)
	}

	// offset/limit page deterministically: limit 2 offset 2 is the tail.
	data = h.mustHTTP("search_vector", map[string]any{
		"namespace": "vec", "table": "t", "column": "v", "vector": query, "limit": 2, "offset": 2,
	})
	tail := data["results"].([]any)
	if len(tail) != 2 {
		t.Fatalf("offset page: %v", tail)
	}
	if tail[0].(map[string]any)["name"] != "ortho" {
		t.Fatalf("offset page order: %v", tail)
	}
	if data["truncated"] != false {
		t.Fatalf("exhausted page must not be truncated: %v", data["truncated"])
	}
}

// skipped_vectors: stored vectors corrupted by an out-of-band writer (wrong
// storage type or wrong shape) are skipped and counted, never silently
// dropped and never fatal.
func TestSearchVectorSkippedVectors(t *testing.T) {
	h := newHarness(t)
	h.seedTable("vec", "s", []map[string]any{
		{"name": "name", "type": "string"},
		{"name": "v", "type": "vector", "dim": 2},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "vec", "table": "s",
		"records": []map[string]any{
			{"name": "good", "v": []any{1, 0}},
			{"name": "text-corrupt", "v": []any{0, 1}},
			{"name": "short-blob", "v": []any{1, 1}},
		},
	})

	// Out-of-band writer corrupts two stored vectors: one becomes TEXT, one
	// becomes a BLOB of the wrong shape (odd byte length).
	rows := h.mustHTTP("query", map[string]any{
		"namespace": "vec", "sql": "SELECT id, name FROM s ORDER BY id",
	})["rows"].([]any)
	idOf := func(name string) int64 {
		for _, r := range rows {
			row := r.(map[string]any)
			if row["name"] == name {
				return int64val(t, "id", row["id"])
			}
		}
		t.Fatalf("row %s not found", name)
		return 0
	}
	h.outOfBand("vec", func(db *sqlDB) error {
		if _, err := db.Exec("UPDATE s SET v = 'not a blob' WHERE id = ?", idOf("text-corrupt")); err != nil {
			return err
		}
		_, err := db.Exec("UPDATE s SET v = x'0102' WHERE id = ?", idOf("short-blob")) // odd, invalid shape
		return err
	})

	data := h.mustHTTP("search_vector", map[string]any{
		"namespace": "vec", "table": "s", "column": "v", "vector": []any{1, 0},
	})
	results := data["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("only the healthy row scores, got %v", results)
	}
	if results[0].(map[string]any)["name"] != "good" {
		t.Fatalf("wrong survivor: %v", results[0])
	}
	if int64val(t, "skipped", data["skipped_vectors"]) != 2 {
		t.Fatalf("skipped_vectors %v, want 2", data["skipped_vectors"])
	}
}

// NULL-embedding exclusion: rows whose vectorize field was null, empty, or
// absent have a NULL _embedding and are excluded from text vector search —
// they are not results and not skipped.
func TestSearchVectorNullEmbeddingExclusion(t *testing.T) {
	h := newHarness(t)
	h.seedTable("vecz", "t", []map[string]any{
		{"name": "body", "type": "text", "vectorize": true},
		{"name": "tag", "type": "string"},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "vecz", "table": "t",
		"records": []map[string]any{
			{"body": "embedded text one", "tag": "a"},
			{"body": "", "tag": "b"}, // empty string → not embedded
			{"tag": "c"},             // absent → not embedded
		},
	})

	data := h.mustHTTP("search_vector", map[string]any{
		"namespace": "vecz", "table": "t", "text": "embedded text one",
	})
	results := data["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("only the embedded row is searchable, got %v", results)
	}
	if results[0].(map[string]any)["tag"] != "a" {
		t.Fatalf("wrong row returned: %v", results[0])
	}
	if int64val(t, "skipped", data["skipped_vectors"]) != 0 {
		t.Fatalf("NULL embeddings are excluded, not skipped: %v", data["skipped_vectors"])
	}
	if data["truncated"] != false {
		t.Fatalf("single result page is not truncated: %v", data["truncated"])
	}
	// The row_count still reports all three rows.
	desc := h.mustHTTP("describe_table", map[string]any{"namespace": "vecz", "table": "t"})
	if int64val(t, "row count", desc["row_count"]) != 3 {
		t.Fatalf("all rows stored: %v", desc["row_count"])
	}
}

// search_fulltext filter/args (#120): the optional SQL WHERE filter applies
// before ranking with the same semantics as search_vector's, and its failure
// classes match the established filter-error contract.
func TestSearchFulltextFilterArgs(t *testing.T) {
	h := newHarness(t)
	h.seedTable("ftsf", "t", []map[string]any{
		{"name": "title", "type": "string", "fulltext": true},
		{"name": "tag", "type": "string"},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "ftsf", "table": "t",
		"records": []map[string]any{
			{"title": "payment one", "tag": "a"},
			{"title": "payment two", "tag": "b"},
		},
	})

	// The filter narrows the match set before ranking; args bind values.
	data := h.mustHTTP("search_fulltext", map[string]any{
		"namespace": "ftsf", "table": "t", "query": "payment",
		"filter": "tag = ?", "args": []any{"a"},
	})
	results := data["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["tag"] != "a" {
		t.Fatalf("filter must narrow to the tagged row, got %v", results)
	}

	// Failure classes: statement-level rejections are invalid_request;
	// execution failures are query_error with WHERE-expression guidance.
	status, body := h.httpCall("search_fulltext", map[string]any{
		"namespace": "ftsf", "table": "t", "query": "payment", "filter": "tag = 'a'; DROP",
	})
	if status != 400 {
		t.Fatalf("semicolon filter: status %d, want 400: %v", status, body)
	}
	errObj := envelopeOf(t, body)
	if errObj["code"] != "invalid_request" {
		t.Fatalf("semicolon filter code %v, want invalid_request", errObj["code"])
	}
	wantMessage(t, "semicolon filter", errObj["message"].(string), `multiple statements are not allowed in filter`)

	status, body = h.httpCall("search_fulltext", map[string]any{
		"namespace": "ftsf", "table": "t", "query": "payment", "filter": "tag =",
	})
	if status != 400 {
		t.Fatalf("malformed filter: status %d, want 400: %v", status, body)
	}
	errObj = envelopeOf(t, body)
	if errObj["code"] != "query_error" {
		t.Fatalf("malformed filter code %v, want query_error", errObj["code"])
	}
	wantMessage(t, "malformed filter", errObj["message"].(string), `single SQL WHERE expression`)
}

// Search pagination contract: truncated is true exactly when more results
// exist beyond the returned page.
func TestSearchTruncatedContract(t *testing.T) {
	h := newHarness(t)
	h.seedTable("trunc", "t", []map[string]any{{"name": "title", "type": "string", "fulltext": true}})
	recs := make([]map[string]any, 25)
	for i := range recs {
		recs[i] = map[string]any{"title": "shared token"}
	}
	h.mustHTTP("insert", map[string]any{"namespace": "trunc", "table": "t", "records": recs})

	cases := []struct {
		limit, offset int
		wantLen       int
		wantTruncated bool
	}{
		{10, 0, 10, true},   // default page of 25
		{10, 5, 10, true},   // middle page, more remain
		{10, 15, 10, false}, // 15+10 = 25 exactly exhausts the set
		{10, 20, 5, false},  // tail page is not truncated
		{25, 0, 25, false},  // whole set in one page
		{30, 0, 25, false},  // limit above the match count
		{10, 25, 0, false},  // past the end
	}
	for _, c := range cases {
		data := h.mustHTTP("search_fulltext", map[string]any{
			"namespace": "trunc", "table": "t", "query": "token",
			"limit": c.limit, "offset": c.offset,
		})
		if got := len(data["results"].([]any)); got != c.wantLen {
			t.Fatalf("limit %d offset %d: %d results, want %d", c.limit, c.offset, got, c.wantLen)
		}
		if data["truncated"] != c.wantTruncated {
			t.Fatalf("limit %d offset %d: truncated %v, want %v", c.limit, c.offset, data["truncated"], c.wantTruncated)
		}
	}
}
