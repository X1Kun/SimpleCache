// Package telemetry owns per-process metrics; core packages only use Observer.
package telemetry

import (
	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/prometheus/client_golang/prometheus"
	"time"
)

type Metrics struct {
	Registry                                            *prometheus.Registry
	api, peer, source                                   *prometheus.CounterVec
	local                                               *prometheus.CounterVec
	fallback                                            *prometheus.CounterVec
	writes, invalidations, broadcasts                   *prometheus.CounterVec
	apiDuration, peerDuration, sourceDuration, slotWait *prometheus.HistogramVec
	bloom, updates, discoveryErrors                     prometheus.Counter
	inflight, members                                   prometheus.Gauge
}

func New() *Metrics {
	m := &Metrics{Registry: prometheus.NewRegistry()}
	counter := func(name string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: name}, labels)
	}
	histogram := func(name string) *prometheus.HistogramVec {
		return prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: name, Buckets: prometheus.DefBuckets}, []string{"result"})
	}
	m.api = counter("simplecache_api_requests_total", "result")
	m.peer = counter("simplecache_peer_requests_total", "result")
	m.source = counter("simplecache_source_loads_total", "result")
	m.local = counter("simplecache_local_lookups_total", "kind", "result")
	m.fallback = counter("simplecache_fallback_total", "reason")
	m.writes = counter("simplecache_source_writes_total", "result")
	m.invalidations = counter("simplecache_invalidations_total", "result")
	m.broadcasts = counter("simplecache_invalidation_broadcasts_total", "result")
	m.apiDuration = histogram("simplecache_api_duration_seconds")
	m.peerDuration = histogram("simplecache_peer_duration_seconds")
	m.sourceDuration = histogram("simplecache_source_duration_seconds")
	m.slotWait = histogram("simplecache_source_slot_wait_seconds")
	m.bloom = prometheus.NewCounter(prometheus.CounterOpts{Name: "simplecache_bloom_rejections_total", Help: "Bloom definite negatives"})
	m.updates = prometheus.NewCounter(prometheus.CounterOpts{Name: "simplecache_membership_updates_total", Help: "Changed membership snapshots"})
	m.discoveryErrors = prometheus.NewCounter(prometheus.CounterOpts{Name: "simplecache_discovery_errors_total", Help: "Discovery list/watch failures"})
	m.inflight = prometheus.NewGauge(prometheus.GaugeOpts{Name: "simplecache_source_inflight", Help: "Executing source getters"})
	m.members = prometheus.NewGauge(prometheus.GaugeOpts{Name: "simplecache_members", Help: "Published member count"})
	m.Registry.MustRegister(m.api, m.peer, m.source, m.local, m.fallback, m.apiDuration, m.peerDuration, m.sourceDuration, m.slotWait, m.bloom, m.updates, m.discoveryErrors, m.inflight, m.members, prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	m.Registry.MustRegister(m.writes, m.invalidations, m.broadcasts)
	return m
}
func (m *Metrics) Observe(operation, result string, elapsed time.Duration) {
	switch operation {
	case "api":
		m.api.WithLabelValues(result).Inc()
		m.apiDuration.WithLabelValues(result).Observe(elapsed.Seconds())
	case "peer":
		m.peer.WithLabelValues(result).Inc()
		m.peerDuration.WithLabelValues(result).Observe(elapsed.Seconds())
	case "source":
		m.source.WithLabelValues(result).Inc()
		m.sourceDuration.WithLabelValues(result).Observe(elapsed.Seconds())
	case "lookup_api":
		m.local.WithLabelValues("api", result).Inc()
	case "lookup_peer":
		m.local.WithLabelValues("peer", result).Inc()
	case "fallback":
		m.fallback.WithLabelValues(result).Inc()
	case "slot":
		m.slotWait.WithLabelValues(result).Observe(elapsed.Seconds())
	case "bloom":
		m.bloom.Inc()
	case "write":
		m.writes.WithLabelValues(result).Inc()
	case "invalidate":
		m.invalidations.WithLabelValues(result).Inc()
	case "broadcast":
		m.broadcasts.WithLabelValues(result).Inc()
	}
}
func (m *Metrics) Inflight(delta float64) { m.inflight.Add(delta) }

// ObserveCache registers logical storage observations for the process's group.
func (m *Metrics) ObserveCache(group *cache.Group) {
	m.Registry.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "simplecache_cache_bytes", Help: "Logical key/value bytes, not process RSS"}, func() float64 { return float64(group.Stats().Bytes) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "simplecache_cache_capacity_bytes", Help: "Configured logical cache capacity"}, func() float64 { return float64(group.Stats().Capacity) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "simplecache_cache_entries", Help: "Stored entries including lazily expired entries"}, func() float64 { return float64(group.Stats().Entries) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "simplecache_cache_removals_total", Help: "Cache entries removed by eviction, expiry or invalidation"}, func() float64 { return float64(group.Stats().Removals) }),
	)
}
func (m *Metrics) Membership(count int, changed bool) {
	m.members.Set(float64(count))
	if changed {
		m.updates.Inc()
	}
}
func (m *Metrics) DiscoveryError() { m.discoveryErrors.Inc() }
