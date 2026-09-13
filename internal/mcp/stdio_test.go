package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/store"
)

func newStdioServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(api.New(st, fakeEmb{}), nil, opts...)
}

func runStdio(t *testing.T, s *Server, input string) ([]map[string]any, error) {
	t.Helper()
	var out bytes.Buffer
	err := s.ServeStdio(context.Background(), strings.NewReader(input), &out)
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	msgs := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stdio wrote a non-JSON line: %q", line)
		}
		msgs = append(msgs, m)
	}
	return msgs, err
}

func TestServeStdioFraming(t *testing.T) {
	s := newStdioServer(t)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":"i1","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		``,
		`   `,
		`{"jsonrpc":"2.0","id":"i2","method":"ping"}`,
		`not json`,
		`{"jsonrpc":"2.0","id":true,"method":"ping"}`,
		`{"jsonrpc":"1.0","id":"i3","method":"ping"}`,
		`{"jsonrpc":"2.0","id":"i4","method":"nope"}`,
		`{"jsonrpc":"2.0","id":"i5","method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":"i6","method":"tools/call","params":{"name":"list_namespaces","arguments":{}}}`,
	}, "\n")
	msgs, err := runStdio(t, s, input)
	if err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}
	if len(msgs) != 8 {
		t.Fatalf("got %d responses, want 8 (notification and blank lines produce none): %+v", len(msgs), msgs)
	}
	byID := map[string]map[string]any{}
	nullID := []map[string]any{}
	for _, m := range msgs {
		if m["jsonrpc"] != "2.0" {
			t.Fatalf("every response must carry jsonrpc 2.0: %+v", m)
		}
		if id, ok := m["id"].(string); ok {
			if _, dup := byID[id]; dup {
				t.Fatalf("duplicate response for id %q", id)
			}
			byID[id] = m
			continue
		}
		if m["id"] != nil {
			t.Fatalf("unexpected id %v", m["id"])
		}
		nullID = append(nullID, m)
	}
	for _, id := range []string{"i1", "i2", "i3", "i4", "i5", "i6"} {
		if byID[id] == nil {
			t.Fatalf("missing response for id %q: %+v", id, msgs)
		}
	}
	if len(nullID) != 2 {
		t.Fatalf("expected exactly 2 null-id error responses (invalid JSON, invalid id), got %+v", nullID)
	}
	nullErrs := map[float64]bool{}
	for _, m := range nullID {
		e, ok := m["error"].(map[string]any)
		if !ok {
			t.Fatalf("null-id response must carry a JSON-RPC error: %+v", m)
		}
		nullErrs[e["code"].(float64)] = true
	}
	if !nullErrs[-32700] || !nullErrs[-32600] {
		t.Fatalf("null-id errors must be one -32700 and one -32600, got %+v", nullID)
	}
	for _, id := range []string{"i1", "i2"} {
		if byID[id]["result"] == nil {
			t.Fatalf("%s must succeed: %+v", id, byID[id])
		}
	}
	i3 := byID["i3"]["error"].(map[string]any)
	if i3["code"] != float64(-32600) || i3["message"] != "expected a JSON-RPC 2.0 request" {
		t.Fatalf("i3 error %v", i3)
	}
	i4 := byID["i4"]["error"].(map[string]any)
	if i4["code"] != float64(-32601) || i4["message"] != `unknown method "nope"` {
		t.Fatalf("i4 error %v", i4)
	}
	i5 := byID["i5"]["error"].(map[string]any)
	if i5["code"] != float64(-32602) || i5["message"] != `unknown tool "no_such_tool"` {
		t.Fatalf("i5 error %v", i5)
	}
	tool := byID["i6"]["result"].(map[string]any)
	if tool["isError"] != false {
		t.Fatalf("tools/call list_namespaces must succeed: %+v", tool)
	}
	if _, ok := tool["structuredContent"].(map[string]any); !ok {
		t.Fatalf("tools/call result must carry structuredContent: %+v", tool)
	}
}

type stdioSession struct {
	t    *testing.T
	s    *Server
	inW  io.WriteCloser
	br   *bufio.Reader
	done chan error
}

func newStdioSession(t *testing.T, s *Server) *stdioSession {
	return newStdioSessionCtx(t, s, context.Background())
}

func (x *stdioSession) send(line string) {
	x.t.Helper()
	if _, err := x.inW.Write([]byte(line + "\n")); err != nil {
		x.t.Fatalf("write stdin: %v", err)
	}
}

func (x *stdioSession) sendCall(id, op string, args map[string]any) {
	x.t.Helper()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": op, "arguments": args}})
	if err != nil {
		x.t.Fatalf("marshal call: %v", err)
	}
	x.send(string(raw))
}

func (x *stdioSession) recv() map[string]any {
	x.t.Helper()
	ch := make(chan string, 1)
	go func() {
		line, err := x.br.ReadString('\n')
		if err != nil {
			ch <- ""
			return
		}
		ch <- line
	}()
	select {
	case line := <-ch:
		if line == "" {
			x.t.Fatal("stdout closed before the expected response")
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			x.t.Fatalf("stdout line is not JSON: %q", line)
		}
		return m
	case <-time.After(15 * time.Second):
		x.t.Fatal("no response within 15s")
		return nil
	}
}

func (x *stdioSession) close() error {
	x.t.Helper()
	x.inW.Close()
	select {
	case err := <-x.done:
		return err
	case <-time.After(15 * time.Second):
		x.t.Fatal("ServeStdio did not return after stdin EOF")
		return nil
	}
}

func (x *stdioSession) seedTable(ns string) {
	x.t.Helper()
	x.sendCall("seed-ns-"+ns, "create_namespace", map[string]any{"namespace": ns})
	if m := x.recv(); m["id"] != "seed-ns-"+ns || m["result"] == nil {
		x.t.Fatalf("create_namespace failed: %v", m)
	}
	x.sendCall("seed-table-"+ns, "create_table", map[string]any{
		"namespace": ns, "table": "t",
		"fields": []map[string]any{{"name": "title", "type": "string"}},
	})
	if m := x.recv(); m["id"] != "seed-table-"+ns || m["result"] == nil {
		x.t.Fatalf("create_table failed: %v", m)
	}
}

func TestServeStdioSlowCallDoesNotBlockReadLoop(t *testing.T) {
	x := newStdioSession(t, newStdioServer(t))
	x.seedTable("slow")

	x.sendCall("b-wait", "wait_for", map[string]any{"namespace": "slow", "table": "t", "timeout_ms": 2500})
	x.send(`{"jsonrpc":"2.0","id":"b-ping","method":"ping"}`)
	first, second := x.recv(), x.recv()
	if first["id"] != "b-ping" {
		t.Fatalf("the ping sent behind a slow wait_for must be answered first (the read loop must stay responsive); first response was for %v", first["id"])
	}
	if second["id"] != "b-wait" {
		t.Fatalf("second response must be the wait_for result, got id %v", second["id"])
	}

	if err := x.close(); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}
}

func TestServeStdioCancellationNotification(t *testing.T) {
	x := newStdioSession(t, newStdioServer(t))
	x.seedTable("cancel")

	x.sendCall("c-wait", "wait_for", map[string]any{"namespace": "cancel", "table": "t", "timeout_ms": 15000})
	time.Sleep(400 * time.Millisecond)
	start := time.Now()
	x.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"c-wait"}}`)
	m := x.recv()
	if m["id"] != "c-wait" {
		t.Fatalf("expected the cancelled request's response, got %v", m)
	}
	if code := toolErrorCode(t, m); code != "canceled" {
		t.Fatalf("a client-cancelled request must teach code canceled, got %q", code)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the cancellation notification must abort the in-flight wait (response after %v; the 15s timeout would still be running)", elapsed)
	}

	x.send(`{"jsonrpc":"2.0","id":"\u0065-wait","method":"tools/call","params":{"name":"wait_for","arguments":{"namespace":"cancel","table":"t","timeout_ms":15000}}}`)
	time.Sleep(400 * time.Millisecond)
	start = time.Now()
	x.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"e-wait"}}`)
	m = x.recv()
	if m["id"] != "e-wait" {
		t.Fatalf("expected the escaped-id request's response, got %v", m)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("a request id spelled with JSON escapes must cancel the same as its canonical form (response after %v)", elapsed)
	}

	if err := x.close(); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}
}

func newStdioSessionCtx(t *testing.T, s *Server, ctx context.Context) *stdioSession {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	x := &stdioSession{t: t, s: s, inW: inW, br: bufio.NewReader(outR), done: make(chan error, 1)}
	go func() { x.done <- s.ServeStdio(ctx, inR, outW) }()
	return x
}

func toolErrorCode(t *testing.T, m map[string]any) string {
	t.Helper()
	result, ok := m["result"].(map[string]any)
	if !ok || result["isError"] != true {
		t.Fatalf("expected a tool error envelope, got %v", m)
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("tool error carries no content: %v", result)
	}
	var env struct {
		Code string `json:"code"`
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatalf("tool error text is not the standard envelope: %q", text)
	}
	return env.Code
}

func TestServeStdioSignalGrantsGrace(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	x := newStdioSessionCtx(t, newStdioServer(t), parent)
	x.seedTable("grace")

	x.sendCall("g-wait", "wait_for", map[string]any{"namespace": "grace", "table": "t", "timeout_ms": 20000})
	time.Sleep(400 * time.Millisecond)
	start := time.Now()
	cancelParent()
	m := x.recv()
	if m["id"] != "g-wait" {
		t.Fatalf("expected the in-flight request's response after the signal, got %v", m)
	}
	if code := toolErrorCode(t, m); code != "canceled" {
		t.Fatalf("a signal-drained request must teach code canceled, got %q", code)
	}
	if elapsed := time.Since(start); elapsed < 3*time.Second {
		t.Fatalf("a signal must grant the drain grace to in-flight work (response after %v; the worker was cancelled at signal time)", elapsed)
	}
	if elapsed := time.Since(start); elapsed > 9*time.Second {
		t.Fatalf("the drain must cancel the worker after the grace and join (response after %v)", elapsed)
	}
	select {
	case err := <-x.done:
		if err != nil {
			t.Fatalf("ServeStdio after signal: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ServeStdio did not return after the signal drain")
	}
}

func TestServeStdioEOFDrainCancelsLingeringWorkers(t *testing.T) {
	x := newStdioSession(t, newStdioServer(t))
	x.seedTable("drain")

	x.sendCall("d-wait", "wait_for", map[string]any{"namespace": "drain", "table": "t", "timeout_ms": 12000})
	time.Sleep(400 * time.Millisecond)
	got := make(chan string, 1)
	go func() {
		line, err := x.br.ReadString('\n')
		if err != nil {
			got <- ""
			return
		}
		got <- line
	}()

	x.inW.Close()
	start := time.Now()
	select {
	case err := <-x.done:
		if err != nil {
			t.Fatalf("ServeStdio: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 9*time.Second {
			t.Fatalf("stdin EOF must not wait out a lingering worker's full timeout (returned after %v; the drain should cancel it after the 5s grace)", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ServeStdio did not return after stdin EOF")
	}
	line := <-got
	if line == "" {
		t.Fatal("stdout closed before the drained worker's response")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("drained worker response is not JSON: %q", line)
	}
	if m["id"] != "d-wait" {
		t.Fatalf("expected the d-wait response, got %v", m)
	}
}

func TestServeStdioReadErrorDrainsWorkers(t *testing.T) {
	x := newStdioSession(t, newStdioServer(t))
	x.seedTable("drainerr")

	x.sendCall("r-wait", "wait_for", map[string]any{"namespace": "drainerr", "table": "t", "timeout_ms": 12000})
	time.Sleep(400 * time.Millisecond)
	go func() { _, _ = x.inW.Write([]byte(strings.Repeat("a", stdioMaxLine+1) + "\n")) }()
	first := x.recv()
	if e, ok := first["error"].(map[string]any); !ok || e["code"] != float64(-32700) {
		t.Fatalf("the oversize line must draw a parse error first, got %v", first)
	}
	got := make(chan string, 1)
	go func() {
		line, err := x.br.ReadString('\n')
		if err != nil {
			got <- ""
			return
		}
		got <- line
	}()
	start := time.Now()
	select {
	case err := <-x.done:
		if err == nil {
			t.Fatal("an oversize line must fail ServeStdio")
		}
		if elapsed := time.Since(start); elapsed > 9*time.Second {
			t.Fatalf("the read-error return must drain lingering workers (grace + cancel + join), not wait out their timeout (returned after %v)", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ServeStdio did not return after the read error")
	}
	line := <-got
	if line == "" {
		t.Fatal("stdout closed before the drained worker's response")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("drained worker response is not JSON: %q", line)
	}
	if m["id"] != "r-wait" {
		t.Fatalf("expected the drained worker's response, got %v", m)
	}
	x.inW.Close()
}

func TestServeStdioLateLineAfterSignalIsNotAdmitted(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	x := newStdioSessionCtx(t, newStdioServer(t), parent)
	x.seedTable("late")

	x.sendCall("l-wait", "wait_for", map[string]any{"namespace": "late", "table": "t", "timeout_ms": 8000})
	time.Sleep(400 * time.Millisecond)
	cancelParent()
	x.sendCall("l-late", "create_namespace", map[string]any{"namespace": "post-shutdown"})
	responses := make(chan string, 16)
	go func() {
		for {
			line, err := x.br.ReadString('\n')
			if err != nil {
				close(responses)
				return
			}
			responses <- line
		}
	}()
	select {
	case err := <-x.done:
		if err != nil {
			t.Fatalf("ServeStdio after signal: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ServeStdio did not return after the signal drain")
	}
	for {
		select {
		case line, ok := <-responses:
			if !ok {
				x.inW.Close()
				return
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("response is not JSON: %q", line)
			}
			if m["id"] == "l-late" {
				t.Fatal("a request that arrives after shutdown begins must not be admitted or answered")
			}
		case <-time.After(2 * time.Second):
			x.inW.Close()
			return
		}
	}
}

func TestServeStdioNotificationsOnly(t *testing.T) {
	s := newStdioServer(t)
	msgs, err := runStdio(t, s, "{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n{\"jsonrpc\":\"2.0\",\"method\":\"cancelled\",\"params\":{}}\n")
	if err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("notifications must produce no output, got %+v", msgs)
	}
}

func TestServeStdioInitializeInstructions(t *testing.T) {
	s := newStdioServer(t)
	msgs, err := runStdio(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("initialize: %d responses, err %v", len(msgs), err)
	}
	res := msgs[0]["result"].(map[string]any)
	if res["protocolVersion"] != protocolVersion {
		t.Fatalf("protocolVersion %v", res["protocolVersion"])
	}
	instr, _ := res["instructions"].(string)
	if !strings.Contains(instr, "runs over stdio") {
		t.Fatalf("stdio instructions must name the transport: %q", instr)
	}
	if strings.Contains(instr, "connect to") {
		t.Fatalf("stdio instructions must not teach an HTTP connect: %q", instr)
	}
}

func TestServeStdioInitializeInstructionsWithBaseURL(t *testing.T) {
	s := newStdioServer(t, WithBaseURL("https://example.com"), WithPrefix("/d"))
	msgs, err := runStdio(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("initialize: %d responses, err %v", len(msgs), err)
	}
	instr := msgs[0]["result"].(map[string]any)["instructions"].(string)
	if !strings.Contains(instr, "https://example.com/d") {
		t.Fatalf("configured base URL (with prefix) must appear in stdio instructions: %q", instr)
	}
}

func TestServeStdioOversizeLine(t *testing.T) {
	s := newStdioServer(t)
	var out bytes.Buffer
	huge := strings.Repeat("a", stdioMaxLine+1)
	err := s.ServeStdio(context.Background(), strings.NewReader(huge+"\n"), &out)
	if err == nil {
		t.Fatal("an oversize line must fail ServeStdio")
	}
	var m map[string]any
	if jsonErr := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &m); jsonErr != nil {
		t.Fatalf("oversize line must still emit one JSON-RPC error line, got %q", out.String())
	}
	e := m["error"].(map[string]any)
	if e["code"] != float64(jsonRPCParseError) {
		t.Fatalf("oversize line must be a parse error, got %v", e)
	}
	if !strings.Contains(e["message"].(string), "MiB") {
		t.Fatalf("oversize message must name the limit, got %v", e["message"])
	}
}

type gatedReader struct {
	release <-chan struct{}
}

func (g *gatedReader) Read(p []byte) (int, error) {
	<-g.release
	return 0, io.EOF
}

func TestServeStdioCanceledContextStops(t *testing.T) {
	s := newStdioServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	release := make(chan struct{})
	defer close(release)
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- s.ServeStdio(ctx, &gatedReader{release}, &out) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("canceled context must stop cleanly, got %v", err)
		}
		if out.Len() != 0 {
			t.Fatalf("canceled context must produce no responses, got %q", out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a canceled context must unblock an idle ServeStdio (the read goroutine parks; the serve loop selects on ctx.Done)")
	}
}

func TestServeStdioToolsListMatchesHTTP(t *testing.T) {
	s := newStdioServer(t)
	httpSrv := httptest.NewServer(s)
	t.Cleanup(httpSrv.Close)
	msgs, err := runStdio(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("tools/list: %d responses, err %v", len(msgs), err)
	}
	res, err := http.Post(httpSrv.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatalf("http tools/list: %v", err)
	}
	defer res.Body.Close()
	var httpMsg map[string]any
	if err := json.NewDecoder(res.Body).Decode(&httpMsg); err != nil {
		t.Fatalf("decode http tools/list: %v", err)
	}
	if !reflect.DeepEqual(msgs[0], httpMsg) {
		t.Fatalf("tools/list must be identical over both framings:\nstdio: %v\nhttp:  %v", msgs[0], httpMsg)
	}
}
