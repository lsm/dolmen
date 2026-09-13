package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/version"
	"github.com/lsm/dolmen/skill"
)

const stdioMaxLine = 32 << 20

const stdioDrainTimeout = 5 * time.Second

type stdioLine struct {
	data []byte
	err  error
}

type stdioCancels struct {
	mu   sync.Mutex
	byID map[string]context.CancelFunc
}

func newStdioCancels() *stdioCancels {
	return &stdioCancels{byID: map[string]context.CancelFunc{}}
}

func (c *stdioCancels) add(key string, cancel context.CancelFunc) {
	c.mu.Lock()
	c.byID[key] = cancel
	c.mu.Unlock()
}

func (c *stdioCancels) remove(key string) {
	c.mu.Lock()
	delete(c.byID, key)
	c.mu.Unlock()
}

func (c *stdioCancels) cancel(key string) bool {
	c.mu.Lock()
	cancel, ok := c.byID[key]
	c.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

func (c *stdioCancels) cancelAll() {
	c.mu.Lock()
	for _, cancel := range c.byID {
		cancel()
	}
	c.mu.Unlock()
}

func cancelledRequestKey(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := dec.Decode(&params); err != nil || len(params.RequestID) == 0 {
		return ""
	}
	return requestIDKey(params.RequestID)
}

func requestIDKey(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || v == nil {
		return ""
	}
	key, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(key)
}

func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	instr := s.stdioInstructions()
	cancels := newStdioCancels()
	var writeMu sync.Mutex
	writeErr := make(chan error, 1)
	write := func(resp map[string]any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := enc.Encode(resp); err != nil {
			select {
			case writeErr <- err:
			default:
			}
		}
	}
	var inflight sync.WaitGroup
	drain := func() {
		drained := make(chan struct{})
		go func() {
			inflight.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-time.After(stdioDrainTimeout):
			cancels.cancelAll()
			inflight.Wait()
		}
	}
	lines := make(chan stdioLine)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 0, 64*1024), stdioMaxLine)
		for sc.Scan() {
			lines <- stdioLine{data: append([]byte(nil), sc.Bytes()...)}
		}
		if err := sc.Err(); err != nil {
			lines <- stdioLine{err: err}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			drain()
			return nil
		case err := <-writeErr:
			drain()
			return err
		case line, ok := <-lines:
			if !ok {
				drain()
				return nil
			}
			if line.err != nil {
				if errors.Is(line.err, bufio.ErrTooLong) {
					write(rpcErrorEnvelope(nil, jsonRPCParseError, fmt.Sprintf("stdio line exceeds the %d MiB limit", stdioMaxLine>>20)))
				}
				drain()
				return fmt.Errorf("read stdin: %w", line.err)
			}
			trimmed := bytes.TrimSpace(line.data)
			if len(trimmed) == 0 {
				continue
			}
			msg, msgErr := parseMessage(trimmed)
			if msgErr != nil {
				write(rpcErrorEnvelope(msgErr.ID, msgErr.Code, msgErr.Message))
				continue
			}
			if len(msg.ID) == 0 {
				if msg.Method == "notifications/cancelled" {
					if key := cancelledRequestKey(msg.Params); key != "" {
						cancels.cancel(key)
					}
				}
				continue
			}
			key := requestIDKey(msg.ID)
			reqCtx, cancelReq := context.WithCancel(ctx)
			cancels.add(key, cancelReq)
			inflight.Add(1)
			go func(msg rpcMessage) {
				defer inflight.Done()
				defer cancels.remove(key)
				defer cancelReq()
				result, rpcErr := s.handle(api.WithRequestID(reqCtx, api.NewRequestID()), msg, instr)
				resp := rpcResultEnvelope(msg.ID, result)
				if rpcErr != nil {
					resp = rpcErrorEnvelope(msg.ID, rpcErr.Code, rpcErr.Message)
				}
				write(resp)
			}(msg)
		}
	}
}

func (s *Server) stdioInstructions() string {
	hint := s.namespaceHint
	if hint == "" {
		hint = skill.DefaultNamespaceHint
	}
	base := s.baseURL
	if p := skill.NormalizePrefix(s.prefix); p != "" && base != "" && !strings.HasSuffix(base, p) {
		base += p
	}
	return skill.StdioInstructions(skill.Context{BaseURL: base, Version: version.Version, NamespaceHint: hint})
}
