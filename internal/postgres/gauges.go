package postgres

import (
	"context"

	"go.opentelemetry.io/otel/metric"

	"github.com/lsm/dolmen/internal/telemetry/dbstat"
)

func (s *Store) TelemetryGauges() dbstat.Snapshot {
	var snap dbstat.Snapshot
	stat := s.pool.Stat()
	snap.Pool = s.catalog
	snap.Values[dbstat.PoolIdle] = int64(stat.IdleConns())
	snap.Values[dbstat.PoolUsed] = int64(stat.AcquiredConns())
	snap.Values[dbstat.PoolLimit] = int64(stat.MaxConns())
	return snap
}

func (s *Store) startEngineGauges(mp metric.MeterProvider) error {
	stop, err := dbstat.Observe(mp, s)
	if err != nil {
		return err
	}
	s.stopGauges = stop
	return nil
}

func (s *Store) stopEngineGauges() {
	s.gaugesOnce.Do(func() {
		if s.stopGauges == nil {
			return
		}
		_ = s.stopGauges(context.Background())
	})
}

var _ dbstat.Engine = (*Store)(nil)
