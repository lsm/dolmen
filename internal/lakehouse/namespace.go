package lakehouse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/store"
)

func (s *Store) NamespaceState(ctx context.Context, name string, auth []store.AuthBinding) (gen [16]byte, err error) {
	if len(auth) != 0 {
		return gen, derr.New(derr.Forbidden, "lakehouse authorization bindings are not implemented yet")
	}
	err = s.withNamespace(ctx, name, func(n *namespace) error { gen = n.generation; return nil })
	return gen, err
}

func (s *Store) CreateNamespace(ctx context.Context, name string, parentGen [16]byte) error {
	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "CREATE", trace.WithAttributes(attribute.String("db.operation.name", "CREATE"), attribute.String("db.namespace", name)))
		defer span.End()
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	if err := store.ValidateNamespace(name); err != nil {
		return err
	}
	if parentGen != [16]byte{} {
		pos := strings.LastIndexByte(name, '/')
		if pos < 0 {
			return fmt.Errorf("%w: root namespace has no parent", store.ErrInvalid)
		}
		parent, err := s.openNamespace(ctx, name[:pos])
		if err != nil {
			return err
		}
		if parent.generation != parentGen {
			return fmt.Errorf("%w: parent namespace was replaced; resolve its current state", store.ErrNotFound)
		}
	}
	if err := s.checkHierarchy(name, true); err != nil {
		return err
	}
	path := namespacePath(name)
	if _, err := s.root.Lstat(path); err == nil {
		return fmt.Errorf("%w: namespace %s %w", store.ErrInvalid, name, store.ErrExists)
	} else if !os.IsNotExist(err) {
		return err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	stage := filepath.Join(filepath.Dir(path), ".lakehouse-create-"+hex.EncodeToString(token[:]))
	if err := s.root.Mkdir(stage, 0o700); err != nil {
		return err
	}
	defer s.root.RemoveAll(stage)
	if err := s.root.Mkdir(filepath.Join(stage, "data"), 0o700); err != nil {
		return err
	}
	f, err := s.root.OpenFile(filepath.Join(stage, "catalog.db"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	n, err := s.openCatalog(ctx, name, stage, true)
	if err != nil {
		return err
	}
	if err := n.db.Close(); err != nil {
		return err
	}
	if err := s.syncDirectory(stage); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.renameNamespace(stage, path); err != nil {
		return err
	}
	return s.syncDirectory(filepath.Dir(path))
}

func (s *Store) ListNamespaces(ctx context.Context, prefix string, auth []store.AuthBinding) ([]string, error) {
	if len(auth) != 0 {
		return nil, derr.New(derr.Forbidden, "lakehouse authorization bindings are not implemented yet")
	}
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer s.unlock()
	if prefix != "" {
		if err := store.ValidateNamespace(prefix); err != nil {
			return nil, err
		}
	}
	return s.listNamespaces(ctx, prefix)
}

func (s *Store) listNamespaces(ctx context.Context, prefix string) ([]string, error) {
	out := []string{}
	var walk func(string, string, int) error
	walk = func(dir, parent string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := s.root.Open(dir)
		if err != nil {
			return err
		}
		entries, err := f.ReadDir(-1)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			stem := strings.TrimSuffix(entry.Name(), ".lakehouse")
			name := stem
			if parent != "" {
				name = parent + "/" + stem
			}
			if store.ValidateNamespace(name) != nil {
				continue
			}
			if stem != entry.Name() {
				if prefix == "" || name == prefix || strings.HasPrefix(name, prefix+"/") {
					if err := s.checkNamespaceFiles(name); err != nil {
						if os.IsNotExist(err) || errors.Is(err, store.ErrCatalogCorrupt) || errors.Is(err, store.ErrInvalid) {
							continue
						}
						return err
					}
					out = append(out, name)
				}
			} else if depth < 3 {
				if err := walk(filepath.Join(dir, entry.Name()), name, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(".", "", 1); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i]+".db" < out[j]+".db" })
	return out, nil
}

func (s *Store) DropNamespace(ctx context.Context, name string, expected [16]byte) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	if err := s.checkDroppable(ctx, name, expected); err != nil {
		s.unlock()
		return err
	}
	if s.dropping == nil {
		s.dropping = map[string]bool{}
	}
	s.dropping[name] = true
	if n := s.namespaces[name]; n != nil {
		n.sql = nil
	}
	draining := s.tracked(name)
	s.unlock()
	s.drainAll(name, draining, true)
	<-s.gate
	defer s.unlock()
	delete(s.dropping, name)
	if s.closed {
		return store.ErrClosed
	}
	if err := s.checkDroppable(ctx, name, expected); err != nil {
		return err
	}
	if err := s.evict(name); err != nil {
		return err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	path := namespacePath(name)
	trash := filepath.Join(filepath.Dir(path), ".lakehouse-drop-"+hex.EncodeToString(token[:]))
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.renameNamespace(path, trash); err != nil {
		return err
	}
	if err := s.syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return s.root.RemoveAll(trash)
}

func (s *Store) checkDroppable(ctx context.Context, name string, expected [16]byte) error {
	if err := store.ValidateNamespace(name); err != nil {
		return err
	}
	if expected != [16]byte{} {
		n, err := s.openNamespace(ctx, name)
		if err != nil {
			return err
		}
		if expected != n.generation {
			return fmt.Errorf("%w: namespace %s was replaced; resolve its current state", store.ErrNotFound, name)
		}
	} else if err := s.checkNamespaceDir(name); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: namespace %s does not exist, so nothing was dropped; list_namespaces shows what is there", store.ErrNotFound, name)
		}
		return err
	}
	names, err := s.listNamespaces(ctx, name)
	if err != nil {
		return err
	}
	children := 0
	for _, child := range names {
		if child != name {
			children++
		}
	}
	if children > 0 {
		return fmt.Errorf("%w: namespace %s has %d descendant namespaces — drop the children first", store.ErrInvalid, name, children)
	}
	return nil
}
