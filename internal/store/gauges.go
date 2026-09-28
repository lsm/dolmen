package store

import (
	"context"
	"os"

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
	paths := make([]string, 0, len(s.nss))
	for name := range s.nss {
		paths = append(paths, s.nsPath(name))
	}
	s.mu.Unlock()
	s.vcache.mu.Lock()
	snap.Values[dbstat.VectorCacheUsed] = s.vcache.used
	snap.Values[dbstat.VectorCacheMax] = s.vcache.max
	s.vcache.mu.Unlock()
	var wal, largest int64
	for _, path := range paths {
		snap.Values[dbstat.DBBytes] += fileBytes(path)
		if size := fileBytes(path + "-wal"); size > 0 {
			wal += size
			if size > largest {
				largest = size
			}
		}
	}
	snap.Values[dbstat.WALBytes] = wal
	snap.Values[dbstat.WALLargestBytes] = largest
	return snap
}

func fileBytes(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
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
