package sidecar

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Op string

const (
	OpInit     Op = "init"
	OpQuery    Op = "query"
	OpCancel   Op = "cancel"
	OpShutdown Op = "shutdown"
)

type Options struct {
	Bin       string
	Args      []string
	DataDir   string
	Env       []string
	MemoryMax int64
	StartWait time.Duration
}

type Sidecar struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	mu        sync.Mutex
	writeMu   sync.Mutex
	nextID    int64
	closed    bool
	stray     int
	lastStray string
}

type Result struct {
	Columns   []Column
	Rows      []string
	ElapsedMS int64
	Truncated bool
}

type Column struct {
	Name string
	Type string
}

type QueryError struct {
	Class   string
	Message string
}

func (e *QueryError) Error() string {
	return e.Class + ": " + e.Message
}

func (e *QueryError) IsConfinement() bool {
	return e.Class == "not_supported"
}

func Start(ctx context.Context, opts Options) (*Sidecar, error) {
	if opts.Bin == "" {
		return nil, errors.New("sidecar: a binary path is required")
	}
	cmd := exec.Command(opts.Bin, opts.Args...)
	cmd.Env = append(os.Environ(), opts.Env...)
	if opts.MemoryMax > 0 {
		cmd.Env = append(cmd.Env, "SIDECAR_MEMORY_MAX="+strconv.FormatInt(opts.MemoryMax, 10))
	}
	if opts.DataDir != "" {
		cmd.Dir = opts.DataDir
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &Sidecar{cmd: cmd, stdin: stdin, stdout: bufio.NewReaderSize(stdout, 1<<20), nextID: 1000}
	return s, nil
}

func (s *Sidecar) call(ctx context.Context, op Op, args ...string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := s.nextID
	fields := append([]string{strconv.FormatInt(id, 10), string(op)}, args...)
	if err := s.write(fields); err != nil {
		return "", err
	}
	return s.readResponse(ctx, strconv.FormatInt(id, 10))
}

func (s *Sidecar) readLine(ctx context.Context) (string, error) {
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		line, err := s.stdout.ReadString('\n')
		ch <- res{line, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return "", r.err
		}
		return r.line, nil
	}
}

const maxStrayLines = 64

func (s *Sidecar) readResponse(ctx context.Context, wantID string) (string, error) {
	for i := 0; i < maxStrayLines; i++ {
		line, err := s.readLine(ctx)
		if err != nil {
			return "", err
		}
		fields := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
		if len(fields) == 0 {
			continue
		}
		if fields[0] == wantID || wantID == "0" {
			return line, nil
		}
		s.stray++
		s.lastStray = line
	}
	return "", fmt.Errorf("sidecar: no response for request %s in %d lines (last stray %q)", wantID, maxStrayLines, s.lastStray)
}

func (s *Sidecar) StrayLines() int { return s.stray }

func (s *Sidecar) LastStrayLine() string { return s.lastStray }

func parseAck(line, wantID string) (string, error) {
	fields := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
	if len(fields) < 2 {
		return "", fmt.Errorf("sidecar: short response %q", line)
	}
	if fields[0] != wantID {
		return "", fmt.Errorf("sidecar: response id %q does not match request %q", fields[0], wantID)
	}
	if fields[1] == "error" {
		return "", errorFrom(fields, line)
	}
	return line, nil
}

func errorFrom(fields []string, line string) error {
	class, msg := "", ""
	if len(fields) > 2 {
		class = fields[2]
	}
	if len(fields) > 3 {
		msg = strings.Join(fields[3:], "\t")
	}
	if class == "" {
		class = "internal_error"
	}
	return &QueryError{Class: class, Message: msg}
}

func (s *Sidecar) write(fields []string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := fmt.Fprintln(s.stdin, strings.Join(fields, "\t"))
	return err
}

func (s *Sidecar) SendFireAndForget(op Op, args ...string) error {
	return s.write(append([]string{"0", string(op)}, args...))
}

func (s *Sidecar) Init(ctx context.Context, dataDir string, snapshot int64, table, location string) error {
	s.mu.Lock()
	wantID := strconv.FormatInt(s.nextID+1, 10)
	s.mu.Unlock()
	line, err := s.call(ctx, OpInit, dataDir, strconv.FormatInt(snapshot, 10), table, location)
	if err != nil {
		return err
	}
	ack, err := parseAck(line, wantID)
	if err != nil {
		return err
	}
	fields := strings.Split(strings.TrimRight(ack, "\r\n"), "\t")
	if len(fields) < 3 || strings.TrimSpace(fields[2]) == "" {
		return fmt.Errorf("sidecar: init for %s at %d was acknowledged with no payload (%q), so nothing can be assumed to be registered", table, snapshot, ack)
	}
	return nil
}

func (s *Sidecar) Query(ctx context.Context, sql string) (*Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := s.nextID
	if err := s.write([]string{strconv.FormatInt(id, 10), string(OpQuery), sql}); err != nil {
		return nil, err
	}
	line, err := s.readResponse(ctx, strconv.FormatInt(id, 10))
	if err != nil {
		return nil, err
	}
	return parseResult(line, strconv.FormatInt(id, 10))
}

func parseResult(line, wantID string) (*Result, error) {
	fields := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
	if len(fields) < 3 {
		return nil, fmt.Errorf("sidecar: short result %q", line)
	}
	if fields[0] != wantID {
		return nil, fmt.Errorf("sidecar: result id %q does not match %q", fields[0], wantID)
	}
	if fields[1] == "error" {
		return nil, errorFrom(fields, line)
	}
	out := &Result{}
	for _, c := range strings.Split(fields[2], ";") {
		if c == "" {
			continue
		}
		name, typ, _ := strings.Cut(c, ":")
		out.Columns = append(out.Columns, Column{Name: name, Type: typ})
	}
	if len(fields) > 3 && fields[3] != "" {
		out.Rows = strings.Split(fields[3], "|")
	}
	if len(fields) > 4 {
		if v, err := strconv.ParseInt(fields[4], 10, 64); err == nil {
			out.ElapsedMS = v
		}
	}
	if len(fields) > 5 {
		out.Truncated = fields[5] == "1"
	}
	return out, nil
}

func (s *Sidecar) Cancel() error { return s.SendFireAndForget(OpCancel) }

func (s *Sidecar) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.call(ctx, OpShutdown)
	_ = s.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		return s.Kill()
	}
}

func (s *Sidecar) Kill() error {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	return s.cmd.Wait()
}

func (s *Sidecar) Pid() int {
	if s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

func Escape(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "|", `\|`)
	return strings.ReplaceAll(v, "\n", `\n`)
}
