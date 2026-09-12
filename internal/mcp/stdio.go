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

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/version"
	"github.com/lsm/dolmen/skill"
)

const stdioMaxLine = 32 << 20

type stdioLine struct {
	data []byte
	err  error
}

func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	instr := s.stdioInstructions()
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
			return nil
		case line, ok := <-lines:
			if !ok {
				return nil
			}
			if line.err != nil {
				if errors.Is(line.err, bufio.ErrTooLong) {
					_ = enc.Encode(rpcErrorEnvelope(nil, jsonRPCParseError, fmt.Sprintf("stdio line exceeds the %d MiB limit", stdioMaxLine>>20)))
				}
				return fmt.Errorf("read stdin: %w", line.err)
			}
			msg, msgErr := parseMessage(bytes.TrimSpace(line.data))
			if msgErr != nil {
				if err := enc.Encode(rpcErrorEnvelope(msgErr.ID, msgErr.Code, msgErr.Message)); err != nil {
					return err
				}
				continue
			}
			if len(msg.ID) == 0 {
				continue
			}
			result, rpcErr := s.handle(api.WithRequestID(ctx, api.NewRequestID()), msg, instr)
			resp := rpcResultEnvelope(msg.ID, result)
			if rpcErr != nil {
				resp = rpcErrorEnvelope(msg.ID, rpcErr.Code, rpcErr.Message)
			}
			if err := enc.Encode(resp); err != nil {
				return err
			}
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
