// Package probe records functional continuity while an external script changes
// a dedicated cluster. Its timing samples are diagnostics, not capacity evidence.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/bench/democlient"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/bench/measurement"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/demo"
)

type Options struct {
	Origin, Dir        string
	Workers            int
	Duration, Interval time.Duration
}
type Summary struct {
	Environment     string   `json:"environment"`
	Scope           string   `json:"scope"`
	LatencyMethod   string   `json:"latency_method"`
	Workers         int      `json:"workers"`
	Requests        int      `json:"requests"`
	Errors          int      `json:"errors"`
	Incorrect       int      `json:"incorrect_values"`
	ElapsedSeconds  float64  `json:"elapsed_seconds"`
	P50             float64  `json:"p50_ms"`
	P95             float64  `json:"p95_ms"`
	P99             float64  `json:"p99_ms"`
	FailureExamples []string `json:"failure_examples"`
	StopSeen        bool     `json:"stop_seen"`
	Status          string   `json:"status"`
}

func normalize(o Options) (Options, error) {
	if o.Workers == 0 {
		o.Workers = 4
	}
	if o.Duration == 0 {
		o.Duration = 3 * time.Minute
	}
	if o.Interval == 0 {
		o.Interval = 100 * time.Millisecond
	}
	if err := democlient.ValidateOrigin(o.Origin); err != nil {
		return o, err
	}
	if o.Dir == "" || o.Workers < 1 || o.Workers > 32 || o.Duration <= 0 || o.Duration > 3*time.Minute || o.Interval <= 0 || o.Interval > time.Second {
		return o, errors.New("invalid bounded continuity probe options")
	}
	return o, nil
}
func Run(parent context.Context, options Options) (Summary, error) {
	o, err := normalize(options)
	if err != nil {
		return Summary{}, err
	}
	if err := os.MkdirAll(o.Dir, 0755); err != nil {
		return Summary{}, err
	}
	client := democlient.NewClient(o.Workers, 2*time.Second)
	defer client.CloseIdleConnections()
	work, cancel := context.WithTimeout(parent, o.Duration)
	defer cancel()
	summary := Summary{Environment: "dedicated Kind, stable entry Pod via port-forward", Scope: "functional continuity; loopback/port-forward timings are diagnostic observations, not a throughput or SLO baseline", LatencyMethod: "nearest-rank", Workers: o.Workers}
	var mu sync.Mutex
	var durations []float64
	var stopped atomic.Bool
	var controlErr error
	var wg sync.WaitGroup
	start := time.Now()
	for worker := 0; worker < o.Workers; worker++ {
		wg.Go(func() {
			for i := 0; work.Err() == nil; i++ {
				_, err := os.Stat(filepath.Join(o.Dir, "probe.stop"))
				if err == nil {
					stopped.Store(true)
					return
				}
				if !errors.Is(err, os.ErrNotExist) {
					mu.Lock()
					controlErr = err
					mu.Unlock()
					cancel()
					return
				}
				key := fmt.Sprintf("Auto-%d", (worker*2000+i)%demo.AutoKeys)
				result := democlient.Read(work, client, o.Origin, key, 0)
				mu.Lock()
				durations = append(durations, result.ElapsedMS)
				if result.Failed {
					summary.Errors++
				}
				if result.Incorrect {
					summary.Incorrect++
				}
				if (result.Failed || result.Incorrect) && len(summary.FailureExamples) < 10 {
					summary.FailureExamples = append(summary.FailureExamples, result.Detail)
				}
				mu.Unlock()
				timer := time.NewTimer(o.Interval)
				select {
				case <-work.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		})
	}
	wg.Wait()
	summary.ElapsedSeconds = time.Since(start).Seconds()
	summary.Requests = len(durations)
	summary.StopSeen = stopped.Load()
	q := measurement.Quantiles(durations)
	summary.P50, summary.P95, summary.P99 = q.P50, q.P95, q.P99
	summary.Status = "passed"
	if summary.Requests == 0 || summary.Errors != 0 || summary.Incorrect != 0 || !summary.StopSeen || work.Err() != nil || controlErr != nil {
		summary.Status = "failed"
	}
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return summary, err
	}
	if err := os.WriteFile(filepath.Join(o.Dir, "probe.json"), append(data, '\n'), 0644); err != nil {
		return summary, err
	}
	if summary.Status != "passed" {
		return summary, errors.Join(errors.New("continuity probe failed; evidence retained"), work.Err(), controlErr)
	}
	return summary, nil
}
