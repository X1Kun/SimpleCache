package telemetry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

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
	if _, err := g.GetDirect(context.Background(), "target"); !errors.Is(err, cache.ErrStaleVersion) {
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
		if !strings.HasPrefix(family.GetName(), "simplecache_version_") {
			continue
		}
		for _, metric := range family.Metric {
			name := family.GetName()
			for _, label := range metric.Label {
				if label.GetName() != "kind" && label.GetName() != "result" {
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
		"simplecache_version_rejections_total/source":     1,
		"simplecache_version_invalidations_total/ignored": 1,
		"simplecache_version_invalidations_total/applied": cache.DefaultVersionRecords + 1,
	} {
		if observed[name] != want {
			t.Errorf("%s=%v want %v", name, observed[name], want)
		}
	}
}
