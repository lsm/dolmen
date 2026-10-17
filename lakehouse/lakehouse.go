package lakehouse

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lsm/dolmen"
	"github.com/lsm/dolmen/internal/lakehouse"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/telemetry/dbspan"
	"github.com/lsm/dolmen/internal/telemetry/dbstat"
)

type Config struct {
	Sidecar           string
	Extensions        string
	MaxOpenNamespaces int
}

func With(cfg Config) dolmen.Option {
	return dolmen.WithEngineOpener(store.EngineLakehouse, store.EngineLakehouse, func(ctx context.Context, retention time.Duration) (store.Engine, error) {
		if cfg.MaxOpenNamespaces < 0 {
			return nil, fmt.Errorf("%w: lakehouse.Config.MaxOpenNamespaces is %d; give at least 1, or 0 for the default of %d", store.ErrInvalid, cfg.MaxOpenNamespaces, lakehouse.DefaultMaxOpenNamespaces)
		}
		dir := store.OpenDirFrom(ctx)
		if dir == "" {
			return nil, errors.New("the lakehouse engine opens a data directory; pass one to dolmen.Open")
		}
		opts := []lakehouse.OpenOption{
			lakehouse.WithSecretKeyring(secret.KeyringFrom(ctx)),
			lakehouse.WithChangeRetention(retention),
			lakehouse.WithSQLEngine(lakehouse.SQLEngine{Binary: cfg.Sidecar, ExtensionDir: cfg.Extensions}),
			lakehouse.WithTracerProvider(dbspan.ProviderFrom(ctx)),
			lakehouse.WithMeterProvider(dbstat.MeterFrom(ctx)),
		}
		if cfg.MaxOpenNamespaces > 0 {
			opts = append(opts, lakehouse.WithMaxOpenNamespaces(cfg.MaxOpenNamespaces))
		}
		return lakehouse.Open(dir, opts...)
	})
}
