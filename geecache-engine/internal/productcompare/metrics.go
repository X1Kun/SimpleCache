package productcompare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Snapshot struct {
	Node   string             `json:"node"`
	PID    int                `json:"pid"`
	Values map[string]float64 `json:"values"`
}

// Counter vectors that have not yet been used are absent from Prometheus text.
// Their zeroes are valid only after required process/cache gauges are verified.
func parseMetrics(body string) (map[string]float64, error) {
	out := map[string]float64{"source": 0, "peer": 0, "fallback": 0, "writes": 0, "invalidations": 0, "removals": 0, "api_hit": 0, "api_miss": 0, "peer_hit": 0, "peer_miss": 0, "discarded_source": 0, "discarded_hot": 0, "hot_hit": 0}
	seen := map[string]bool{}
	gauges := map[string]string{"process_cpu_seconds_total": "cpu", "process_resident_memory_bytes": "rss", "go_memstats_heap_alloc_bytes": "heap", "go_memstats_alloc_bytes_total": "alloc_bytes", "simplecache_cache_bytes": "cache_bytes", "simplecache_cache_capacity_bytes": "capacity", "simplecache_cache_entries": "entries", "simplecache_cache_removals_total": "removals"}
	optionalGauges := map[string]string{"simplecache_hot_cache_bytes": "hot_bytes", "simplecache_hot_cache_capacity_bytes": "hot_capacity"}
	counters := map[string]string{"simplecache_source_loads_total": "source", "simplecache_peer_requests_total": "peer", "simplecache_fallback_total": "fallback", "simplecache_source_writes_total": "writes", "simplecache_invalidations_total": "invalidations"}
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		name, labels, _ := strings.Cut(fields[0], "{")
		key := gauges[name]
		if key == "" {
			key = optionalGauges[name]
		}
		if key == "" {
			key = counters[name]
		}
		if name == "simplecache_local_lookups_total" {
			kind := "api"
			if strings.Contains(labels, `kind="peer"`) {
				kind = "peer"
			}
			if strings.Contains(labels, `result="hit"`) {
				key = kind + "_hit"
			} else if strings.Contains(labels, `result="miss"`) {
				key = kind + "_miss"
			}
		}
		if name == "simplecache_discarded_fills_total" {
			if strings.Contains(labels, `kind="source"`) {
				key = "discarded_source"
			} else if strings.Contains(labels, `kind="hot"`) {
				key = "discarded_hot"
			}
		}
		if name == "simplecache_hot_cache_hits_total" {
			key = "hot_hit"
		}
		if key == "" {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, errors.New("invalid process/cache metrics")
		}
		out[key] += value
		seen[name] = true
	}
	for name := range gauges {
		if !seen[name] {
			return nil, fmt.Errorf("required metric missing: %s", name)
		}
	}
	return out, nil
}

func collect(ctx context.Context, client *http.Client, nodes []*node, dir, label string) ([]Snapshot, error) {
	var snapshots []Snapshot
	for _, n := range nodes {
		if !n.running {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, "GET", n.api+"/metrics", nil)
		if err != nil {
			return nil, err
		}
		response, err := client.Do(req)
		if err != nil {
			return nil, errors.New("could not collect fixture metrics")
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode != 200 {
			return nil, errors.New("fixture metrics response failed")
		}
		if err := os.WriteFile(filepath.Join(dir, label+"-"+n.name+".prom"), body, 0644); err != nil {
			return nil, err
		}
		values, err := parseMetrics(string(body))
		if err != nil {
			return nil, err
		}
		if values["capacity"] != float64(n.capacity) {
			return nil, errors.New("fixture cache capacity does not match the experiment budget")
		}
		snapshots = append(snapshots, Snapshot{Node: n.name, PID: n.pid, Values: values})
	}
	return snapshots, nil
}

func metricDelta(before, after []Snapshot) (map[string]float64, error) {
	if len(before) != len(after) {
		return nil, errors.New("process set changed inside measurement stage")
	}
	out := map[string]float64{}
	for i, b := range before {
		a := after[i]
		if b.Node != a.Node || b.PID != a.PID {
			return nil, errors.New("PID changed inside measurement stage; counters cannot be subtracted")
		}
		if a.Values["cache_bytes"] > a.Values["capacity"] {
			return nil, errors.New("logical cache capacity exceeded")
		}
		for _, key := range []string{"source", "peer", "fallback", "writes", "invalidations", "removals", "cpu", "alloc_bytes", "api_hit", "api_miss", "peer_hit", "peer_miss", "discarded_source", "discarded_hot", "hot_hit"} {
			delta := a.Values[key] - b.Values[key]
			if delta < 0 {
				return nil, errors.New("counter reset inside measurement stage")
			}
			out[key] += delta
		}
		for _, key := range []string{"rss", "heap", "cache_bytes", "entries", "capacity", "hot_bytes", "hot_capacity"} {
			out[key+"_after"] += a.Values[key]
		}
	}
	return out, nil
}
