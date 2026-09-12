package blackbox

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
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const (
	scenarioNamespace = "acme/support"
	mainRetention     = "1h"
	waitBudget        = 20 * time.Second
)

type serverProc struct {
	cmd     *exec.Cmd
	url     string
	dataDir string
	logPath string
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpResult struct {
	Content           []mcpContent   `json:"content"`
	IsError           bool           `json:"isError"`
	StructuredContent map[string]any `json:"structuredContent"`
}

var app struct {
	binPath    string
	tmpDir     string
	srv        *serverProc
	openapi    map[string]any
	schemas    map[string]map[string]any
	kbIDs      []int64
	kbCancelID []int64
	probeID    int64
	daemon     *sseStream
	daemonLast string
}

var mcpSeq atomic.Int64

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	goBin, err := exec.LookPath("go")
	if err != nil {
		fmt.Fprintln(os.Stderr, "blackbox: no go toolchain on PATH; this suite builds and execs the packaged binary")
		return 1
	}
	root, err := moduleRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "blackbox:", err)
		return 1
	}
	tmp, err := os.MkdirTemp("", "dolmen-blackbox-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "blackbox: tmp:", err)
		return 1
	}
	defer os.RemoveAll(tmp)
	app.tmpDir = tmp
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "blackbox:", err)
		return 1
	}
	app.binPath = filepath.Join(binDir, "dolmen")
	build := exec.Command(goBin, "build", "-o", app.binPath, ".")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "blackbox: building the packaged binary: %v\n%s", err, out)
		return 1
	}
	srv, err := bootServer(filepath.Join(tmp, "data"), mainRetention, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "blackbox:", err)
		return 1
	}
	app.srv = srv
	defer srv.stop()
	return m.Run()
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod found above the blackbox package")
		}
		dir = parent
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func bootServer(dataDir string, retention string, withBaseURL bool) (*serverProc, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		srv, err := startServer(dataDir, retention, withBaseURL)
		if err == nil {
			return srv, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func startServer(dataDir string, retention string, withBaseURL bool) (*serverProc, error) {
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("pick port: %w", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	url := "http://" + addr
	args := []string{"-addr", addr, "-data", dataDir, "-change-retention", retention}
	if withBaseURL {
		args = append(args, "-base-url", url)
	}
	logPath := filepath.Join(app.tmpDir, fmt.Sprintf("dolmen-%d.log", port))
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("create log: %w", err)
	}
	cmd := exec.Command(app.binPath, args...)
	cmd.Stderr = logFile
	cmd.Stdout = logFile
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("start server: %w", err)
	}
	srv := &serverProc{cmd: cmd, url: url, dataDir: dataDir, logPath: logPath}
	if err := srv.waitHealthy(15 * time.Second); err != nil {
		srv.stop()
		logFile.Close()
		return nil, fmt.Errorf("server on %s: %w\nserver log:\n%s", url, err, srv.logTail())
	}
	return srv, nil
}

func (s *serverProc) waitHealthy(deadline time.Duration) error {
	end := time.Now().Add(deadline)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(end) {
		resp, err := client.Get(s.url + "/healthz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == `{"status":"ok"}` {
				return nil
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("/healthz did not report ok")
}

func (s *serverProc) stop() error {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return errors.New("server not running")
	}
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		s.cmd.Process.Kill()
	}
	return s.cmd.Wait()
}

func (s *serverProc) logTail() string {
	raw, err := os.ReadFile(s.logPath)
	if err != nil {
		return "(no log)"
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return strings.Join(lines, "\n")
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func rawPost(t *testing.T, url string, headers map[string]string, body any) (int, http.Header, []byte) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, resp.Header, raw
}

func postRawBody(t *testing.T, url string, headers map[string]string, rawBody string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(rawBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, resp.Header, raw
}

func opRaw(name string, req any) (map[string]any, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Post(app.srv.url+"/v1/"+name, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d: %s", name, resp.StatusCode, raw)
	}
	var env struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%s: decode envelope: %v: %s", name, err, raw)
	}
	if !env.OK {
		return nil, fmt.Errorf("%s: ok=false: %s", name, raw)
	}
	return env.Data, nil
}

func op(t *testing.T, name string, req any) map[string]any {
	t.Helper()
	data, err := opRaw(name, req)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func opErrorEnvelope(t *testing.T, name string, req any) (int, map[string]any) {
	t.Helper()
	code, _, raw := rawPost(t, app.srv.url+"/v1/"+name, nil, req)
	var whole map[string]any
	if err := json.Unmarshal(raw, &whole); err != nil {
		t.Fatalf("%s: decode envelope: %v: %s", name, err, raw)
	}
	return code, whole
}

func decodeInto(t *testing.T, raw []byte, target any, what string) {
	t.Helper()
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("%s: decode: %v: %s", what, err, raw)
	}
}

func mcpRaw(method string, params any) (*mcpResult, error) {
	id := mcpSeq.Add(1)
	body := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		body["params"] = params
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Post(app.srv.url+"/mcp", "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mcp %s: HTTP %d: %s", method, resp.StatusCode, raw)
	}
	var reply struct {
		ID     int64      `json:"id"`
		Result *mcpResult `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("mcp %s: decode: %v: %s", method, err, raw)
	}
	if reply.ID != id {
		return nil, fmt.Errorf("mcp %s: response id %d does not match request id %d", method, reply.ID, id)
	}
	if reply.Error != nil {
		return nil, fmt.Errorf("mcp %s: protocol error %d: %s", method, reply.Error.Code, reply.Error.Message)
	}
	if reply.Result == nil {
		return nil, fmt.Errorf("mcp %s: no result object: %s", method, raw)
	}
	return reply.Result, nil
}

func mcpCall(t *testing.T, method string, params any) *mcpResult {
	t.Helper()
	res, err := mcpRaw(method, params)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func mcpToolRaw(name string, args map[string]any) (map[string]any, error) {
	res, err := mcpRaw("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, err
	}
	if res.IsError {
		msg := ""
		if len(res.Content) > 0 {
			msg = res.Content[0].Text
		}
		return nil, fmt.Errorf("mcp tools/call %s: isError with text: %s", name, msg)
	}
	if res.StructuredContent == nil {
		return nil, fmt.Errorf("mcp tools/call %s: no structuredContent in result", name)
	}
	return res.StructuredContent, nil
}

func mcpTool(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()
	data, err := mcpToolRaw(name, args)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type sseFrame struct {
	Event string
	Data  string
}

type sseStream struct {
	resp   *http.Response
	frames chan sseFrame
	errCh  chan error
	once   sync.Once
}

func openSubscribe(t *testing.T, namespace string, cursor string) *sseStream {
	t.Helper()
	stream, err := openSubscribeOn(app.srv.url, namespace, cursor)
	if err != nil {
		t.Fatalf("open subscribe stream: %v", err)
	}
	t.Cleanup(func() { stream.Close() })
	return stream
}

func openSubscribeOn(baseURL string, namespace string, cursor string) (*sseStream, error) {
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/subscribe", nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Set("namespace", namespace)
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	req.URL.RawQuery = q.Encode()
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("subscribe: HTTP %d", resp.StatusCode)
	}
	stream := &sseStream{resp: resp, frames: make(chan sseFrame, 4096), errCh: make(chan error, 1)}
	go stream.pump()
	return stream, nil
}

func (s *sseStream) pump() {
	scanner := bufio.NewScanner(s.resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var event, data strings.Builder
	flush := func() {
		if event.Len() == 0 && data.Len() == 0 {
			return
		}
		s.frames <- sseFrame{Event: event.String(), Data: data.String()}
		event.Reset()
		data.Reset()
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteString("\n")
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flush()
	s.errCh <- scanner.Err()
	close(s.frames)
}

func (s *sseStream) Next(t *testing.T, what string) sseFrame {
	t.Helper()
	select {
	case f, ok := <-s.frames:
		if !ok {
			select {
			case err := <-s.errCh:
				t.Fatalf("subscribe stream ended (%s): %v", what, err)
			default:
				t.Fatalf("subscribe stream ended (%s)", what)
			}
		}
		return f
	case <-time.After(waitBudget):
		t.Fatalf("timed out waiting for a frame (%s)", what)
		return sseFrame{}
	}
}

func (s *sseStream) NextChange(t *testing.T, what string) map[string]any {
	t.Helper()
	f := s.Next(t, what)
	if f.Event != "change" {
		t.Fatalf("(%s) expected a change frame, got event %q data %q", what, f.Event, f.Data)
	}
	var change map[string]any
	if err := json.Unmarshal([]byte(f.Data), &change); err != nil {
		t.Fatalf("(%s) decode change frame: %v: %s", what, err, f.Data)
	}
	return change
}

func (s *sseStream) Poll() (sseFrame, bool) {
	select {
	case f, ok := <-s.frames:
		return f, ok
	default:
		return sseFrame{}, false
	}
}

func (s *sseStream) Close() {
	s.once.Do(func() { s.resp.Body.Close() })
}

func fetchOpenAPI(t *testing.T) {
	t.Helper()
	if app.openapi != nil {
		return
	}
	resp, err := httpClient.Get(app.srv.url + "/v1/openapi.json")
	if err != nil {
		t.Fatalf("fetch openapi: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("openapi: HTTP %d", resp.StatusCode)
	}
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode openapi: %v", err)
	}
	app.openapi = doc
	app.schemas = map[string]map[string]any{}
}

func openapiPaths(t *testing.T) map[string]any {
	t.Helper()
	fetchOpenAPI(t)
	paths, _ := app.openapi["paths"].(map[string]any)
	if paths == nil {
		t.Fatal("openapi document has no paths")
	}
	return paths
}

func openapiSchema(t *testing.T, name string) map[string]any {
	t.Helper()
	fetchOpenAPI(t)
	if s, ok := app.schemas[name]; ok {
		return s
	}
	components, _ := app.openapi["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	raw, ok := schemas[name]
	if !ok {
		t.Fatalf("served openapi has no components/schemas/%s", name)
	}
	schema, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("components/schemas/%s is not an object", name)
	}
	app.schemas[name] = schema
	return schema
}

func assertConforms(t *testing.T, schema map[string]any, value any, where string) {
	t.Helper()
	for _, problem := range conformanceProblems(t, schema, value, where) {
		t.Error(problem)
	}
}

func conformanceProblems(t *testing.T, schema map[string]any, value any, where string) []string {
	t.Helper()
	if ref, ok := schema["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/components/schemas/")
		return conformanceProblems(t, openapiSchema(t, name), value, where)
	}
	if en, ok := schema["enum"].([]any); ok {
		for _, allowed := range en {
			if fmt.Sprintf("%v", allowed) == fmt.Sprintf("%v", value) {
				return nil
			}
		}
		return []string{fmt.Sprintf("%s: value %v is not in the served enum %v", where, value, en)}
	}
	if c, ok := schema["const"]; ok {
		if fmt.Sprintf("%v", c) != fmt.Sprintf("%v", value) {
			return []string{fmt.Sprintf("%s: value %v does not equal the served const %v", where, value, c)}
		}
		return nil
	}
	var problems []string
	if p, ok := schema["pattern"].(string); ok {
		s, isStr := value.(string)
		if !isStr {
			problems = append(problems, fmt.Sprintf("%s: pattern applies to strings, got %T", where, value))
		} else {
			re, err := regexp.Compile(p)
			if err != nil {
				t.Fatalf("%s: served pattern %q does not compile: %v", where, p, err)
			}
			if !re.MatchString(s) {
				problems = append(problems, fmt.Sprintf("%s: %q does not match the served pattern %q", where, s, p))
			}
		}
	}
	switch schema["type"] {
	case "object":
		obj, ok := value.(map[string]any)
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: expected an object, got %T", where, value))
			return problems
		}
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				key, _ := r.(string)
				if _, present := obj[key]; !present {
					problems = append(problems, fmt.Sprintf("%s: required key %q is missing", where, key))
				}
			}
		}
		props, _ := schema["properties"].(map[string]any)
		strict := false
		if raw, ok := schema["additionalProperties"]; ok {
			if b, isBool := raw.(bool); isBool && !b {
				strict = true
			}
		}
		for k, v := range obj {
			prop, declared := props[k]
			if !declared {
				if strict {
					problems = append(problems, fmt.Sprintf("%s: key %q is not in the served schema (additionalProperties: false)", where, k))
				}
				continue
			}
			propSchema, _ := prop.(map[string]any)
			problems = append(problems, conformanceProblems(t, propSchema, v, where+"."+k)...)
		}
	case "array":
		arr, ok := value.([]any)
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: expected an array, got %T", where, value))
			return problems
		}
		items, _ := schema["items"].(map[string]any)
		for i, item := range arr {
			problems = append(problems, conformanceProblems(t, items, item, where+"["+strconv.Itoa(i)+"]")...)
		}
	case "string":
		if _, ok := value.(string); !ok {
			problems = append(problems, fmt.Sprintf("%s: expected a string, got %T (%v)", where, value, value))
		}
	case "integer":
		num, ok := toFloat(value)
		if !ok || num != float64(int64(num)) {
			problems = append(problems, fmt.Sprintf("%s: expected an integer, got %v", where, value))
		}
	case "number":
		if _, ok := toFloat(value); !ok {
			problems = append(problems, fmt.Sprintf("%s: expected a number, got %T (%v)", where, value, value))
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			problems = append(problems, fmt.Sprintf("%s: expected a boolean, got %T (%v)", where, value, value))
		}
	case "null":
		if value != nil {
			problems = append(problems, fmt.Sprintf("%s: expected null, got %v", where, value))
		}
	case nil:
	default:
		t.Fatalf("%s: validator does not understand served schema type %v", where, schema["type"])
	}
	return problems
}

func toFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func asMapList(t *testing.T, value any, what string) []map[string]any {
	t.Helper()
	raw, ok := value.([]any)
	if !ok {
		t.Fatalf("%s: expected a list, got %T", what, value)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s: expected objects in the list, got %T", what, item)
		}
		out = append(out, m)
	}
	return out
}

func asInt(t *testing.T, value any, what string) int64 {
	t.Helper()
	f, ok := toFloat(value)
	if !ok {
		t.Fatalf("%s: expected a number, got %T (%v)", what, value, value)
	}
	return int64(f)
}

func asStr(t *testing.T, value any, what string) string {
	t.Helper()
	s, ok := value.(string)
	if !ok {
		t.Fatalf("%s: expected a string, got %T (%v)", what, value, value)
	}
	return s
}
