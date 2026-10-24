package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func importArgs(url string, extra ...string) []string {
	return append([]string{"-url", url, "-namespace", "demo", "-table", "events", "-snapshot", "snapshot"}, extra...)
}

func TestImportChunksExactIntegersAndDeterministicKeys(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/import_records" || r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Errorf("request path/header mismatch")
		}
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		fmt.Fprint(w, `{"ok":true,"data":{"inserted":1,"skipped":0,"replayed":false}}`)
	}))
	defer srv.Close()
	input := "{\"id\":9007199254740993,\"created_at\":\"2026-01-02T03:04:05.123Z\",\"body\":\"<escaped>\"}\n{\"id\":9007199254740994,\"created_at\":\"2026-01-02T03:04:05.123Z\"}\n"
	for range 2 {
		var out, errs bytes.Buffer
		err := runImport(importArgs(srv.URL, "-rows", "1"), func(k string) string {
			if k == "DOLMEN_TOKEN" {
				return "synthetic-token"
			}
			return ""
		}, strings.NewReader(input), &out, &errs)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "committed chunk 1") || !strings.Contains(out.String(), "committed chunk 2") {
			t.Fatalf("commits: %s", out.String())
		}
	}
	if len(bodies) != 4 || bodies[0] != bodies[2] || bodies[1] != bodies[3] {
		t.Fatalf("unstable requests: %d", len(bodies))
	}
	if !strings.Contains(bodies[0], `9007199254740993`) || !strings.Contains(bodies[0], `snapshot/events/1`) {
		t.Fatalf("lost identity or key")
	}
}

func TestImportEncodedByteBoundAndRowLimit(t *testing.T) {
	var sizes, rows []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var v struct{ Records []json.RawMessage }
		json.Unmarshal(b, &v)
		sizes = append(sizes, len(b))
		rows = append(rows, len(v.Records))
		fmt.Fprint(w, `{"ok":true,"data":{"inserted":1}}`)
	}))
	defer srv.Close()
	line := `{"id":1,"created_at":"2026-01-02T03:04:05.123Z","body":"<<<<<<"}` + "\n"
	var out, errs bytes.Buffer
	if err := runImport(importArgs(srv.URL, "-bytes", "230"), func(string) string { return "" }, strings.NewReader(strings.Repeat(line, 3)), &out, &errs); err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 3 {
		t.Fatalf("chunks %v", sizes)
	}
	for _, n := range sizes {
		if n > 230 {
			t.Fatalf("encoded request %d > 230", n)
		}
	}
	if err := runImport(importArgs(srv.URL, "-bytes", "230"), func(string) string { return "" }, strings.NewReader(strings.Repeat(" ", 300)+line), &out, &errs); err != nil {
		t.Fatalf("bound encoded request, not insignificant source whitespace: %v", err)
	}
	sizes = nil
	rows = nil
	if err := runImport(importArgs(srv.URL), func(string) string { return "" }, strings.NewReader(strings.Repeat(line, 1001)), &out, &errs); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(rows) != "[1000 1]" {
		t.Fatalf("row chunks %v", rows)
	}
}

func TestImportStopsAtFirstErrorWithoutEchoingValues(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			fmt.Fprint(w, `{"ok":true,"data":{"inserted":1}}`)
			return
		}
		w.WriteHeader(400)
		fmt.Fprint(w, `{"ok":false,"error":{"code":"invalid_request","message":"import record 1: invalid request: field \"body\": value \"NEVER-ECHO\" is invalid"}}`)
	}))
	defer srv.Close()
	var out, errs bytes.Buffer
	line := `{"id":1,"created_at":"2026-01-02T03:04:05.123Z","body":"NEVER-ECHO"}` + "\n"
	err := runImport(importArgs(srv.URL, "-rows", "2"), func(string) string { return "" }, strings.NewReader(strings.Repeat(line, 6)), &out, &errs)
	if err == nil || calls != 2 || !strings.Contains(err.Error(), "line 4") || !strings.Contains(err.Error(), "field body") || !strings.Contains(err.Error(), "invalid_request") {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
	if strings.Contains(fmt.Sprint(err)+errs.String()+out.String(), "NEVER-ECHO") {
		t.Fatal("leaked value")
	}
	if strings.Contains(out.String(), "committed chunk 2") {
		t.Fatal("reported failed commit")
	}
}

func TestImportRejectsMalformedOrOversizedLines(t *testing.T) {
	for _, input := range []string{"", `{"body":"NEVER-ECHO",`, `null`, `[]`, `{} {"body":"NEVER-ECHO"}`, strings.Repeat("x", 300)} {
		var out, errs bytes.Buffer
		err := runImport(importArgs("http://127.0.0.1:1", "-bytes", "230"), func(string) string { return "" }, strings.NewReader(input), &out, &errs)
		if err == nil || !strings.Contains(err.Error(), "line 1") || strings.Contains(err.Error(), "NEVER-ECHO") {
			t.Fatalf("unsafe/missing line error: %v", err)
		}
	}
}

func TestImportDoesNotFollowRedirectOrExposeCredentials(t *testing.T) {
	followed := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	var out, errs bytes.Buffer
	err := runImport(importArgs(source.URL), func(string) string { return "NEVER-ECHO" }, strings.NewReader(`{"id":1}`), &out, &errs)
	if err == nil || followed || strings.Contains(err.Error()+errs.String()+out.String(), "NEVER-ECHO") {
		t.Fatalf("redirect/credential handling: followed=%v err=%v", followed, err)
	}
	for _, args := range [][]string{{"-token", "NEVER-ECHO"}, {"-url", "http://NEVER-ECHO@localhost"}, {"-rows", "1001"}, {"-bytes", "33554433"}} {
		err := runImport(args, func(string) string { return "" }, strings.NewReader("{}"), &out, &errs)
		if err == nil || strings.Contains(err.Error()+errs.String()+out.String(), "NEVER-ECHO") {
			t.Fatalf("unsafe option error: %v", err)
		}
	}
}
