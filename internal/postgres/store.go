package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
)

type Config struct {
	DSN             string
	Catalog         string
	QueryRole       string
	MaxConns        int32
	SharedFilter    bool
	ChangeRetention *time.Duration
	Secrets         *secret.Keyring
}

type Store struct {
	pool            *pgxpool.Pool
	catalog         string
	queryRole       string
	queryGrantMu    sync.Mutex
	queryGrants     map[string][16]byte
	changeRetention time.Duration
	now             func() time.Time
	mu              sync.Mutex
	closed          bool
	done            chan struct{}
	active          sync.WaitGroup
	wake            *wakeSet
	sharedFilter    bool
	secrets         *secret.Keyring
}

type connectionError struct {
	operation string
	cause     error
}

func (e *connectionError) Error() string {
	return "postgres: " + e.operation + "; check the connection settings and database permissions"
}

func (e *connectionError) Unwrap() error { return e.cause }

var catalogName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

const DefaultCatalog = "dolmen_catalog"

func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("%w: PostgreSQL requires an explicit connection string", store.ErrInvalid)
	}
	if cfg.Catalog == "" {
		cfg.Catalog = DefaultCatalog
	}
	if !catalogName.MatchString(cfg.Catalog) || cfg.Catalog == "public" || cfg.Catalog == "information_schema" || len(cfg.Catalog) >= 3 && cfg.Catalog[:3] == "pg_" {
		return nil, fmt.Errorf("%w: invalid PostgreSQL catalog schema", store.ErrInvalid)
	}
	if cfg.QueryRole != "" && !catalogName.MatchString(cfg.QueryRole) {
		return nil, fmt.Errorf("%w: invalid PostgreSQL query role", store.ErrInvalid)
	}
	if cfg.MaxConns < 0 {
		return nil, fmt.Errorf("%w: PostgreSQL MaxConns must not be negative", store.ErrInvalid)
	}
	retention := store.DefaultChangeRetention
	if cfg.ChangeRetention != nil {
		retention = *cfg.ChangeRetention
	}
	if retention < 0 || retention > time.Duration(1<<62-1) {
		return nil, fmt.Errorf("%w: invalid PostgreSQL change retention", store.ErrInvalid)
	}
	pc, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, &connectionError{"invalid connection string", errors.Join(store.ErrInvalid, err)}
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	if pc.ConnConfig.ConnectTimeout == 0 {
		pc.ConnConfig.ConnectTimeout = 10 * time.Second
	}
	pc.ConnConfig.RuntimeParams["application_name"] = "dolmen"
	pc.ConnConfig.RuntimeParams["search_path"] = "pg_catalog"
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, &connectionError{"open pool", err}
	}
	s := &Store{pool: pool, catalog: cfg.Catalog, queryRole: cfg.QueryRole, done: make(chan struct{}), changeRetention: retention, now: time.Now, sharedFilter: cfg.SharedFilter, secrets: cfg.Secrets}
	if err = pool.Ping(ctx); err == nil {
		err = s.bootstrap(ctx)
	}
	if err != nil {
		pool.Close()
		var tooOld *serverVersionError
		if errors.Is(err, store.ErrCatalogTooNew) || errors.As(err, &tooOld) {
			return nil, err
		}
		return nil, &connectionError{"initialize store", err}
	}
	return s, nil
}

func (s *Store) begin(ctx context.Context) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, store.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.active.Add(1)
	return s.active.Done, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-s.done
		return nil
	}
	s.closed = true
	w := s.wake
	s.mu.Unlock()
	if w != nil {
		w.mu.Lock()
		if !w.stopped {
			w.stopped = true
			close(w.stop)
		}
		w.mu.Unlock()
	}
	s.active.Wait()
	s.pool.Close()
	close(s.done)
	return nil
}
