package files

import "os"

func (s *Store) DropNamespace(ns string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(s.nsDir(ns)); err != nil {
		return err
	}
	delete(s.totals, ns)
	delete(s.attaches, ns)
	return nil
}
