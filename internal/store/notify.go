package store

import (
	"log/slog"
	"sync/atomic"
	"time"
)

type commitListener struct {
	fn   func(table string, changes ChangeRange)
	dead atomic.Bool
}

func (s *Store) onCommit(ns string, fn func(table string, changes ChangeRange)) func() {
	l := &commitListener{fn: fn}
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	if s.listeners == nil {
		s.listeners = map[string][]*commitListener{}
	}
	s.listeners[ns] = append(s.listeners[ns], l)
	return func() {
		l.dead.Store(true)
		s.notifyMu.Lock()
		defer s.notifyMu.Unlock()
		ls := s.listeners[ns]
		for i, cand := range ls {
			if cand == l {
				s.listeners[ns] = append(ls[:i], ls[i+1:]...)
				break
			}
		}
	}
}

func (s *Store) trackSession(sess *listenSession) {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	if s.listenSessions == nil {
		s.listenSessions = map[string][]*listenSession{}
	}
	s.listenSessions[sess.nsName] = append(s.listenSessions[sess.nsName], sess)
}

func (s *Store) untrackSession(sess *listenSession) {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	sessions := s.listenSessions[sess.nsName]
	for i, cand := range sessions {
		if cand == sess {
			s.listenSessions[sess.nsName] = append(sessions[:i], sessions[i+1:]...)
			break
		}
	}
}

func (s *Store) wakeListenSessions(ns string) {
	s.notifyMu.Lock()
	sessions := append([]*listenSession(nil), s.listenSessions[ns]...)
	s.notifyMu.Unlock()
	for _, sess := range sessions {
		sess.wake("", ChangeRange{})
	}
}

func (s *Store) endListenSessions(ns string, cause error) {
	s.notifyMu.Lock()
	sessions := append([]*listenSession(nil), s.listenSessions[ns]...)
	s.notifyMu.Unlock()
	for _, sess := range sessions {
		sess.endParked(cause)
	}
}

const nsPruneInterval = listenPollInterval

func (s *Store) pruneDue(ns string, now time.Time) bool {
	if s.changeRetention <= 0 {
		return false
	}
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	if s.pruneNext == nil {
		s.pruneNext = map[string]time.Time{}
	}
	if now.Before(s.pruneNext[ns]) {
		return false
	}
	s.pruneNext[ns] = now.Add(nsPruneInterval)
	return true
}

func (s *Store) notifyCommitted(ns, table string, changes ChangeRange) {
	if changes.Count == 0 {
		return
	}
	s.notifyMu.Lock()
	listeners := append([]*commitListener(nil), s.listeners[ns]...)
	s.notifyMu.Unlock()
	for _, l := range listeners {
		if l.dead.Load() {
			continue
		}
		deliverCommit(l, ns, table, changes)
	}
}

func deliverCommit(l *commitListener, ns, table string, changes ChangeRange) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("post-commit listener panicked; write already committed, wake dropped",
				"namespace", ns, "table", table,
				"changes_first", changes.First, "changes_last", changes.Last,
				"panic", r)
		}
	}()
	l.fn(table, changes)
}
