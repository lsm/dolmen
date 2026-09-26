package dbstat

import (
	"context"

	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/lsm/dolmen/internal/telemetry/dbspan"
)

const instrumentationName = "github.com/lsm/dolmen"

const (
	NamespacesOpen   = "dolmen.namespaces.open"
	VectorCacheUsage = "dolmen.vector_cache.usage"
	VectorCacheLimit = "dolmen.vector_cache.limit"
	PoolConnections  = "db.client.connection.count"
	PoolMax          = "db.client.connection.max"
)

type Gauge int

const (
	OpenNamespaces Gauge = iota
	VectorCacheUsed
	VectorCacheMax
	PoolIdle
	PoolUsed
	PoolLimit
	gaugeCount
)

type Snapshot struct {
	Pool   string
	Values [gaugeCount]int64
}

func (s Snapshot) Value(g Gauge) int64 { return s.Values[g] }

type Engine interface {
	TelemetryGauges() Snapshot
}

type instruments struct {
	namespacesOpen   metric.Int64ObservableUpDownCounter
	vectorCacheUsage metric.Int64ObservableUpDownCounter
	vectorCacheLimit metric.Int64ObservableUpDownCounter
	poolConnections  metric.Int64ObservableUpDownCounter
	poolMax          metric.Int64ObservableUpDownCounter
}

func newInstruments(m metric.Meter) (*instruments, error) {
	var in instruments
	var err error
	if in.namespacesOpen, err = m.Int64ObservableUpDownCounter(NamespacesOpen,
		metric.WithUnit("{namespace}"),
		metric.WithDescription("Namespace files this process holds open.")); err != nil {
		return nil, err
	}
	if in.vectorCacheUsage, err = m.Int64ObservableUpDownCounter(VectorCacheUsage,
		metric.WithUnit("By"),
		metric.WithDescription("Bytes of decoded vectors held in the vector cache.")); err != nil {
		return nil, err
	}
	if in.vectorCacheLimit, err = m.Int64ObservableUpDownCounter(VectorCacheLimit,
		metric.WithUnit("By"),
		metric.WithDescription("Bytes the vector cache may hold before it evicts.")); err != nil {
		return nil, err
	}
	if in.poolConnections, err = m.Int64ObservableUpDownCounter(PoolConnections,
		metric.WithUnit("{connection}"),
		metric.WithDescription("Database connections currently idle or in use.")); err != nil {
		return nil, err
	}
	if in.poolMax, err = m.Int64ObservableUpDownCounter(PoolMax,
		metric.WithUnit("{connection}"),
		metric.WithDescription("Connections the pool may open.")); err != nil {
		return nil, err
	}
	return &in, nil
}

func Observe(mp metric.MeterProvider, engine any) (func(context.Context) error, error) {
	if mp == nil {
		return nil, nil
	}
	if _, off := mp.(metricnoop.MeterProvider); off {
		return nil, nil
	}
	source, ok := engine.(Engine)
	if !ok {
		return nil, nil
	}
	m := mp.Meter(instrumentationName)
	in, err := newInstruments(m)
	if err != nil {
		return nil, err
	}
	reg, err := m.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		snap := source.TelemetryGauges()
		o.ObserveInt64(in.namespacesOpen, snap.Value(OpenNamespaces))
		o.ObserveInt64(in.vectorCacheUsage, snap.Value(VectorCacheUsed))
		o.ObserveInt64(in.vectorCacheLimit, snap.Value(VectorCacheMax))
		if snap.Pool == "" {
			return nil
		}
		pool := semconv.DBClientConnectionPoolName(dbspan.Clean(snap.Pool, dbspan.MaxNameAttr))
		o.ObserveInt64(in.poolConnections, snap.Value(PoolIdle),
			metric.WithAttributes(pool, semconv.DBClientConnectionStateIdle))
		o.ObserveInt64(in.poolConnections, snap.Value(PoolUsed),
			metric.WithAttributes(pool, semconv.DBClientConnectionStateUsed))
		o.ObserveInt64(in.poolMax, snap.Value(PoolLimit), metric.WithAttributes(pool))
		return nil
	}, in.namespacesOpen, in.vectorCacheUsage, in.vectorCacheLimit, in.poolConnections, in.poolMax)
	if err != nil {
		return nil, err
	}
	return func(context.Context) error { return reg.Unregister() }, nil
}

type meterKey struct{}

func ContextWithMeter(ctx context.Context, mp metric.MeterProvider) context.Context {
	if mp == nil {
		return ctx
	}
	return context.WithValue(ctx, meterKey{}, mp)
}

func MeterFrom(ctx context.Context) metric.MeterProvider {
	mp, _ := ctx.Value(meterKey{}).(metric.MeterProvider)
	return mp
}
