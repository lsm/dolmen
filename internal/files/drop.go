package files

import "context"

func (s *Store) DropNamespace(ctx context.Context, ns string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.blobs.DropNamespace(ctx, ns); err != nil {
		return err
	}
	delete(s.totals, ns)
	delete(s.attaches, ns)
	return nil
}
