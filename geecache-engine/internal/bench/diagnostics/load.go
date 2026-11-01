package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/demo"
)

func newClient(workers int) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConns = workers + 4
	transport.MaxIdleConnsPerHost = workers + 4
	return &http.Client{Transport: transport, Timeout: 3 * time.Second}
}

func waitReady(ctx context.Context, c *http.Client, origin string) error {
	work, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		req, err := http.NewRequestWithContext(work, http.MethodGet, origin+"/readyz", nil)
		if err != nil {
			return err
		}
		r, err := c.Do(req)
		if err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return nil
			}
		}
		select {
		case <-work.Done():
			return work.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func readValue(ctx context.Context, client *http.Client, origin, key string, valueBytes int) (bad, wrong bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api?key="+key, nil)
	if err != nil {
		return true, false
	}
	r, err := client.Do(req)
	if err != nil {
		return true, false
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil || r.StatusCode != 200 {
		return true, false
	}
	return false, string(b) != demo.AutoValue(key, valueBytes)
}

func warmup(ctx context.Context, c *http.Client, origin string, count, valueBytes, workers int) error {
	var next, failures atomic.Int64
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := next.Add(1) - 1
				if int(i) >= count {
					return
				}
				key := fmt.Sprintf("Auto-%d", i)
				// Cold distinct-key traffic starts at Auto-1, outside the warmup set.
				bad, wrong := readValue(ctx, c, origin, key, valueBytes)
				if bad || wrong {
					failures.Add(1)
					return
				}
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 {
		return errors.New("warmup failed")
	}
	return ctx.Err()
}

func metrics(ctx context.Context, c *http.Client, origin, path string) (map[string]float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/metrics", nil)
	if err != nil {
		return nil, err
	}
	r, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if r.StatusCode != 200 {
		return nil, fmt.Errorf("metrics HTTP %d", r.StatusCode)
	}
	if path != "" {
		if err = os.WriteFile(path, b, 0644); err != nil {
			return nil, err
		}
	}
	return parseMetrics(string(b))
}

func parseMetrics(body string) (map[string]float64, error) {
	out := map[string]float64{"source": 0, "removals": 0}
	seen := map[string]bool{}
	names := map[string]string{"process_cpu_seconds_total": "cpu", "process_resident_memory_bytes": "rss", "go_memstats_heap_alloc_bytes": "heap", "simplecache_cache_bytes": "cache_bytes", "simplecache_cache_removals_total": "removals"}
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		name := fields[0]
		if pos := strings.IndexByte(name, '{'); pos >= 0 {
			name = name[:pos]
		}
		alias, known := names[name]
		if name == "simplecache_source_loads_total" {
			alias = "source"
			known = true
		}
		if !known {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return nil, err
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, fmt.Errorf("invalid metric %s", name)
		}
		seen[alias] = true
		if alias == "source" {
			out[alias] += value
		} else {
			out[alias] = value
		}
	}
	for _, name := range []string{"cpu", "rss", "heap", "cache_bytes", "source", "removals"} {
		if !seen[name] {
			return nil, fmt.Errorf("missing server metric %s", name)
		}
	}
	return out, nil
}

func load(ctx context.Context, c *http.Client, origin string, o Options, dir string, trial *Trial) error {
	start := time.Now()
	deadline := start.Add(time.Duration(o.Seconds) * time.Second)
	var next atomic.Int64
	var mu sync.Mutex
	var durations []float64
	var wg sync.WaitGroup
	var samplingError error
	sampleCtx, cancelSampling := context.WithCancel(ctx)
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-sampleCtx.Done():
				return
			case <-ticker.C:
				m, err := metrics(sampleCtx, c, origin, "")
				if err != nil {
					if sampleCtx.Err() == nil {
						samplingError = err
					}
					return
				}
				trial.Samples = append(trial.Samples, ResourceSample{Seconds: time.Since(start).Seconds(), RSS: m["rss"], Heap: m["heap"], CPU: m["cpu"]})
				trial.MaxObservedRSS = max(trial.MaxObservedRSS, m["rss"])
			}
		}
	}()
	for worker := 0; worker < o.Workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) && ctx.Err() == nil {
				i := next.Add(1)
				index := int64(0)
				switch trial.Scenario {
				case "distinct-keys":
					index = i
				case "capacity-pressure":
					index = (i - 1) % int64(trial.WorkingKeys)
				}
				began := time.Now()
				bad, wrong := readValue(ctx, c, origin, fmt.Sprintf("Auto-%d", index), trial.ValueBytes)
				mu.Lock()
				durations = append(durations, float64(time.Since(began).Microseconds())/1000)
				if bad {
					trial.Errors++
				}
				if wrong {
					trial.Incorrect++
				}
				mu.Unlock()
				timer := time.NewTimer(time.Millisecond)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return
				}
			}
		}()
	}
	wg.Wait()
	cancelSampling()
	<-sampled
	trial.Seconds = time.Since(start).Seconds()
	trial.Requests = len(durations)
	sort.Float64s(durations)
	if len(durations) > 0 {
		p := func(q float64) float64 { return durations[int(float64(len(durations)-1)*q)] }
		trial.P50, trial.P95, trial.P99 = p(.5), p(.95), p(.99)
	}
	if trial.Scenario == "distinct-keys" && next.Load() >= demo.AutoKeys {
		return errors.New("finite cold dataset exhausted; reduce workers/duration")
	}
	return errors.Join(ctx.Err(), samplingError)
}
