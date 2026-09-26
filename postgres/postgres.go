package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lsm/dolmen"
	"github.com/lsm/dolmen/internal/postgres"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/telemetry/dbspan"
)

type Config struct {
	DSN       string
	Catalog   string
	QueryRole string
	MaxConns  int32
}

func ownerKey(cfg Config, catalog string) string {
	target := cfg.DSN
	if parsed, err := pgxpool.ParseConfig(cfg.DSN); err == nil {
		conn := parsed.ConnConfig
		target = fmt.Sprintf("%s\x00%d\x00%s\x00%s", conn.Host, conn.Port, conn.Database, conn.User)
	}
	return "postgres\x00" + target + "\x00" + catalog
}

func With(cfg Config) dolmen.Option {
	catalog := cfg.Catalog
	if catalog == "" {
		catalog = postgres.DefaultCatalog
	}
	key := ownerKey(cfg, catalog)
	return dolmen.WithEngineOpener(store.EnginePostgres, key, func(ctx context.Context, retention time.Duration) (store.Engine, error) {
		return postgres.Open(ctx, postgres.Config{
			DSN:             cfg.DSN,
			Catalog:         cfg.Catalog,
			QueryRole:       cfg.QueryRole,
			MaxConns:        cfg.MaxConns,
			ChangeRetention: &retention,
			Secrets:         secret.KeyringFrom(ctx),
			TracerProvider:  dbspan.ProviderFrom(ctx),
		})
	})
}
