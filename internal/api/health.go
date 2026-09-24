package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

var errDraining = errors.New("the server is shutting down")

type drainState struct {
	once     sync.Once
	ch       chan struct{}
	draining atomic.Bool
	inflight atomic.Int64
}

type ReadyChecker interface {
	Ready(ctx context.Context) error
}

const readyProbeTimeout = 2 * time.Second

func (s *Server) drainCh() chan struct{} {
	s.drain.once.Do(func() { s.drain.ch = make(chan struct{}) })
	return s.drain.ch
}

func (s *Server) Drain() {
	ch := s.drainCh()
	if s.drain.draining.CompareAndSwap(false, true) {
		close(ch)
	}
}

func (s *Server) Inflight() int64 {
	return s.drain.inflight.Load()
}

func (s *Server) Track(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.drain.inflight.Add(1)
		defer s.drain.inflight.Add(-1)
		h.ServeHTTP(w, r)
	})
}

func (s *Server) embeddingStatus() map[string]any {
	name := "none"
	if s.emb != nil {
		name = s.emb.Name()
	}
	status := "configured"
	if name == "none" {
		status = "none"
	}
	return map[string]any{"provider": name, "status": status}
}

func (s *Server) handleLivez(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	reasons := []string{}
	if s.drain.draining.Load() {
		reasons = append(reasons, "draining: the server is shutting down and takes no new work")
	}
	if rc, ok := s.eng.(ReadyChecker); ok {
		ctx, cancel := context.WithTimeout(r.Context(), readyProbeTimeout)
		err := rc.Ready(ctx)
		cancel()
		if err != nil {
			reasons = append(reasons, redactPaths(err.Error()))
		}
	}
	body := map[string]any{"status": "ready", "embedding": s.embeddingStatus()}
	if len(reasons) > 0 {
		body["status"] = "not_ready"
		body["reasons"] = reasons
		writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}
	writeJSON(w, http.StatusOK, body)
}
