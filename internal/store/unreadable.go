package store

import (
	"context"
	"fmt"
	"log/slog"
)

var ErrNamespaceUnreadable = fmt.Errorf("namespace file cannot be read")

type unreadableNamespace struct {
	name   string
	reason error
}

func (s *Store) markUnreadable(name string, err error) {
	slog.Warn("namespace is unreadable; requests to it fail until it is repaired", "namespace", name, "err", err)
	s.unreadableMu.Lock()
	defer s.unreadableMu.Unlock()
	for i, kept := range s.unreadable {
		if kept.name == name {
			s.unreadable[i].reason = err
			return
		}
	}
	s.unreadable = append(s.unreadable, unreadableNamespace{name: name, reason: err})
}

func (s *Store) unreadableReason(name string) (error, bool) {
	s.unreadableMu.Lock()
	defer s.unreadableMu.Unlock()
	for _, kept := range s.unreadable {
		if kept.name == name {
			return kept.reason, true
		}
	}
	return nil, false
}

func (s *Store) markedUnreadable(name string) bool {
	_, marked := s.unreadableReason(name)
	return marked
}

func (s *Store) refuseIfUnreadable(ctx context.Context, name string) error {
	reason, marked := s.unreadableReason(name)
	if !marked {
		return nil
	}
	s.stillUnreadable(ctx)
	reason, marked = s.unreadableReason(name)
	if !marked {
		return nil
	}
	if reason == nil {
		return fmt.Errorf("%w: namespace %s cannot be read, so no operation on it can be served; restore it from a backup (dolmen restore), or drop_namespace it to start over; the other namespaces in this data directory are unaffected", ErrNamespaceUnreadable, name)
	}
	return fmt.Errorf("%w: namespace %s cannot be read (%w), so no operation on it can be served; restore it from a backup (dolmen restore), or drop_namespace it to start over; the other namespaces in this data directory are unaffected", ErrNamespaceUnreadable, name, reason)
}

func preGateNamespace(ctx context.Context, db rowQuerier) bool {
	var tables, meta int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name <> 'sqlite_sequence'`).Scan(&tables); err != nil {
		return false
	}
	if tables == 0 {
		return false
	}
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = '_dolmen_meta'`).Scan(&meta); err != nil {
		return false
	}
	return meta == 0
}
