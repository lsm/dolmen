package dolmen

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/telemetry"
)

var ErrClosed = errors.New("dolmen: store is closed")

type Store struct {
	dir             string
	eng             store.Engine
	emb             ops.EmbeddingProvider
	tracing         *telemetry.Tracing
	changeRetention time.Duration

	mu       sync.Mutex
	closed   bool
	closeErr error
	closing  chan struct{}
	wg       sync.WaitGroup
}

var (
	ownersMu sync.Mutex
	owners   = map[string]*Store{}
)

func Open(dataDir string, opts ...Option) (*Store, error) {
	cfg := config{changeRetention: store.DefaultChangeRetention, vectorCache: store.DefaultVectorCacheBytes}
	for _, opt := range opts {
		if opt == nil {
			return nil, derr.New(derr.InvalidRequest, "options must not be nil")
		}
		opt(&cfg)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.opener != nil {
		return openWithOpener(cfg)
	}
	if dataDir == "" {
		return nil, derr.New(derr.InvalidRequest, "data directory must not be empty")
	}
	dir, err := canonicalDir(dataDir)
	if err != nil {
		return nil, derr.Wrap(derr.InvalidRequest, fmt.Errorf("cannot resolve data directory %s: %w", dataDir, err))
	}
	ownersMu.Lock()
	if _, dup := owners[dir]; dup {
		ownersMu.Unlock()
		return nil, derr.New(derr.Conflict, "data directory %s is already open in this process; close that store before reopening the directory", dataDir)
	}
	s := &Store{dir: dir, emb: cfg.embedding, tracing: telemetry.New(cfg.tracerProvider, nil, false), changeRetention: cfg.changeRetention, closing: make(chan struct{})}
	owners[dir] = s
	ownersMu.Unlock()

	eng, err := store.Open(dir, store.WithChangeRetention(cfg.changeRetention), store.WithVectorCacheBytes(cfg.vectorCache), store.WithSecretKey(cfg.secrets))
	if err != nil {
		releaseOwnership(dir)
		code := derr.Internal
		if pathShapeError(err) || errors.Is(err, store.ErrCatalogTooNew) {
			code = derr.InvalidRequest
		}
		return nil, derr.Wrap(code, fmt.Errorf("open data directory: %w", err))
	}
	s.eng = eng
	return s, nil
}

func openWithOpener(cfg config) (*Store, error) {
	key := cfg.ownerKey
	ownersMu.Lock()
	if _, dup := owners[key]; dup {
		ownersMu.Unlock()
		return nil, derr.New(derr.Conflict, "this engine is already open in this process; close that store before reopening it")
	}
	s := &Store{dir: key, emb: cfg.embedding, tracing: telemetry.New(cfg.tracerProvider, nil, false), changeRetention: cfg.changeRetention, closing: make(chan struct{})}
	owners[key] = s
	ownersMu.Unlock()

	eng, err := cfg.opener(secret.WithKeyring(context.Background(), cfg.secrets), cfg.changeRetention)
	if err != nil {
		releaseOwnership(key)
		code := derr.Internal
		if errors.Is(err, store.ErrInvalid) || errors.Is(err, store.ErrCatalogTooNew) {
			code = derr.InvalidRequest
		}
		return nil, derr.Wrap(code, fmt.Errorf("open engine: %w", err))
	}
	s.eng = eng
	return s, nil
}

func releaseOwnership(dir string) {
	ownersMu.Lock()
	delete(owners, dir)
	ownersMu.Unlock()
}

func pathShapeError(err error) bool {
	var pe *fs.PathError
	return errors.As(err, &pe) && errors.Is(pe.Err, syscall.ENOTDIR)
}

func canonicalDir(dir string) (string, error) {
	base := string(filepath.Separator)
	if !filepath.IsAbs(dir) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		resolved, err := filepath.EvalSymlinks(cwd)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(filepath.ToSlash(dir), "/") {
			if vol := filepath.VolumeName(resolved); vol != "" {
				return resolveFrom(vol+string(filepath.Separator), dir, 0)
			}
		}
		base = resolved
	}
	return resolveFrom(base, dir, 0)
}

func resolveFrom(base, path string, depth int) (string, error) {
	if depth > 40 {
		return "", fmt.Errorf("too many levels of symbolic links in %s", path)
	}
	if vol := filepath.VolumeName(path); vol != "" {
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("drive-relative path %q is not supported; pass an absolute path", path)
		}
		base = vol + string(filepath.Separator)
		path = path[len(vol):]
	}
	comps := strings.Split(filepath.ToSlash(path), "/")
	resolved := base
	for _, comp := range comps {
		switch comp {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, comp)
		fi, err := os.Lstat(next)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
			resolved = next
			continue
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			resolved = next
			continue
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			if filepath.VolumeName(target) != "" {
				return "", fmt.Errorf("drive-relative symlink target %q is not supported", target)
			}
			if vol := filepath.VolumeName(resolved); vol != "" && strings.HasPrefix(filepath.ToSlash(target), "/") {
				target = vol + target
			} else {
				target = resolved + string(filepath.Separator) + target
			}
		}
		resolved, err = resolveFrom(string(filepath.Separator), target, depth+1)
		if err != nil {
			return "", err
		}
	}
	return resolved, nil
}

func (s *Store) Close() (err error) {
	s.mu.Lock()
	if s.closed {
		ch := s.closing
		s.mu.Unlock()
		<-ch
		s.mu.Lock()
		err = s.closeErr
		s.mu.Unlock()
		return err
	}
	s.closed = true
	s.mu.Unlock()

	s.wg.Wait()
	defer func() {
		releaseOwnership(s.dir)
		s.mu.Lock()
		s.closeErr = err
		close(s.closing)
		s.mu.Unlock()
	}()
	err = s.eng.Close()
	return err
}

func (s *Store) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.wg.Add(1)
	return nil
}

func (s *Store) done() {
	s.wg.Done()
}
