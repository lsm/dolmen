package postgres

import (
	"context"
	"time"

	"github.com/lsm/dolmen"
	"github.com/lsm/dolmen/internal/postgres"
	"github.com/lsm/dolmen/internal/store"
)

type Config struct {
	DSN       string
	Catalog   string
	QueryRole string
	MaxConns  int32
}

func With(cfg Config) dolmen.Option {
	catalog := cfg.Catalog
	if catalog == "" {
		catalog = postgres.DefaultCatalog
	}
	key := "postgres\x00" + cfg.DSN + "\x00" + catalog
	return dolmen.WithEngineOpener(store.EnginePostgres, key, func(ctx context.Context, retention time.Duration) (store.Engine, error) {
		return postgres.Open(ctx, postgres.Config{
			DSN:             cfg.DSN,
			Catalog:         cfg.Catalog,
			QueryRole:       cfg.QueryRole,
			MaxConns:        cfg.MaxConns,
			ChangeRetention: &retention,
		})
	})
}
