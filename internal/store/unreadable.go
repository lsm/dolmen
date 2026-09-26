package store

import (
	"context"
	"fmt"
	"log/slog"
)

var ErrNamespaceUnreadable = fmt.Errorf("namespace file cannot be read")

type unreadableNamespace struct {
	name   string
	reason string
}

func (s *Store) markUnreadable(name string, err error) {
	slog.Warn("namespace is unreadable; requests to it fail until it is repaired", "namespace", name, "err", err)
	reason := ""
	if err != nil {
		reason = err.Error()
	}
	s.unreadableMu.Lock()
	defer s.unreadableMu.Unlock()
	for i, kept := range s.unreadable {
		if kept.name == name {
			s.unreadable[i].reason = reason
			return
		}
	}
	s.unreadable = append(s.unreadable, unreadableNamespace{name: name, reason: reason})
}

func (s *Store) unreadableReason(name string) (string, bool) {
	s.unreadableMu.Lock()
	defer s.unreadableMu.Unlock()
	for _, kept := range s.unreadable {
		if kept.name == name {
			return kept.reason, true
		}
	}
	return "", false
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
	why := ""
	if reason != "" {
		why = " (" + reason + ")"
	}
	return fmt.Errorf("%w: namespace %s cannot be read%s, so no operation on it can be served; restore it from a backup (dolmen restore), or drop_namespace it to start over; the other namespaces in this data directory are unaffected", ErrNamespaceUnreadable, name, why)
}
