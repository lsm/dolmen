package conformance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/embed"
	"github.com/lsm/dolmen/internal/mcp"
	"github.com/lsm/dolmen/internal/store"
)

var (
	stdioBinOnce sync.Once
	stdioBinPath string
	stdioBinErr  error
)

func dolmenBinary(t *testing.T) string {
	t.Helper()
	stdioBinOnce.Do(func() {
		root := filepath.Join("..", "..")
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
			stdioBinErr = fmt.Errorf("repo root not found at %s: %w", root, err)
			return
		}
		dir, err := os.MkdirTemp("", "dolmen-stdio-bin-")
		if err != nil {
			stdioBinErr = err
			return
		}
		stdioBinPath = filepath.Join(dir, "dolmen")
		build := exec.Command("go", "build", "-o", stdioBinPath, ".")
		build.Dir = root
		if out, err := build.CombinedOutput(); err != nil {
			stdioBinErr = fmt.Errorf("go build: %v: %s", err, out)
		}
	})
	if stdioBinErr != nil {
		t.Fatalf("build dolmen binary: %v", stdioBinErr)
	}
	return stdioBinPath
}

type stdioProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string
	stderr struct {
		mu  sync.Mutex
		buf bytes.Buffer
	}
}

func stdioEnv() []string {
	out := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "DOLMEN_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "DOLMEN_EMBED_PROVIDER=none")
}

func startStdio(t *testing.T, args ...string) *stdioProc {
	t.Helper()
	full := append([]string{"mcp", "-data", t.TempDir()}, args...)
	cmd := exec.Command(dolmenBinary(t), full...)
	cmd.Env = stdioEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dolmen mcp: %v", err)
	}
	p := &stdioProc{t: t, cmd: cmd, stdin: stdin, lines: make(chan string, 1024)}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 40<<20)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
		close(p.lines)
	}()
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			p.stderr.mu.Lock()
			p.stderr.buf.WriteString(sc.Text() + "\n")
			p.stderr.mu.Unlock()
		}
	}()
	t.Cleanup(p.cleanup)
	return p
}

func (p *stdioProc) stderrText() string {
	p.stderr.mu.Lock()
	defer p.stderr.mu.Unlock()
	return p.stderr.buf.String()
}

func (p *stdioProc) cleanup() {
	_ = p.stdin.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.cmd.Wait()
}

func (p *stdioProc) waitEOF() error {
	p.t.Helper()
	if err := p.stdin.Close(); err != nil {
		p.t.Fatalf("close stdin: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
		p.t.Fatalf("stdin EOF: dolmen mcp did not exit within 15s; stderr: %s", p.stderrText())
		return nil
	}
}

func (p *stdioProc) send(msg any) {
	p.t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		p.t.Fatalf("marshal rpc: %v", err)
	}
	p.sendLine(string(raw))
}

func (p *stdioProc) sendLine(line string) {
	p.t.Helper()
	if _, err := p.stdin.Write([]byte(line + "\n")); err != nil {
		p.t.Fatalf("write stdin: %v", err)
	}
}

func (p *stdioProc) recv() map[string]any {
	p.t.Helper()
	select {
	case line, ok := <-p.lines:
		if !ok {
			p.t.Fatalf("stdout closed before the expected response; stderr: %s", p.stderrText())
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			p.t.Fatalf("stdout line is not JSON (stdout must carry protocol only): %q; stderr: %s", line, p.stderrText())
		}
		return m
	case <-time.After(20 * time.Second):
		p.t.Fatalf("no stdio response within 20s; stderr: %s", p.stderrText())
		return nil
	}
}

func (p *stdioProc) rpc(id, method string, params any) map[string]any {
	p.t.Helper()
	p.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	res := p.recv()
	if res["id"] != id {
		p.t.Fatalf("response id %v, want %q (a response for another request or notification leaked)", res["id"], id)
	}
	if res["jsonrpc"] != "2.0" {
		p.t.Fatalf("response jsonrpc %v, want 2.0", res["jsonrpc"])
	}
	return res
}

func (p *stdioProc) rpcErr(id, method string, params any) map[string]any {
	p.t.Helper()
	res := p.rpc(id, method, params)
	e, ok := res["error"].(map[string]any)
	if !ok {
		p.t.Fatalf("%s: expected a JSON-RPC error, got %v", method, res)
	}
	return e
}

func (p *stdioProc) call(id, op string, args any) map[string]any {
	p.t.Helper()
	return p.rpc(id, "tools/call", map[string]any{"name": op, "arguments": args})
}

func (p *stdioProc) callData(id, op string, args any) map[string]any {
	p.t.Helper()
	res := p.call(id, op, args)
	result, ok := res["result"].(map[string]any)
	if !ok {
		p.t.Fatalf("tools/call %s: no result object: %v", op, res)
	}
	if result["isError"] == true {
		p.t.Fatalf("tools/call %s failed: %v", op, result)
	}
	sc, ok := result["structuredContent"].(map[string]any)
	if !ok {
		p.t.Fatalf("tools/call %s: no structuredContent: %v", op, result)
	}
	return sc
}

func (p *stdioProc) callError(id, op string, args any) map[string]any {
	p.t.Helper()
	res := p.call(id, op, args)
	result, ok := res["result"].(map[string]any)
	if !ok || result["isError"] != true {
		p.t.Fatalf("tools/call %s: expected a tool error, got %v", op, res)
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		p.t.Fatalf("tools/call %s: tool error carries no text content: %v", op, result)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	var env map[string]any
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		p.t.Fatalf("tools/call %s: tool error text is not the standard envelope: %q", op, text)
	}
	return env
}

func stdioInitializeParams() map[string]any {
	return map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "conformance", "version": "1.0"},
	}
}

func TestStdioInitializeHandshake(t *testing.T) {
	p := startStdio(t)
	res := p.rpc("h-init", "initialize", stdioInitializeParams())
	result := res["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion %v, want 2025-06-18", result["protocolVersion"])
	}
	info := result["serverInfo"].(map[string]any)
	if info["name"] != "dolmen" {
		t.Fatalf("serverInfo.name %v, want dolmen", info["name"])
	}
	if instr, _ := result["instructions"].(string); instr == "" {
		t.Fatal("initialize must carry instructions")
	}
	p.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	ping := p.rpc("h-ping", "ping", map[string]any{})
	if pingRes, ok := ping["result"].(map[string]any); !ok || len(pingRes) != 0 {
		t.Fatalf("ping result must be an empty object, got %v", ping)
	}

	h := newHarness(t)
	httpRes := h.rpc(map[string]any{"jsonrpc": "2.0", "id": 9001, "method": "initialize", "params": stdioInitializeParams()})
	httpInit := httpRes.result
	for _, key := range []string{"protocolVersion", "capabilities", "serverInfo"} {
		assertJSONEqual(t, "initialize "+key+" across transports", result[key], httpInit[key])
	}
}

func TestStdioToolsListMatchesHTTP(t *testing.T) {
	h := newHarness(t)
	httpRes := h.rpc(map[string]any{"jsonrpc": "2.0", "id": 9002, "method": "tools/list"})
	if httpRes.result == nil {
		t.Fatalf("HTTP tools/list failed: %+v", httpRes)
	}
	p := startStdio(t)
	stdioRes := p.rpc("t-list", "tools/list", map[string]any{})
	assertJSONEqual(t, "tools/list across transports", stdioRes["result"], httpRes.result)

	stdioResult := stdioRes["result"].(map[string]any)
	tools := stdioResult["tools"].([]any)
	if len(tools) != len(api.OpNames()) {
		t.Fatalf("stdio tools/list exposes %d tools, want %d", len(tools), len(api.OpNames()))
	}
	names := map[string]bool{}
	for _, tool := range tools {
		def := tool.(map[string]any)
		name := def["name"].(string)
		names[name] = true
		if _, ok := def["description"].(string); !ok {
			t.Fatalf("tool %s carries no description", name)
		}
		if _, ok := def["inputSchema"].(map[string]any); !ok {
			t.Fatalf("tool %s carries no inputSchema", name)
		}
	}
	for _, name := range api.OpNames() {
		if !names[name] {
			t.Fatalf("stdio tools/list is missing tool %q", name)
		}
	}
}

func TestStdioInsertQuerySearchRoundTrip(t *testing.T) {
	p := startStdio(t)
	p.callData("rt-ns", "create_namespace", map[string]any{"namespace": "rt"})
	p.callData("rt-table", "create_table", map[string]any{
		"namespace": "rt", "table": "docs",
		"fields": []map[string]any{
			{"name": "title", "type": "string", "fulltext": true},
			{"name": "tag", "type": "string"},
			{"name": "score", "type": "number"},
		},
	})
	p.callData("rt-insert", "insert", map[string]any{
		"namespace": "rt", "table": "docs",
		"records": []map[string]any{
			{"title": "first bug", "tag": "a", "score": 0.75},
			{"title": "second crash", "tag": "b", "score": 2},
		},
	})
	query := p.callData("rt-query", "query", map[string]any{
		"namespace": "rt", "sql": "SELECT id, title, tag, score FROM docs ORDER BY id",
	})
	rows, _ := query["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("query returned %d rows, want 2: %v", len(rows), query)
	}
	first := rows[0].(map[string]any)
	if first["title"] != "first bug" || first["tag"] != "a" {
		t.Fatalf("row 1 mismatch: %v", first)
	}
	if int64val(t, "row 1 id", first["id"]) != 1 {
		t.Fatalf("row 1 id %v, want 1", first["id"])
	}
	search := p.callData("rt-search", "search_fulltext", map[string]any{
		"namespace": "rt", "table": "docs", "query": "bug OR crash",
	})
	hits, _ := search["results"].([]any)
	if len(hits) != 2 {
		t.Fatalf("search returned %d results, want 2: %v", len(hits), search)
	}
	read := p.callData("rt-read", "read_rows", map[string]any{
		"namespace": "rt", "table": "docs", "ids": []any{2},
	})
	readRows, _ := read["rows"].([]any)
	if len(readRows) != 1 || readRows[0].(map[string]any)["title"] != "second crash" {
		t.Fatalf("read_rows mismatch: %v", read)
	}
	if err := p.waitEOF(); err != nil {
		t.Fatalf("dolmen mcp must exit cleanly on stdin EOF, got %v; stderr: %s", err, p.stderrText())
	}
}

func noneServer(t *testing.T) string {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	apiSrv := api.New(st, embed.None{})
	mcpSrv := mcp.New(apiSrv, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpSrv)
	mux.Handle("/", apiSrv.Handler())
	srv := httptest.NewServer(api.OriginGuard(mux, nil))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { _ = st.Close() })
	return srv.URL
}

func nonePost(t *testing.T, url string, body any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	res, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode response from %s: %v", url, err)
	}
	return out
}

func stdioParityScript() []parityStep {
	ns := "stdio-par"
	fields := []map[string]any{
		{"name": "title", "type": "string", "fulltext": true},
		{"name": "tag", "type": "string"},
		{"name": "score", "type": "number"},
		{"name": "flag", "type": "boolean"},
		{"name": "meta", "type": "json"},
	}
	return []parityStep{
		{"create_namespace", "create_namespace", map[string]any{"namespace": ns}, false},
		{"create_table", "create_table", map[string]any{"namespace": ns, "table": "docs", "fields": fields}, false},
		{"insert", "insert", map[string]any{
			"namespace": ns, "table": "docs",
			"records": []map[string]any{
				{"title": "first bug", "tag": "a", "score": 0.75, "flag": true, "meta": map[string]any{"k": []any{1, 2}}},
				{"title": "second crash", "tag": "b", "score": 2, "flag": false, "meta": nil},
			},
		}, false},
		{"upsert_by_key", "upsert_by_key", map[string]any{
			"namespace": ns, "table": "docs", "on": []string{"tag"},
			"records": []map[string]any{{"tag": "a", "score": 9}},
		}, false},
		{"upsert_insert_branch", "upsert", map[string]any{
			"namespace": ns, "table": "docs", "filter": "tag = 'zz'",
			"set": map[string]any{"tag": "zz", "title": "new row"},
		}, false},
		{"update", "update", map[string]any{
			"namespace": ns, "table": "docs", "filter": "tag = 'zz'",
			"set": map[string]any{"score": 1.5},
		}, false},
		{"query", "query", map[string]any{
			"namespace": ns,
			"sql":       "SELECT id, title, tag, score, flag, meta FROM docs ORDER BY id",
		}, false},
		{"read_rows", "read_rows", map[string]any{
			"namespace": ns, "table": "docs", "ids": []any{3, 999, 1},
		}, false},
		{"search_fulltext", "search_fulltext", map[string]any{
			"namespace": ns, "table": "docs", "query": "bug OR crash",
		}, false},
		{"search_fulltext_filtered", "search_fulltext", map[string]any{
			"namespace": ns, "table": "docs", "query": "bug OR crash",
			"filter": "score >= ?", "args": []any{1},
		}, false},
		{"changes_since", "changes_since", map[string]any{
			"namespace": ns, "table": "docs", "cursor": "begin",
		}, false},
		{"wait_for", "wait_for", map[string]any{
			"namespace": ns, "table": "docs", "cursor": "begin", "timeout_ms": 0,
		}, false},
		{"describe_table", "describe_table", map[string]any{"namespace": ns, "table": "docs"}, false},
		{"list_tables", "list_tables", map[string]any{"namespace": ns}, false},
		{"infer_schema", "infer_schema", map[string]any{
			"samples": []map[string]any{{"a": 1, "b": "x", "c": true}},
		}, false},
		{"list_migrations", "list_migrations", map[string]any{"namespace": ns, "table": "docs"}, false},
		{"capabilities", "capabilities", map[string]any{}, false},
		{"describe_server", "describe_server", map[string]any{}, false},
		{"list_namespaces", "list_namespaces", map[string]any{}, false},
		{"delete", "delete", map[string]any{
			"namespace": ns, "table": "docs", "filter": "tag = 'zz'",
		}, false},
		{"query_after_delete", "query", map[string]any{
			"namespace": ns, "sql": "SELECT id, title FROM docs ORDER BY id",
		}, false},
	}
}

func TestStdioParityWithHTTP(t *testing.T) {
	base := noneServer(t)
	p := startStdio(t)
	for i, step := range stdioParityScript() {
		id := fmt.Sprintf("par-%02d-%s", i, step.name)
		httpOut := nonePost(t, base+"/v1/"+step.op, step.body)
		res := p.call(id, step.op, step.body)
		result := res["result"].(map[string]any)
		var stdioOut, httpCmp map[string]any
		if result["isError"] == true {
			stdioOut = withoutRequestID(parseStdioToolError(t, id, result))
			errEnv, ok := httpOut["error"].(map[string]any)
			if !ok {
				t.Fatalf("step %s: stdio tool error but HTTP succeeded: %v", step.name, httpOut)
			}
			httpCmp = withoutRequestID(errEnv)
		} else {
			stdioOut = result["structuredContent"].(map[string]any)
			if httpOut["ok"] != true {
				t.Fatalf("step %s: HTTP failed where stdio succeeded: %v", step.name, httpOut)
			}
			httpCmp = httpOut["data"].(map[string]any)
		}
		t.Run(step.name, func(t *testing.T) {
			assertJSONEqual(t, step.name+" across transports", maskVolatile(t, stdioOut), maskVolatile(t, httpCmp))
		})
	}
}

func parseStdioToolError(t *testing.T, id string, result map[string]any) map[string]any {
	t.Helper()
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("%s: tool error carries no text content: %v", id, result)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	var env map[string]any
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatalf("%s: tool error text is not the standard envelope: %q", id, text)
	}
	return env
}

func TestStdioErrorTeachingPaths(t *testing.T) {
	p := startStdio(t)
	p.sendLine("not json")
	e := p.recv()["error"].(map[string]any)
	if e["code"] != float64(-32700) {
		t.Fatalf("parse error envelope: %v", e)
	}
	res := p.rpc("e-init", "initialize", stdioInitializeParams())
	if res["result"] == nil {
		t.Fatalf("initialize failed: %v", res)
	}
	p.callData("e-ns", "create_namespace", map[string]any{"namespace": "errt"})
	p.callData("e-table", "create_table", map[string]any{
		"namespace": "errt", "table": "t",
		"fields": []map[string]any{{"name": "title", "type": "string", "fulltext": true}},
	})
	env := p.callError("e-missing-table", "describe_table", map[string]any{"namespace": "errt", "table": "nope"})
	if env["code"] != "not_found" {
		t.Fatalf("missing-table error code %v, want not_found: %v", env["code"], env)
	}
	if rid, _ := env["request_id"].(string); rid == "" {
		t.Fatal("tool error envelope must carry a request_id")
	}
	vecEnv := p.callError("e-vectorize", "create_table", map[string]any{
		"namespace": "errt", "table": "vec",
		"fields": []map[string]any{{"name": "body", "type": "text", "vectorize": true}},
	})
	if vecEnv["code"] != "invalid_request" {
		t.Fatalf("vectorize error code %v, want invalid_request: %v", vecEnv["code"], vecEnv)
	}
	if !strings.Contains(vecEnv["message"].(string), "DOLMEN_EMBED_PROVIDER") {
		t.Fatalf("vectorize error must teach the provider fix: %v", vecEnv)
	}
	proto := p.rpcErr("e-unknown-tool", "tools/call", map[string]any{"name": "bogus", "arguments": map[string]any{}})
	if proto["code"] != float64(-32602) {
		t.Fatalf("unknown tool code %v, want -32602: %v", proto["code"], proto)
	}
	proto = p.rpcErr("e-bad-method", "bogus/method", map[string]any{})
	if proto["code"] != float64(-32601) {
		t.Fatalf("unknown method code %v, want -32601: %v", proto["code"], proto)
	}
}

func TestStdioSignalsExitCleanly(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  syscall.Signal
	}{
		{"SIGTERM", syscall.SIGTERM},
		{"SIGINT", syscall.SIGINT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := startStdio(t)
			if res := p.rpc("s-ping", "ping", map[string]any{}); res["result"] == nil {
				t.Fatalf("ping before signal failed: %v", res)
			}
			done := make(chan error, 1)
			go func() { done <- p.cmd.Wait() }()
			if err := p.cmd.Process.Signal(tc.sig); err != nil {
				t.Fatalf("signal dolmen mcp: %v", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("dolmen mcp must exit cleanly on %s, got %v; stderr: %s", tc.name, err, p.stderrText())
				}
			case <-time.After(15 * time.Second):
				_ = p.cmd.Process.Kill()
				t.Fatalf("%s: dolmen mcp did not exit within 15s; stderr: %s", tc.name, p.stderrText())
			}
		})
	}
}
