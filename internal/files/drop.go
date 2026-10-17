package files

import "context"

func (s *Store) DropNamespace(ctx context.Context, ns string) error {
	s.mu.Lock()
	delete(s.totals, ns)
	delete(s.attaches, ns)
	s.mu.Unlock()
	return s.blobs.DropNamespace(ctx, ns)
}
