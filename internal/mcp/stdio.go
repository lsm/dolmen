package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/version"
	"github.com/lsm/dolmen/skill"
)

const stdioMaxLine = 32 << 20

const stdioDrainGrace = 5 * time.Second

const stdioDrainJoinBound = 10 * time.Second

type stdioLine struct {
	data []byte
	err  error
}

func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	instr := s.stdioInstructions()
	inflight := newInflightRequests()
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
	drain := func() {
		if !inflight.drain(context.Background(), stdioDrainGrace, stdioDrainJoinBound) {
			slog.Warn("stdio shutdown abandoned workers past the join bound", "grace", stdioDrainGrace, "join_bound", stdioDrainJoinBound)
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
						inflight.cancelKey(key)
					}
				}
				continue
			}
			inflight.start(context.Background(), msg.ID, func(reqCtx context.Context) {
				result, rpcErr := s.handle(api.WithRequestID(reqCtx, api.NewRequestID()), msg, instr)
				if rpcErr != nil {
					write(rpcErrorEnvelope(msg.ID, rpcErr.Code, rpcErr.Message))
					return
				}
				write(rpcResultEnvelope(msg.ID, result))
			})
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
