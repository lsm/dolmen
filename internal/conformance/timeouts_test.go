package conformance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/api"
)

const endlessQuery = "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT count(*) FROM c"

func postWithin(t *testing.T, url string, body any, limit time.Duration) (int, map[string]any, time.Duration) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: limit}
	start := time.Now()
	res, err := client.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s did not answer within %s: %v", url, limit, err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return res.StatusCode, out, time.Since(start)
}

func errorOf(t *testing.T, env map[string]any) (string, string) {
	t.Helper()
	e, _ := env["error"].(map[string]any)
	if e == nil {
		t.Fatalf("no error envelope in %v", env)
	}
	code, _ := e["code"].(string)
	msg, _ := e["message"].(string)
	return code, msg
}

func TestAnOperationPastItsLimitAnswersTimeoutOnEveryTransport(t *testing.T) {
	h := newHarnessTimeouts(t, api.Timeouts{Op: 300 * time.Millisecond})
	h.ensureNS("slow")
	want := "query did not finish within the server's 300ms operation time limit (-op-timeout, DOLMEN_OP_TIMEOUT)"

	status, out, took := postWithin(t, h.httpURL+"/query", map[string]any{"namespace": "slow", "sql": endlessQuery}, 20*time.Second)
	if status != http.StatusGatewayTimeout {
		t.Fatalf("an endless query past the operation limit: status %d, want 504: %v", status, out)
	}
	if code, msg := errorOf(t, out); code != "timeout" || !strings.Contains(msg, want) {
		t.Fatalf("got %s %q, want timeout naming %q", code, msg, want)
	}
	if took > 10*time.Second {
		t.Fatalf("the refusal took %s: SQLite must stop at the deadline, not run the statement to completion", took)
	}

	env := h.mcpCall("query", map[string]any{"namespace": "slow", "sql": endlessQuery}).toolError()
	if env == nil || env["code"] != "timeout" || !strings.Contains(fmt.Sprint(env["message"]), want) {
		t.Fatalf("MCP must carry the same timeout envelope as /v1, got %v", env)
	}

	data := h.mustHTTP("query", map[string]any{"namespace": "slow", "sql": "SELECT 1 AS one"})
	if rows, _ := data["rows"].([]any); len(rows) != 1 {
		t.Fatalf("the namespace must still answer after an interrupted statement, got %v", data)
	}
}

func TestAWriteThatOverrunsItsLimitCommitsNothing(t *testing.T) {
	h := newHarnessTimeouts(t, api.Timeouts{Op: 200 * time.Millisecond})
	h.seedTable("slowemb", "notes", []map[string]any{{"name": "body", "type": "text", "vectorize": true}})
	h.emb.mu.Lock()
	h.emb.delay = 5 * time.Second
	h.emb.mu.Unlock()

	status, out, took := postWithin(t, h.httpURL+"/insert", map[string]any{
		"namespace": "slowemb", "table": "notes", "records": []map[string]any{{"body": "hello"}},
	}, 20*time.Second)
	if status != http.StatusGatewayTimeout {
		t.Fatalf("an insert whose embedding outlives the limit: status %d, want 504: %v", status, out)
	}
	if code, msg := errorOf(t, out); code != "timeout" || !strings.Contains(msg, "insert did not finish") || !strings.Contains(msg, "may or may not have committed") {
		t.Fatalf("got %s %q", code, msg)
	}
	if took > 4*time.Second {
		t.Fatalf("the refusal took %s: the deadline must reach the embedding provider", took)
	}

	h.emb.mu.Lock()
	h.emb.delay = 0
	h.emb.mu.Unlock()
	data := h.mustHTTP("query", map[string]any{"namespace": "slowemb", "sql": "SELECT count(*) AS n FROM notes"})
	if rows, _ := data["rows"].([]any); len(rows) != 1 || fmt.Sprint(rows[0].(map[string]any)["n"]) != "0" {
		t.Fatalf("the timed-out insert must have committed nothing, got %v", data)
	}
}

func TestWaitForKeepsItsOwnTimeoutOnTopOfTheOperationLimit(t *testing.T) {
	h := newHarnessTimeouts(t, api.Timeouts{Op: 200 * time.Millisecond, Write: 200 * time.Millisecond})
	h.seedTable("waits", "t", []map[string]any{{"name": "body", "type": "text"}})

	status, out, took := postWithin(t, h.httpURL+"/wait_for", map[string]any{"namespace": "waits", "table": "t", "timeout_ms": 700}, 20*time.Second)
	if status != http.StatusOK || out["ok"] != true {
		t.Fatalf("a wait longer than both limits must still end as an empty page: status %d %v", status, out)
	}
	if took < 600*time.Millisecond {
		t.Fatalf("the wait returned after %s, before its own timeout_ms", took)
	}
	res := h.mcpCall("wait_for", map[string]any{"namespace": "waits", "table": "t", "timeout_ms": 700})
	if res.isError() || res.proto != nil {
		t.Fatalf("the same wait over MCP must end as an empty page, got %+v", res)
	}
}

func TestMigrateAnswersToItsOwnLimitNotTheOperationLimit(t *testing.T) {
	seed := func(h *harness) {
		h.seedTable("mig", "notes", []map[string]any{{"name": "body", "type": "text"}})
		h.mustHTTP("insert", map[string]any{"namespace": "mig", "table": "notes", "records": []map[string]any{{"body": "a"}, {"body": "b"}}})
		h.emb.mu.Lock()
		h.emb.delay = 600 * time.Millisecond
		h.emb.mu.Unlock()
	}
	vectorize := map[string]any{"namespace": "mig", "table": "notes", "changes": []map[string]any{{"op": "set_vectorize", "name": "body", "value": true}}}

	bounded := newHarnessTimeouts(t, api.Timeouts{Op: 100 * time.Millisecond, Migrate: 200 * time.Millisecond})
	seed(bounded)
	status, out, _ := postWithin(t, bounded.httpURL+"/migrate", vectorize, 20*time.Second)
	if status != http.StatusGatewayTimeout {
		t.Fatalf("a backfill past -migrate-timeout: status %d, want 504: %v", status, out)
	}
	if code, msg := errorOf(t, out); code != "timeout" || !strings.Contains(msg, "migrate did not finish within the server's 200ms migration time limit (-migrate-timeout, DOLMEN_MIGRATE_TIMEOUT)") {
		t.Fatalf("got %s %q", code, msg)
	}
	if v := tableVersion(bounded, "mig", "notes"); fmt.Sprint(v) != "1" {
		t.Fatalf("the stopped migration must leave the table at version 1, got %v", v)
	}

	unbounded := newHarnessTimeouts(t, api.Timeouts{Op: 100 * time.Millisecond})
	seed(unbounded)
	status, out, took := postWithin(t, unbounded.httpURL+"/migrate", vectorize, 20*time.Second)
	if status != http.StatusOK || out["ok"] != true {
		t.Fatalf("with no migration limit the same backfill must finish, whatever -op-timeout says: status %d %v", status, out)
	}
	if took < 500*time.Millisecond {
		t.Fatalf("the migration finished in %s, before the provider's delay, so it proves nothing about the operation limit", took)
	}
	if v := tableVersion(unbounded, "mig", "notes"); fmt.Sprint(v) != "2" {
		t.Fatalf("the finished migration must bump the version to 2, got %v", v)
	}
}

func tableVersion(h *harness, ns, table string) any {
	h.t.Helper()
	described, _ := h.mustHTTP("describe_table", map[string]any{"namespace": ns, "table": table})["table"].(map[string]any)
	return described["version"]
}

func dialServer(t *testing.T, h *harness) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", h.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestASlowBodyAnswersTimeoutWith408(t *testing.T) {
	h := newHarnessTimeouts(t, api.Timeouts{Read: 600 * time.Millisecond, Write: 200 * time.Millisecond})
	for _, path := range []string{"/v1/list_namespaces", "/mcp"} {
		t.Run(path, func(t *testing.T) {
			conn := dialServer(t, h)
			fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: dolmen\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n{", path)
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			res, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("no response to a body that stopped arriving: %v", err)
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != http.StatusRequestTimeout {
				t.Fatalf("status %d, want 408: %s", res.StatusCode, body)
			}
			if !strings.Contains(string(body), "did not arrive within the server's 600ms read limit (-read-timeout, DOLMEN_READ_TIMEOUT)") {
				t.Fatalf("the refusal must name the limit and its knob: %s", body)
			}
			if path != "/mcp" && !strings.Contains(string(body), `"code":"timeout"`) {
				t.Fatalf("/v1 must answer the timeout envelope: %s", body)
			}
		})
	}
}

func TestAResponseIsCutOffWhenItsReaderStalls(t *testing.T) {
	h := newHarnessTimeouts(t, api.Timeouts{Write: 300 * time.Millisecond})
	h.ensureNS("big")
	const payload = 12 << 20
	conn := dialServer(t, h)
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(16 << 10)
	}
	body := fmt.Sprintf(`{"namespace":"big","sql":"SELECT hex(zeroblob(%d)) AS b"}`, payload)
	fmt.Fprintf(conn, "POST /v1/query HTTP/1.1\r\nHost: dolmen\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)

	time.Sleep(2 * time.Second)
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	got, err := io.Copy(io.Discard, conn)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the server kept the stalled connection open after %d bytes; it must give up at the write limit", got)
	}
	if got >= 2*payload {
		t.Fatalf("the whole %d-byte response arrived although its reader stalled past the 300ms write limit", got)
	}
}

func TestAnIdleConnectionIsClosedAtTheIdleLimit(t *testing.T) {
	h := newHarnessTimeouts(t, api.Timeouts{Idle: 300 * time.Millisecond})
	conn := dialServer(t, h)
	fmt.Fprint(conn, "GET /healthz HTTP/1.1\r\nHost: dolmen\r\n\r\n")
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	_, err = br.ReadByte()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("an idle keep-alive connection must be closed by the server, got %v after %s", err, time.Since(start))
	}
}

func TestAHeaderBlockOverTheLimitIsRefused(t *testing.T) {
	h := newHarnessTimeouts(t, api.Timeouts{MaxHeaderBytes: 8 << 10})
	for _, size := range []int{2 << 10, 32 << 10} {
		conn := dialServer(t, h)
		fmt.Fprintf(conn, "GET /healthz HTTP/1.1\r\nHost: dolmen\r\nX-Padding: %s\r\n\r\n", strings.Repeat("a", size))
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		res, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("%d-byte header: %v", size, err)
		}
		res.Body.Close()
		want := http.StatusOK
		if size > 8<<10 {
			want = http.StatusRequestHeaderFieldsTooLarge
		}
		if res.StatusCode != want {
			t.Fatalf("%d-byte header: status %d, want %d", size, res.StatusCode, want)
		}
	}
}

func TestASubscriptionOutlivesTheWriteLimit(t *testing.T) {
	h := newHarnessTimeouts(t, api.Timeouts{Write: 200 * time.Millisecond})
	h.seedTable("live", "t", []map[string]any{{"name": "body", "type": "text"}})
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/subscribe?namespace=live&table=t", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("subscribe: status %d", res.StatusCode)
	}

	time.Sleep(700 * time.Millisecond)
	h.mustHTTP("insert", map[string]any{"namespace": "live", "table": "t", "records": []map[string]any{{"body": "late"}}})

	events := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "event: change") {
				events <- sc.Text()
				return
			}
		}
		close(events)
	}()
	select {
	case _, ok := <-events:
		if !ok {
			t.Fatal("the stream closed before delivering the change: the write limit must not apply to an open subscription")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no change arrived on the stream")
	}
}
