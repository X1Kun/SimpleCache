package telemetry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
)

func TestLogicalCacheMetrics(t *testing.T) {
	m := New()
	g := cache.NewGroup("test", 8, cache.GetterFunc(func(context.Context, string) ([]byte, error) { return []byte("value"), nil }), cache.Options{})
	m.ObserveCache(g)
	for _, key := range []string{"a", "b"} {
		if _, err := g.Get(context.Background(), key); err != nil {
			t.Fatal(err)
		}
	}
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{}
	for _, family := range families {
		for _, metric := range family.Metric {
			if metric.Gauge != nil {
				values[family.GetName()] = metric.Gauge.GetValue()
			}
			if metric.Counter != nil {
				values[family.GetName()] = metric.Counter.GetValue()
			}
		}
	}
	for name, want := range map[string]float64{"simplecache_cache_bytes": 6, "simplecache_cache_capacity_bytes": 8, "simplecache_cache_entries": 1, "simplecache_cache_removals_total": 1} {
		if values[name] != want {
			t.Errorf("%s=%v want %v", name, values[name], want)
		}
	}
}

func TestVersionGuardMetricsExposeBoundedRecordsAndRejectedLoads(t *testing.T) {
	m := New()
	g := cache.NewGroup("versions", 1024, cache.GetterFunc(func(context.Context, string) ([]byte, error) { return []byte("unversioned"), nil }), cache.Options{Observer: m})
	m.ObserveCache(g)
	if err := g.InvalidateVersion("target", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Get(context.Background(), "target"); !errors.Is(err, cache.ErrStaleVersion) {
		t.Fatal(err)
	}
	_ = g.InvalidateVersion("target", 2)
	for i := 0; i < cache.DefaultVersionRecords; i++ {
		_ = g.InvalidateVersion(fmt.Sprint(i), 1)
	}
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	observed := map[string]float64{}
	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), "simplecache_version_") && family.GetName() != "simplecache_source_retries_total" {
			continue
		}
		for _, metric := range family.Metric {
			name := family.GetName()
			for _, label := range metric.Label {
				if label.GetName() != "kind" && label.GetName() != "result" && label.GetName() != "reason" {
					t.Fatal("unbounded version metric label", label)
				}
				name += "/" + label.GetValue()
			}
			if metric.Gauge != nil {
				observed[name] = metric.Gauge.GetValue()
			}
			if metric.Counter != nil {
				observed[name] = metric.Counter.GetValue()
			}
		}
	}
	for name, want := range map[string]float64{
		"simplecache_version_records":                     cache.DefaultVersionRecords,
		"simplecache_version_record_capacity":             cache.DefaultVersionRecords,
		"simplecache_version_record_evictions_total":      1,
		"simplecache_version_rejections_total/source":     2,
		"simplecache_source_retries_total/stale_version":  1,
		"simplecache_version_invalidations_total/ignored": 1,
		"simplecache_version_invalidations_total/applied": cache.DefaultVersionRecords + 1,
	} {
		if observed[name] != want {
			t.Errorf("%s=%v want %v", name, observed[name], want)
		}
	}
}

func TestOutboxMetricsSeparateDeliveryAndSampledBacklog(t *testing.T) {
	m := New()
	m.OutboxEvent("finish", "delivered")
	m.OutboxEvent("finish", "retry")
	m.OutboxEvent("finish", "lease_lost")
	m.OutboxBacklog(3, 2*time.Second)
	m.OutboxStorage(7, 4096)
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	counters := 0
	for _, family := range families {
		switch family.GetName() {
		case "simplecache_outbox_operations_total":
			for _, metric := range family.Metric {
				counters++
				if metric.Counter.GetValue() != 1 {
					t.Fatal(metric)
				}
				for _, label := range metric.Label {
					if label.GetName() != "operation" && label.GetName() != "result" {
						t.Fatal("unbounded outbox label", label)
					}
				}
			}
		case "simplecache_outbox_purge_eligible_events":
			if family.Metric[0].Gauge.GetValue() != 7 {
				t.Fatal(family)
			}
		case "simplecache_outbox_table_bytes":
			if family.Metric[0].Gauge.GetValue() != 4096 {
				t.Fatal(family)
			}
		case "simplecache_outbox_pending_events":
			if family.Metric[0].Gauge.GetValue() != 3 {
				t.Fatal(family)
			}
		case "simplecache_outbox_oldest_pending_seconds":
			if family.Metric[0].Gauge.GetValue() != 2 {
				t.Fatal(family)
			}
		}
	}
	if counters != 3 {
		t.Fatal("missing outbox outcome series", counters)
	}
}
