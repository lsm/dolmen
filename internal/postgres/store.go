package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lsm/dolmen/internal/store"
)

type Config struct {
	DSN      string
	Catalog  string
	MaxConns int32
}

type Store struct {
	pool    *pgxpool.Pool
	catalog string
	mu      sync.Mutex
	closed  bool
	done    chan struct{}
	active  sync.WaitGroup
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

func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("%w: PostgreSQL requires an explicit connection string", store.ErrInvalid)
	}
	if cfg.Catalog == "" {
		cfg.Catalog = "dolmen_catalog"
	}
	if !catalogName.MatchString(cfg.Catalog) || cfg.Catalog == "public" || cfg.Catalog == "information_schema" || len(cfg.Catalog) >= 3 && cfg.Catalog[:3] == "pg_" {
		return nil, fmt.Errorf("%w: invalid PostgreSQL catalog schema", store.ErrInvalid)
	}
	if cfg.MaxConns < 0 {
		return nil, fmt.Errorf("%w: PostgreSQL MaxConns must not be negative", store.ErrInvalid)
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
	s := &Store{pool: pool, catalog: cfg.Catalog, done: make(chan struct{})}
	if err = pool.Ping(ctx); err == nil {
		err = s.bootstrap(ctx)
	}
	if err != nil {
		pool.Close()
		if errors.Is(err, store.ErrCatalogTooNew) {
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
	s.mu.Unlock()
	s.active.Wait()
	s.pool.Close()
	close(s.done)
	return nil
}
