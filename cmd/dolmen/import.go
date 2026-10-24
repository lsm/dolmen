package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type importChunk struct {
	Namespace string            `json:"namespace"`
	Table     string            `json:"table"`
	Key       string            `json:"idempotency_key"`
	Records   []json.RawMessage `json:"records"`
}

func runImport(args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("dolmen import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	endpoint := fs.String("url", "http://127.0.0.1:8790", "server URL, including any path prefix")
	ns := fs.String("namespace", "", "existing target namespace")
	table := fs.String("table", "", "existing target table")
	snapshot := fs.String("snapshot", "", "immutable export name; reuse only with identical input and chunk options")
	rows := fs.Int("rows", 1000, "maximum records per chunk (1-1000)")
	budget := fs.Int("bytes", 32<<20, "maximum encoded request bytes (up to 33554432)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "dolmen import -url URL -namespace NAME -table NAME -snapshot NAME [-rows N] [-bytes N] < records.jsonl\nCredential: DOLMEN_TOKEN environment variable; no credential flag.")
			fs.SetOutput(stderr)
			fs.PrintDefaults()
			return err
		}
		return errors.New("import options: use -help for flags; credentials belong in DOLMEN_TOKEN")
	}
	u, err := url.Parse(*endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("import option url: use an HTTP(S) server URL without credentials, query or fragment")
	}
	keyPrefix := *snapshot + "/" + *table + "/"
	if fs.NArg() != 0 || *ns == "" || *table == "" || *snapshot == "" || *rows < 1 || *rows > 1000 || *budget < 2 || *budget > 32<<20 || len(keyPrefix)+20 > 256 || strings.IndexFunc(keyPrefix, func(r rune) bool { return r < 32 || r > 126 }) >= 0 {
		return errors.New("import options: namespace, table and snapshot are required; rows must be 1-1000, bytes 2-33554432, and snapshot/table key prefix printable ASCII within 236 bytes")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/import_records"
	client := &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	chunk, line, first := 1, 0, 1
	request := importChunk{Namespace: *ns, Table: *table, Records: []json.RawMessage{}}
	size := 0
	reset := func() {
		request.Key = keyPrefix + strconv.Itoa(chunk)
		request.Records = request.Records[:0]
		b, _ := json.Marshal(request)
		size = len(b)
	}
	reset()
	flush := func() error {
		if len(request.Records) == 0 {
			return nil
		}
		body, _ := json.Marshal(request)
		req, e := http.NewRequest(http.MethodPost, u.String(), bytes.NewReader(body))
		if e != nil {
			return fmt.Errorf("line %d field request: cannot construct request", first)
		}
		req.Header.Set("Content-Type", "application/json")
		if token := getenv("DOLMEN_TOKEN"); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, e := client.Do(req)
		if e != nil {
			return fmt.Errorf("line %d field request: transport failed; retry the same export and chunk options", first)
		}
		defer res.Body.Close()
		var receipt struct {
			OK   bool `json:"ok"`
			Data *struct {
				Inserted int64 `json:"inserted"`
				Skipped  int64 `json:"skipped"`
				Replayed bool  `json:"replayed"`
			} `json:"data"`
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&receipt) != nil {
			return fmt.Errorf("line %d field response: invalid receipt (HTTP %d)", first, res.StatusCode)
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 || !receipt.OK || receipt.Data == nil {
			at, field := importErrorLocation(receipt.Error.Message, first, len(request.Records))
			code := "request_failed"
			switch receipt.Error.Code {
			case "invalid_request", "not_found", "conflict", "unauthorized", "forbidden", "timeout", "canceled", "internal_error", "storage_unavailable", "embedder_unavailable", "sql_engine_unavailable":
				code = receipt.Error.Code
			}
			return fmt.Errorf("line %d field %s: %s (HTTP %d); inspect source/schema before retrying", at, field, code, res.StatusCode)
		}
		if _, e = fmt.Fprintf(stdout, "committed chunk %d inserted=%d skipped=%d replayed=%t\n", chunk, receipt.Data.Inserted, receipt.Data.Skipped, receipt.Data.Replayed); e != nil {
			return errors.New("field output: cannot report committed chunk; retry with the same export and chunk options")
		}
		chunk++
		reset()
		return nil
	}
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 4096), (32<<20)+1)
	for scanner.Scan() {
		line++
		var row map[string]any
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.UseNumber()
		if decoder.Decode(&row) != nil || row == nil || decoder.Decode(new(any)) != io.EOF {
			return fmt.Errorf("line %d field record: expected exactly one JSON object", line)
		}
		encoded, e := json.Marshal(row)
		if e != nil {
			return fmt.Errorf("line %d field record: cannot encode object", line)
		}
		if len(request.Records) > 0 && size+1+len(encoded) > *budget {
			if err := flush(); err != nil {
				return err
			}
		}
		if size+len(encoded) > *budget {
			return fmt.Errorf("line %d field record: encoded row plus request envelope exceeds bytes limit", line)
		}
		if len(request.Records) == 0 {
			first = line
		} else {
			size++
		}
		request.Records = append(request.Records, encoded)
		size += len(encoded)
		if len(request.Records) == *rows {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if scanner.Err() != nil {
		return fmt.Errorf("line %d field record: input unreadable or line exceeds 32 MiB", line+1)
	}
	if line == 0 {
		return errors.New("line 1 field record: expected JSONL input")
	}
	return flush()
}

var importRecordLocation = regexp.MustCompile(`^import record ([0-9]+)`)
var importFieldLocation = regexp.MustCompile(`^import record [0-9]+: (?:unknown )?field "([a-zA-Z_][a-zA-Z0-9_]*)"`)

func importErrorLocation(message string, first, count int) (int, string) {
	field := "record"
	if match := importRecordLocation.FindStringSubmatch(message); match != nil {
		if index, err := strconv.Atoi(match[1]); err == nil && index < count {
			first += index
		}
		if match := importFieldLocation.FindStringSubmatch(message); match != nil {
			field = match[1]
		}
		if strings.Contains(message, "requires a distinct positive int64 id") {
			field = "id"
		}
		if strings.Contains(message, "requires created_at") {
			field = "created_at"
		}
	}
	return first, field
}
