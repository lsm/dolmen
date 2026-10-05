package lakehouse

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/value"
)

const DialectDuckDB = "duckdb"

const currentMetadataName = "dolmen-current.metadata.json"

const sidecarStartTimeout = 60 * time.Second

const sidecarCancelGrace = 5 * time.Second

var ErrSQLEngineUnavailable = errors.New("lakehouse SQL engine unavailable")

type SQLEngine struct {
	Binary       string
	ExtensionDir string
	Memory       int64
	Threads      int
}

func WithSQLEngine(cfg SQLEngine) OpenOption { return func(s *Store) { s.sqlEngine = cfg } }

func sqlUnavailable(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrSQLEngineUnavailable, fmt.Sprintf(format, args...))
}

type sidecar struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	fingerprint string
	run         sync.Mutex
	write       sync.Mutex
	pmu         sync.Mutex
	pending     map[string]chan []string
	next        uint64
	done        chan struct{}
	exitErr     error
	stderr      *tailBuffer
	home        string
}

type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

func escapeField(v string) string {
	r := strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)
	return r.Replace(v)
}

func splitLine(line string) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && i+1 < len(line):
			i++
			switch line[i] {
			case 't':
				cur.WriteByte('\t')
			case 'n':
				cur.WriteByte('\n')
			case 'r':
				cur.WriteByte('\r')
			default:
				cur.WriteByte(line[i])
			}
		case c == '\t':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(out, cur.String())
}

func startSidecar(ctx context.Context, cfg SQLEngine, dataDir string, views []string, fingerprint string) (*sidecar, error) {
	if cfg.Binary == "" {
		return nil, sqlUnavailable("query on a lakehouse namespace needs the dolmen-duckdb SQL sidecar, and none is configured")
	}
	home, err := os.MkdirTemp("", "dolmen-duckdb-home-")
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(cfg.Binary)
	cmd.Env = []string{
		"HOME=" + home,
		"USERPROFILE=" + home,
		"DOLMEN_DUCKDB_DATA_DIR=" + dataDir,
		"DOLMEN_DUCKDB_EXTENSION_DIR=" + cfg.ExtensionDir,
		"DOLMEN_DUCKDB_MEMORY=" + strconv.FormatInt(cfg.Memory, 10),
		"DOLMEN_DUCKDB_THREADS=" + strconv.Itoa(cfg.Threads),
		"TMPDIR=" + os.TempDir(),
		"TMP=" + os.TempDir(),
		"TEMP=" + os.TempDir(),
	}
	if root := os.Getenv("SYSTEMROOT"); root != "" {
		cmd.Env = append(cmd.Env, "SYSTEMROOT="+root)
	}
	sc := &sidecar{home: home, cmd: cmd, fingerprint: fingerprint, pending: map[string]chan []string{}, done: make(chan struct{}), stderr: &tailBuffer{}}
	cmd.Stderr = sc.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		os.RemoveAll(home)
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		os.RemoveAll(home)
		return nil, err
	}
	sc.stdin = stdin
	if err := cmd.Start(); err != nil {
		os.RemoveAll(home)
		return nil, sqlUnavailable("cannot start the SQL sidecar %s: %v", cfg.Binary, err)
	}
	ready := make(chan error, 1)
	go sc.read(stdout, ready)
	timer := time.NewTimer(sidecarStartTimeout)
	defer timer.Stop()
	select {
	case err := <-ready:
		if err != nil {
			sc.stop()
			return nil, err
		}
	case <-ctx.Done():
		sc.stop()
		return nil, ctx.Err()
	case <-timer.C:
		sc.stop()
		return nil, sqlUnavailable("the SQL sidecar did not become ready within %s", sidecarStartTimeout)
	}
	for _, stmt := range append(views, "") {
		op, args := "define", []string{stmt}
		if stmt == "" {
			op, args = "seal", nil
		}
		if _, err := sc.call(ctx, op, args...); err != nil {
			sc.stop()
			return nil, err
		}
	}
	return sc, nil
}

func (sc *sidecar) read(stdout io.Reader, ready chan<- error) {
	r := bufio.NewReaderSize(stdout, 1<<20)
	first := true
	var exitErr error
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			exitErr = err
			break
		}
		fields := splitLine(strings.TrimRight(line, "\r\n"))
		if first {
			first = false
			if len(fields) == 2 && fields[0] == "0" && fields[1] == "ready" {
				ready <- nil
				continue
			}
			ready <- sqlUnavailable("the SQL sidecar answered %q instead of its ready line", line)
			continue
		}
		sc.pmu.Lock()
		ch := sc.pending[fields[0]]
		delete(sc.pending, fields[0])
		sc.pmu.Unlock()
		if ch != nil {
			ch <- fields
		}
	}
	waitErr := sc.cmd.Wait()
	reason := sc.stderr.String()
	if reason == "" && waitErr != nil {
		reason = waitErr.Error()
	}
	if reason == "" {
		reason = exitErr.Error()
	}
	sc.pmu.Lock()
	sc.exitErr = sqlUnavailable("the SQL sidecar exited: %s", reason)
	for id, ch := range sc.pending {
		close(ch)
		delete(sc.pending, id)
	}
	sc.pmu.Unlock()
	if first {
		ready <- sc.exitErr
	}
	os.RemoveAll(sc.home)
	close(sc.done)
}

func (sc *sidecar) alive() bool {
	select {
	case <-sc.done:
		return false
	default:
		return true
	}
}

func (sc *sidecar) send(fields ...string) error {
	escaped := make([]string, len(fields))
	for i, f := range fields {
		escaped[i] = escapeField(f)
	}
	sc.write.Lock()
	defer sc.write.Unlock()
	_, err := io.WriteString(sc.stdin, strings.Join(escaped, "\t")+"\n")
	return err
}

func (sc *sidecar) call(ctx context.Context, op string, args ...string) ([]string, error) {
	sc.pmu.Lock()
	if sc.exitErr != nil {
		err := sc.exitErr
		sc.pmu.Unlock()
		return nil, err
	}
	sc.next++
	id := strconv.FormatUint(sc.next, 10)
	ch := make(chan []string, 1)
	sc.pending[id] = ch
	sc.pmu.Unlock()
	if err := sc.send(append([]string{id, op}, args...)...); err != nil {
		return nil, sqlUnavailable("cannot reach the SQL sidecar: %v", err)
	}
	select {
	case fields, ok := <-ch:
		if !ok {
			return nil, sc.exitErr
		}
		return fields, nil
	case <-ctx.Done():
		_ = sc.send("0", "cancel")
		timer := time.NewTimer(sidecarCancelGrace)
		defer timer.Stop()
		select {
		case <-ch:
		case <-timer.C:
			sc.stop()
		}
		return nil, ctx.Err()
	}
}

func (sc *sidecar) stop() {
	if sc.alive() {
		_ = sc.send("0", "shutdown")
		_ = sc.stdin.Close()
		select {
		case <-sc.done:
			return
		case <-time.After(sidecarCancelGrace):
		}
	}
	if sc.cmd.Process != nil {
		_ = sc.cmd.Process.Kill()
	}
}

func quoteLiteral(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }

func quoteIdent(v string) string { return `"` + strings.ReplaceAll(v, `"`, `""`) + `"` }

func emptyType(f schema.Field) string {
	switch f.Type {
	case schema.Number:
		return "DOUBLE"
	case schema.Boolean:
		return "BOOLEAN"
	case schema.Vector:
		return "BLOB"
	}
	return "VARCHAR"
}

func viewSQL(state tableState, raw bool) string {
	sc := state.schema
	cols := []string{}
	if snap := state.native.Metadata().CurrentSnapshot(); snap == nil {
		cols = append(cols, `CAST(NULL AS BIGINT) AS "id"`, `CAST(NULL AS VARCHAR) AS "created_at"`)
		for _, f := range sc.Fields {
			cols = append(cols, "CAST(NULL AS "+emptyType(f)+") AS "+quoteIdent(f.Name))
		}
		if sc.HasOwner {
			cols = append(cols, "CAST(NULL AS VARCHAR) AS "+quoteIdent(schema.OwnerColumn))
		}
		return "CREATE VIEW " + viewName(sc.Name, raw) + " AS SELECT " + strings.Join(cols, ", ") + " WHERE false"
	}
	cols = append(cols, `"id"`, `"created_at"`)
	for _, f := range sc.Fields {
		col := quoteIdent(f.Name)
		switch f.Type {
		case schema.Number:
			col = "TRY_CAST(" + col + " AS DOUBLE) AS " + col
		case schema.Secret:
			stand := quoteLiteral(secret.Mask)
			if raw {
				stand = "chr(0)"
			}
			col = "CASE WHEN " + col + " IS NULL THEN NULL ELSE " + stand + " END AS " + col
		}
		cols = append(cols, col)
	}
	if sc.HasOwner {
		cols = append(cols, quoteIdent(schema.OwnerColumn))
	}
	path := filepath.Join(filepath.FromSlash(strings.TrimPrefix(state.native.Location(), "file://")), "metadata", currentMetadataName)
	return "CREATE VIEW " + viewName(sc.Name, raw) + " AS SELECT " + strings.Join(cols, ", ") + " FROM iceberg_scan(" + quoteLiteral(filepath.ToSlash(path)) + ")"
}

const filterSchema = "_dolmen_filter"

func viewName(table string, raw bool) string {
	if raw {
		return quoteIdent(filterSchema) + "." + quoteIdent(table)
	}
	return quoteIdent(table)
}

func (s *Store) namespaceViews(ctx context.Context, n *namespace, ns string) ([]string, string, map[string]schema.FieldType, error) {
	var states []tableState
	for ident, err := range n.catalog.ListTables(ctx, tableIdentifierNamespace(ns)) {
		if err != nil {
			return nil, "", nil, err
		}
		state, err := loadTable(ctx, n, ns, ident[len(ident)-1])
		if err != nil {
			return nil, "", nil, err
		}
		states = append(states, state)
	}
	slices.SortFunc(states, func(a, b tableState) int { return strings.Compare(a.schema.Name, b.schema.Name) })
	views := []string{"CREATE SCHEMA " + quoteIdent(filterSchema)}
	labels := map[string]schema.FieldType{"id": schema.Number, "created_at": schema.Timestamp}
	ambiguous := map[string]bool{}
	var fp strings.Builder
	for _, state := range states {
		views = append(views, viewSQL(state, false), viewSQL(state, true))
		fmt.Fprintf(&fp, "%s|%d|%d|%t;", state.schema.Name, state.schema.Version, state.incarnation.DropGen, state.native.Metadata().CurrentSnapshot() != nil)
		for _, f := range state.schema.Fields {
			if t, seen := labels[f.Name]; seen && t != f.Type {
				ambiguous[f.Name] = true
			}
			labels[f.Name] = f.Type
		}
	}
	for name := range ambiguous {
		delete(labels, name)
	}
	return views, fp.String(), labels, nil
}

func queryArg(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "n", nil
	case bool:
		if x {
			return "b1", nil
		}
		return "b0", nil
	case string:
		return "s" + x, nil
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return "i" + strconv.FormatInt(i, 10), nil
		}
		f, err := x.Float64()
		if err != nil {
			return "", invalidf("query parameter %q is not a number", x)
		}
		return "f" + strconv.FormatFloat(f, 'g', -1, 64), nil
	case float64:
		return "f" + strconv.FormatFloat(x, 'g', -1, 64), nil
	case float32:
		return "f" + strconv.FormatFloat(float64(x), 'g', -1, 32), nil
	case int:
		return "i" + strconv.Itoa(x), nil
	case int64:
		return "i" + strconv.FormatInt(x, 10), nil
	}
	return "", invalidf("query parameters must be strings, numbers, booleans or null, not %T", v)
}

func decodeCell(raw string) (any, error) {
	if raw == "" {
		return nil, fmt.Errorf("empty cell")
	}
	body := raw[1:]
	switch raw[0] {
	case 'n':
		return nil, nil
	case 'b':
		return body == "1", nil
	case 'i':
		return strconv.ParseInt(body, 10, 64)
	case 'f':
		return strconv.ParseFloat(body, 64)
	case 'd':
		return json.Number(body), nil
	case 'x':
		return hex.DecodeString(body)
	case 's':
		return body, nil
	}
	return nil, fmt.Errorf("unknown cell tag %q", raw[0])
}

func sidecarError(fields []string) error {
	class, msg := "", ""
	if len(fields) > 2 {
		class = fields[2]
	}
	if len(fields) > 3 {
		msg = fields[3]
	}
	switch class {
	case "query_error":
		return derr.New(derr.Query, "lakehouse SQL (DuckDB dialect) failed: %s", msg)
	case "invalid":
		return invalidf("multiple statements are not allowed; send one SELECT per query")
	case "canceled":
		return context.Canceled
	case "resource":
		return derr.New(derr.Query, "the query ran out of the SQL engine's memory budget; narrow it (fewer rows, columns or groups): %s", msg)
	case "too_large":
		return invalidf("query result exceeds the %d MiB response budget on its first row; select fewer or smaller columns", store.MaxQueryBytes>>20)
	}
	return sqlUnavailable("the SQL sidecar failed: %s", msg)
}

func parseQueryReply(fields []string) (store.QueryResult, error) {
	var result store.QueryResult
	if len(fields) < 2 {
		return result, sqlUnavailable("the SQL sidecar sent a malformed reply")
	}
	if fields[1] == "error" {
		return result, sidecarError(fields)
	}
	bad := func() (store.QueryResult, error) {
		return store.QueryResult{}, sqlUnavailable("the SQL sidecar sent a malformed query reply")
	}
	if fields[1] != "ok" || len(fields) < 4 {
		return bad()
	}
	result.Truncated = fields[2] == "1"
	ncols, err := strconv.Atoi(fields[3])
	if err != nil || ncols < 0 || len(fields) < 5+2*ncols {
		return bad()
	}
	names := make([]string, ncols)
	for c := 0; c < ncols; c++ {
		names[c] = fields[4+2*c]
	}
	pos := 4 + 2*ncols
	nrows, err := strconv.Atoi(fields[pos])
	if err != nil || nrows < 0 || len(fields) != pos+1+nrows*ncols {
		return bad()
	}
	pos++
	result.Rows = make([]map[string]any, nrows)
	for r := 0; r < nrows; r++ {
		row := make(map[string]any, ncols)
		for c := 0; c < ncols; c++ {
			v, err := decodeCell(fields[pos])
			if err != nil {
				return bad()
			}
			row[names[c]] = v
			pos++
		}
		result.Rows[r] = row
	}
	return result, nil
}

func (s *Store) ensureSidecar(ctx context.Context, n *namespace, ns string) (*sidecar, map[string]schema.FieldType, error) {
	views, fp, labels, err := s.namespaceViews(ctx, n, ns)
	if err != nil {
		return nil, nil, err
	}
	if n.sql != nil && (n.sql.fingerprint != fp || !n.sql.alive()) {
		n.sql.stop()
		n.sql = nil
	}
	if n.sql == nil {
		if n.sql, err = startSidecar(ctx, s.sqlEngine, n.dataDir, views, fp); err != nil {
			return nil, nil, err
		}
	}
	return n.sql, labels, nil
}

func (s *Store) Query(ctx context.Context, ns, sql string, args []any, nsGen [16]byte, page store.Page) (store.QueryResult, error) {
	if err := store.ValidateQueryShape(sql); err != nil {
		return store.QueryResult{}, err
	}
	if len(args) > 100 {
		return store.QueryResult{}, invalidf("too many query parameters")
	}
	if page.Offset < 0 {
		return store.QueryResult{}, invalidf("offset must be non-negative")
	}
	limit := page.Limit
	if limit <= 0 || limit > store.MaxPageLimit {
		limit = store.DefaultPageLimit
	}
	encoded := make([]string, len(args))
	for i, a := range args {
		var err error
		if encoded[i], err = queryArg(a); err != nil {
			return store.QueryResult{}, err
		}
	}
	var sc *sidecar
	var labels map[string]schema.FieldType
	err := s.withNamespace(ctx, ns, func(n *namespace) error {
		if nsGen != [16]byte{} && nsGen != n.generation {
			return fmt.Errorf("%w: namespace %s was replaced; resolve its current state", store.ErrNotFound, ns)
		}
		var err error
		sc, labels, err = s.ensureSidecar(ctx, n, ns)
		return err
	})
	if err != nil {
		return store.QueryResult{}, err
	}
	sc.run.Lock()
	defer sc.run.Unlock()
	fields, err := sc.call(ctx, "query", append([]string{strconv.Itoa(page.Offset), strconv.Itoa(limit), strconv.Itoa(store.MaxQueryBytes), sql}, encoded...)...)
	if err != nil {
		return store.QueryResult{}, err
	}
	result, err := parseQueryReply(fields)
	if err != nil {
		return result, err
	}
	for _, row := range result.Rows {
		for label, v := range row {
			if t, ok := labels[label]; ok {
				row[label] = presentQueryValue(t, v)
			}
		}
	}
	return result, nil
}

func presentQueryValue(t schema.FieldType, v any) any {
	if t == schema.Number {
		if f, ok := v.(float64); ok && f == math.Trunc(f) && math.Abs(f) < 1<<53 {
			return int64(f)
		}
		return v
	}
	if t == schema.Secret {
		return v
	}
	return value.Decode(t, v)
}

func (s *Store) Capabilities() store.EngineCapabilities {
	return store.EngineCapabilities{
		VectorExecution: store.VectorExact,
		QueryDialect:    DialectDuckDB,
		FilterDialect:   DialectDuckDB,
	}
}

func publishCurrentMetadata(metadataPath string) error {
	raw, err := os.ReadFile(metadataPath)
	if err != nil {
		return err
	}
	dir := filepath.Dir(metadataPath)
	tmp, err := os.CreateTemp(dir, ".dolmen-current-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(raw)
	err = errors.Join(err, tmp.Sync(), tmp.Close())
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(dir, currentMetadataName))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}
