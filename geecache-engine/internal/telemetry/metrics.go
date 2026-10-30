// Package telemetry owns per-process metrics; core packages only use Observer.
package telemetry

import (
	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/prometheus/client_golang/prometheus"
	"time"
)

type Metrics struct {
	sourceRetries                                              *prometheus.CounterVec
	versionEvents, versionRejections                           *prometheus.CounterVec
	Registry                                                   *prometheus.Registry
	api, peer, source                                          *prometheus.CounterVec
	local                                                      *prometheus.CounterVec
	fallback                                                   *prometheus.CounterVec
	writes, invalidations, broadcasts                          *prometheus.CounterVec
	discardedFills                                             *prometheus.CounterVec
	hotHits                                                    prometheus.Counter
	apiDuration, peerDuration, sourceDuration, slotWait        *prometheus.HistogramVec
	writeDuration, broadcastDuration                           *prometheus.HistogramVec
	rejections                                                 *prometheus.CounterVec
	unknownCommits, cleanupChecks, cleanupRemoved, cleanupBusy prometheus.Counter
	cleanupDuration                                            prometheus.Histogram
	writeInflight                                              prometheus.Gauge
	bloom, updates, discoveryErrors                            prometheus.Counter
	inflight, members                                          prometheus.Gauge
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
	m.sourceRetries = counter("simplecache_source_retries_total", "reason")
	m.versionEvents = counter("simplecache_version_invalidations_total", "result")
	m.versionRejections = counter("simplecache_version_rejections_total", "kind")
	m.discardedFills = counter("simplecache_discarded_fills_total", "kind")
	m.hotHits = prometheus.NewCounter(prometheus.CounterOpts{Name: "simplecache_hot_cache_hits_total", Help: "Successful local hot-cache lookups"})
	m.apiDuration = histogram("simplecache_api_duration_seconds")
	m.peerDuration = histogram("simplecache_peer_duration_seconds")
	m.sourceDuration = histogram("simplecache_source_duration_seconds")
	m.slotWait = histogram("simplecache_source_slot_wait_seconds")
	m.writeDuration = histogram("simplecache_source_write_duration_seconds")
	m.broadcastDuration = histogram("simplecache_invalidation_broadcast_duration_seconds")
	m.rejections = counter("simplecache_admission_rejections_total", "kind")
	m.unknownCommits = prometheus.NewCounter(prometheus.CounterOpts{Name: "simplecache_source_unknown_commits_total", Help: "Source write failures with unknown commit outcome"})
	m.cleanupChecks = prometheus.NewCounter(prometheus.CounterOpts{Name: "simplecache_cleanup_checked_total", Help: "Entries examined by bounded active expiration"})
	m.cleanupRemoved = prometheus.NewCounter(prometheus.CounterOpts{Name: "simplecache_cleanup_removed_total", Help: "Expired entries removed by active expiration"})
	m.cleanupBusy = prometheus.NewCounter(prometheus.CounterOpts{Name: "simplecache_cleanup_busy_total", Help: "Maintenance areas skipped because their cache lock was busy"})
	m.cleanupDuration = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "simplecache_cleanup_duration_seconds", Help: "Wall time per cache area cleanup attempt", Buckets: []float64{0.00001, 0.00005, 0.0001, 0.0005, 0.001, 0.002, 0.005, 0.01, 0.05, 0.1, 1}})
	m.writeInflight = prometheus.NewGauge(prometheus.GaugeOpts{Name: "simplecache_write_requests_inflight", Help: "Admitted write/invalidation requests including body, source and fan-out work"})
	m.bloom = prometheus.NewCounter(prometheus.CounterOpts{Name: "simplecache_bloom_rejections_total", Help: "Bloom definite negatives"})
	m.updates = prometheus.NewCounter(prometheus.CounterOpts{Name: "simplecache_membership_updates_total", Help: "Changed membership snapshots"})
	m.discoveryErrors = prometheus.NewCounter(prometheus.CounterOpts{Name: "simplecache_discovery_errors_total", Help: "Discovery list/watch failures"})
	m.inflight = prometheus.NewGauge(prometheus.GaugeOpts{Name: "simplecache_source_inflight", Help: "Executing source getters"})
	m.members = prometheus.NewGauge(prometheus.GaugeOpts{Name: "simplecache_members", Help: "Published member count"})
	m.Registry.MustRegister(m.api, m.peer, m.source, m.local, m.fallback, m.apiDuration, m.peerDuration, m.sourceDuration, m.slotWait, m.bloom, m.updates, m.discoveryErrors, m.inflight, m.members, prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	m.Registry.MustRegister(m.writes, m.invalidations, m.broadcasts)
	m.Registry.MustRegister(m.sourceRetries, m.discardedFills, m.versionEvents, m.versionRejections)
	m.Registry.MustRegister(m.hotHits)
	m.Registry.MustRegister(m.writeDuration, m.broadcastDuration, m.rejections, m.unknownCommits, m.cleanupChecks, m.cleanupRemoved, m.cleanupBusy, m.cleanupDuration, m.writeInflight)
	return m
}
func (m *Metrics) Observe(operation, result string, elapsed time.Duration) {
	switch operation {
	case "source_retry":
		m.sourceRetries.WithLabelValues(result).Inc()
	case "version_event":
		m.versionEvents.WithLabelValues(result).Inc()
	case "version_rejected":
		m.versionRejections.WithLabelValues(result).Inc()
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
		m.writeDuration.WithLabelValues(result).Observe(elapsed.Seconds())
		if result == "error" || result == "timeout" {
			m.unknownCommits.Inc()
		}
	case "invalidate":
		m.invalidations.WithLabelValues(result).Inc()
	case "broadcast":
		m.broadcasts.WithLabelValues(result).Inc()
		m.broadcastDuration.WithLabelValues(result).Observe(elapsed.Seconds())
	case "rejected":
		m.rejections.WithLabelValues(result).Inc()
	case "discard_source":
		m.discardedFills.WithLabelValues("source").Inc()
	case "discard_hot":
		m.discardedFills.WithLabelValues("hot").Inc()
	case "hot_hit":
		m.hotHits.Inc()
	}
}
func (m *Metrics) Inflight(delta float64)      { m.inflight.Add(delta) }
func (m *Metrics) WriteInflight(delta float64) { m.writeInflight.Add(delta) }

func (m *Metrics) Cleanup(checked, removed int, elapsed time.Duration, busy bool) {
	m.cleanupChecks.Add(float64(checked))
	m.cleanupRemoved.Add(float64(removed))
	m.cleanupDuration.Observe(elapsed.Seconds())
	if busy {
		m.cleanupBusy.Inc()
	}
}

// ObserveCache registers logical storage observations for the process's group.
func (m *Metrics) ObserveCache(group *cache.Group) {
	m.Registry.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "simplecache_version_records", Help: "Retained per-key source-version floors"}, func() float64 { return float64(group.Stats().VersionRecords) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "simplecache_version_record_capacity", Help: "Maximum retained source-version floors per group"}, func() float64 { return float64(group.Stats().VersionCapacity) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "simplecache_version_record_evictions_total", Help: "Forgotten floors; retiring a floor also fences old in-flight fills"}, func() float64 { return float64(group.Stats().VersionEvictions) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "simplecache_cache_bytes", Help: "Logical key/value bytes, not process RSS"}, func() float64 { return float64(group.Stats().Bytes) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "simplecache_cache_capacity_bytes", Help: "Configured logical cache capacity"}, func() float64 { return float64(group.Stats().Capacity) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "simplecache_cache_entries", Help: "Stored entries including lazily expired entries"}, func() float64 { return float64(group.Stats().Entries) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "simplecache_cache_removals_total", Help: "Cache entries removed by eviction, expiry or invalidation"}, func() float64 { return float64(group.Stats().Removals) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "simplecache_hot_cache_bytes", Help: "Hot bytes included in total cache bytes"}, func() float64 { return float64(group.Stats().HotBytes) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "simplecache_hot_cache_capacity_bytes", Help: "Hot share of total logical capacity"}, func() float64 { return float64(group.Stats().HotCapacity) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "simplecache_api_miss_waiters", Help: "API callers waiting on cache-miss work"}, func() float64 { return float64(group.AdmissionStats().APIWaiters) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "simplecache_peer_miss_waiters", Help: "Peer callers waiting on local miss work"}, func() float64 { return float64(group.AdmissionStats().PeerWaiters) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "simplecache_route_tasks_inflight", Help: "Admitted shared route tasks, including tasks outliving callers"}, func() float64 { return float64(group.AdmissionStats().RouteTasks) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "simplecache_source_pending", Help: "Source tasks queued for execution slots"}, func() float64 { return float64(group.AdmissionStats().SourcePending) }),
	)
}
func (m *Metrics) Membership(count int, changed bool) {
	m.members.Set(float64(count))
	if changed {
		m.updates.Inc()
	}
}
func (m *Metrics) DiscoveryError() { m.discoveryErrors.Inc() }
