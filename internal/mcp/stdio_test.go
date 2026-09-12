package mcp

import (
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
	type want struct {
		id      any
		code    float64
		message string
	}
	cases := []want{
		{"i1", 0, ""},
		{"i2", 0, ""},
		{nil, -32700, "invalid JSON"},
		{nil, -32600, "request id must be a string or number"},
		{"i3", -32600, "expected a JSON-RPC 2.0 request"},
		{"i4", -32601, `unknown method "nope"`},
		{"i5", -32602, `unknown tool "no_such_tool"`},
	}
	for i, c := range cases {
		if got := msgs[i]["id"]; !reflect.DeepEqual(got, c.id) {
			t.Errorf("response %d id %v, want %v", i, got, c.id)
		}
		if c.code == 0 {
			if msgs[i]["result"] == nil {
				t.Errorf("response %d must carry a result: %+v", i, msgs[i])
			}
			continue
		}
		e, ok := msgs[i]["error"].(map[string]any)
		if !ok {
			t.Fatalf("response %d must carry a JSON-RPC error: %+v", i, msgs[i])
		}
		if e["code"] != c.code || e["message"] != c.message {
			t.Errorf("response %d error %v/%v, want %v/%v", i, e["code"], e["message"], c.code, c.message)
		}
		if msgs[i]["jsonrpc"] != "2.0" {
			t.Errorf("response %d must carry jsonrpc 2.0: %+v", i, msgs[i])
		}
	}
	last := msgs[7]["result"].(map[string]any)
	if last["isError"] != false {
		t.Fatalf("tools/call list_namespaces must succeed: %+v", last)
	}
	if _, ok := last["structuredContent"].(map[string]any); !ok {
		t.Fatalf("tools/call result must carry structuredContent: %+v", last)
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
