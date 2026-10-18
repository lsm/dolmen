package files

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/lsm/dolmen/internal/derr"
)

func (s *Store) DropNamespace(ctx context.Context, ns string) error {
	s.mu.Lock()
	delete(s.totals, ns)
	delete(s.attaches, ns)
	s.drops[ns]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.drops[ns]--; s.drops[ns] == 0 {
			delete(s.drops, ns)
		}
		s.mu.Unlock()
	}()
	err := s.blobs.DropNamespace(ctx, ns)
	if err == nil {
		return nil
	}
	where, ok := s.blobs.(interface{ Location(ns string) string })
	if !ok {
		return err
	}
	var fe *Error
	if !errors.As(err, &fe) {
		fe = &Error{Status: http.StatusServiceUnavailable, Code: derr.StorageUnavailable}
	}
	return &Error{Status: fe.Status, Code: fe.Code, Message: fmt.Sprintf("namespace %s was dropped, but not all of its files could be deleted from the file store (%s); its rows are gone, so remove what is left under %s with your S3 tooling", ns, err, where.Location(ns))}
}
