package lakehouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	sqlcatalog "github.com/apache/iceberg-go/catalog/sql"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
)

const DefaultMaxOpenNamespaces = 16

var owners = struct {
	sync.Mutex
	dirs map[string]bool
}{dirs: map[string]bool{}}

type Store struct {
	secrets    *secret.Keyring
	sqlEngine  SQLEngine
	dir        string
	root       *os.Root
	gate       chan struct{}
	namespaces map[string]*namespace
	maxOpen    int
	tick       uint64
	closed     bool
	closeErr   error
	dropping   map[string]bool
	sidecarMu  sync.Mutex
	sidecars   map[string]map[*sidecar]bool
}

type namespace struct {
	db         *sql.DB
	catalog    *sqlcatalog.Catalog
	generation [16]byte
	dataDir    string
	lastUse    uint64
	pending    int
	sql        *sidecar
	sqlErr     error
	sqlRetry   time.Time
	sqlFails   int
	published  map[string]string
}

type OpenOption func(*Store)

func WithMaxOpenNamespaces(n int) OpenOption {
	return func(s *Store) { s.maxOpen = n }
}

func Open(dir string, opts ...OpenOption) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: abs, gate: make(chan struct{}, 1), namespaces: map[string]*namespace{}, maxOpen: DefaultMaxOpenNamespaces}
	for _, opt := range opts {
		opt(s)
	}
	if s.maxOpen < 1 {
		return nil, fmt.Errorf("%w: max open lakehouse namespaces must be at least 1", store.ErrInvalid)
	}
	owners.Lock()
	defer owners.Unlock()
	if owners.dirs[abs] {
		return nil, fmt.Errorf("%w: lakehouse data directory already open in this process; close its store before reopening", store.ErrInvalid)
	}
	if err := secureDir(abs); err != nil {
		return nil, err
	}
	s.root, err = os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	owners.dirs[abs] = true
	s.gate <- struct{}{}
	return s, nil
}

func (s *Store) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.gate:
	}
	if err := ctx.Err(); err != nil {
		s.unlock()
		return err
	}
	if s.closed {
		s.unlock()
		return store.ErrClosed
	}
	return nil
}

func (s *Store) unlock() { s.gate <- struct{}{} }

func (s *Store) Close() error {
	<-s.gate
	defer s.unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	s.sidecarMu.Lock()
	running := map[string][]*sidecar{}
	for ns, set := range s.sidecars {
		for sc := range set {
			running[ns] = append(running[ns], sc)
		}
	}
	s.sidecarMu.Unlock()
	for ns, scs := range running {
		s.drainAll(ns, scs, true)
	}
	for name, n := range s.namespaces {
		n.sql = nil
		s.closeErr = errors.Join(s.closeErr, n.db.Close())
		delete(s.namespaces, name)
	}
	s.closeErr = errors.Join(s.closeErr, s.root.Close())
	owners.Lock()
	delete(owners.dirs, s.dir)
	owners.Unlock()
	return s.closeErr
}

func (s *Store) withNamespace(ctx context.Context, name string, fn func(*namespace) error) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	if err := store.ValidateNamespace(name); err != nil {
		return err
	}
	n, err := s.openNamespace(ctx, name)
	if err != nil {
		return err
	}
	if err := s.materialize(ctx, n, name); err != nil {
		return err
	}
	return fn(n)
}

func (s *Store) evict(name string) error {
	n := s.namespaces[name]
	if n == nil {
		return nil
	}
	if n.sql != nil {
		s.retire(name, n.sql)
		n.sql = nil
	}
	if err := n.db.Close(); err != nil {
		return err
	}
	delete(s.namespaces, name)
	return nil
}

func (s *Store) makeRoom() error {
	if len(s.namespaces) < s.maxOpen {
		return nil
	}
	var oldest string
	var tick uint64
	for name, n := range s.namespaces {
		if oldest == "" || n.lastUse < tick {
			oldest, tick = name, n.lastUse
		}
	}
	return s.evict(oldest)
}
