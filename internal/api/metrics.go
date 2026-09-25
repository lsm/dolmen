package api

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lsm/dolmen/internal/version"
)

var latencyBuckets = []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 30, 120}

type opKey struct {
	op, outcome string
}

type opLatency struct {
	counts []uint64
	sum    float64
	n      uint64
}

type metrics struct {
	mu            sync.Mutex
	ops           map[opKey]uint64
	latency       map[string]*opLatency
	inFlight      atomic.Int64
	subscriptions atomic.Int64
	started       time.Time
}

func newMetrics() *metrics {
	return &metrics{ops: map[opKey]uint64{}, latency: map[string]*opLatency{}, started: time.Now()}
}

func (s *Server) metricOp(op string) string {
	if _, ok := s.Op(op); ok {
		return op
	}
	return "unknown"
}

func (m *metrics) observe(op string, err error, d time.Duration) {
	outcome := "ok"
	if err != nil {
		outcome = string(WrapError(err).Code)
	}
	secs := d.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ops[opKey{op, outcome}]++
	l := m.latency[op]
	if l == nil {
		l = &opLatency{counts: make([]uint64, len(latencyBuckets))}
		m.latency[op] = l
	}
	for i, b := range latencyBuckets {
		if secs <= b {
			l.counts[i]++
		}
	}
	l.sum += secs
	l.n++
}

func (m *metrics) write(w io.Writer) {
	m.mu.Lock()
	keys := make([]opKey, 0, len(m.ops))
	for k := range m.ops {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].op != keys[j].op {
			return keys[i].op < keys[j].op
		}
		return keys[i].outcome < keys[j].outcome
	})
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP dolmen_build_info The running dolmen version.\n# TYPE dolmen_build_info gauge\ndolmen_build_info{version=%q} 1\n", version.Version)
	fmt.Fprintf(&b, "# HELP dolmen_uptime_seconds Seconds since the server started.\n# TYPE dolmen_uptime_seconds gauge\ndolmen_uptime_seconds %g\n", time.Since(m.started).Seconds())
	b.WriteString("# HELP dolmen_operations_total Operations finished, by operation and outcome (ok or an error code).\n# TYPE dolmen_operations_total counter\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "dolmen_operations_total{op=%q,outcome=%q} %d\n", k.op, k.outcome, m.ops[k])
	}
	ops := make([]string, 0, len(m.latency))
	for op := range m.latency {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	b.WriteString("# HELP dolmen_operation_duration_seconds Operation latency, by operation.\n# TYPE dolmen_operation_duration_seconds histogram\n")
	for _, op := range ops {
		l := m.latency[op]
		for i, bound := range latencyBuckets {
			fmt.Fprintf(&b, "dolmen_operation_duration_seconds_bucket{op=%q,le=\"%g\"} %d\n", op, bound, l.counts[i])
		}
		fmt.Fprintf(&b, "dolmen_operation_duration_seconds_bucket{op=%q,le=\"+Inf\"} %d\n", op, l.n)
		fmt.Fprintf(&b, "dolmen_operation_duration_seconds_sum{op=%q} %g\n", op, l.sum)
		fmt.Fprintf(&b, "dolmen_operation_duration_seconds_count{op=%q} %d\n", op, l.n)
	}
	m.mu.Unlock()
	fmt.Fprintf(&b, "# HELP dolmen_operations_in_flight Operations running now.\n# TYPE dolmen_operations_in_flight gauge\ndolmen_operations_in_flight %d\n", m.inFlight.Load())
	fmt.Fprintf(&b, "# HELP dolmen_subscriptions_active Open /v1/subscribe streams.\n# TYPE dolmen_subscriptions_active gauge\ndolmen_subscriptions_active %d\n", m.subscriptions.Load())
	io.WriteString(w, b.String())
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.write(w)
}
