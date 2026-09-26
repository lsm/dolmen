package store

import (
	"context"

	"go.opentelemetry.io/otel/metric"

	"github.com/lsm/dolmen/internal/telemetry/dbstat"
)

func WithMeterProvider(mp metric.MeterProvider) OpenOption {
	return func(s *Store) { s.mp = mp }
}

func (s *Store) TelemetryGauges() dbstat.Snapshot {
	var snap dbstat.Snapshot
	s.mu.Lock()
	snap.Values[dbstat.OpenNamespaces] = int64(len(s.nss))
	s.mu.Unlock()
	s.vcache.mu.Lock()
	snap.Values[dbstat.VectorCacheUsed] = s.vcache.used
	snap.Values[dbstat.VectorCacheMax] = s.vcache.max
	s.vcache.mu.Unlock()
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
