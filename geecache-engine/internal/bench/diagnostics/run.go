// Package diagnostics runs bounded, separate-process server investigations.
package diagnostics

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type Options struct {
	Server, Dir               string
	Seconds, Repeats, Workers int
	Quick                     bool
}

type Summary struct {
	Status           string  `json:"status"`
	Environment      string  `json:"environment"`
	Seconds          int     `json:"seconds"`
	Repeats          int     `json:"repeats"`
	Workers          int     `json:"workers"`
	ThinkMS          int     `json:"think_ms"`
	SourceDelayMS    int     `json:"source_delay_ms"`
	ServerGOMAXPROCS int     `json:"server_gomaxprocs"`
	Quick            bool    `json:"quick_smoke_only"`
	Trials           []Trial `json:"trials"`
	Failure          string  `json:"failure,omitempty"`
	ServerSHA256     string  `json:"server_binary_sha256"`
}

type Trial struct {
	ID             string             `json:"id"`
	Scenario       string             `json:"scenario"`
	Repeat         int                `json:"repeat"`
	Profiled       bool               `json:"profiled"`
	ServerPID      int                `json:"server_pid"`
	ClientPID      int                `json:"client_pid"`
	Capacity       int64              `json:"capacity_bytes"`
	ValueBytes     int                `json:"value_bytes"`
	WorkingKeys    int                `json:"working_keys"`
	WarmupRequests int                `json:"warmup_requests"`
	Requests       int                `json:"requests"`
	Errors         int                `json:"errors"`
	Incorrect      int                `json:"incorrect_values"`
	Seconds        float64            `json:"elapsed_seconds"`
	P50            float64            `json:"p50_ms"`
	P95            float64            `json:"p95_ms"`
	P99            float64            `json:"p99_ms"`
	SourceLoads    float64            `json:"source_loads_delta"`
	CPUSeconds     float64            `json:"server_cpu_seconds_delta"`
	MaxObservedRSS float64            `json:"max_observed_rss_bytes"`
	Before         map[string]float64 `json:"metrics_before"`
	After          map[string]float64 `json:"metrics_after"`
	Samples        []ResourceSample   `json:"resource_samples"`
	Failure        string             `json:"failure,omitempty"`
}

type ResourceSample struct {
	Seconds float64 `json:"elapsed_seconds"`
	RSS     float64 `json:"rss_bytes"`
	Heap    float64 `json:"heap_alloc_bytes"`
	CPU     float64 `json:"cpu_seconds"`
}

func (o Options) Validate() error {
	minimum := 10
	if o.Quick {
		minimum = 1
	}
	if o.Server == "" || o.Dir == "" || o.Seconds < minimum || o.Seconds > 30 || o.Repeats < 1 || o.Repeats > 5 || (!o.Quick && o.Repeats < 3) || o.Workers < 1 || o.Workers > 32 {
		return errors.New("require server/dir, 10-30 seconds, 3-5 repeats and 1-32 workers; quick explicitly permits 1 second/1 repeat")
	}
	return nil
}

// Run always writes a partial summary on failure; each trial owns a fresh server.
func Run(ctx context.Context, o Options) (summary Summary, result error) {
	summary = Summary{Status: "failed", Environment: "separate client/server processes on one host; synthetic immutable source", Seconds: o.Seconds, Repeats: o.Repeats, Workers: o.Workers, ThinkMS: 1, SourceDelayMS: 100, ServerGOMAXPROCS: 2, Quick: o.Quick}
	if err := o.Validate(); err != nil {
		return summary, err
	}
	if err := os.MkdirAll(o.Dir, 0755); err != nil {
		return summary, err
	}
	defer func() {
		if result != nil {
			summary.Failure = result.Error()
		}
		b, err := json.MarshalIndent(summary, "", "  ")
		if err == nil {
			err = os.WriteFile(filepath.Join(o.Dir, "performance.json"), append(b, '\n'), 0644)
		}
		result = errors.Join(result, err)
	}()
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	f, err := os.Open(o.Server)
	if err != nil {
		return summary, err
	}
	hash := sha256.New()
	_, hashErr := io.Copy(hash, f)
	closeErr := f.Close()
	if err = errors.Join(hashErr, closeErr); err != nil {
		return summary, err
	}
	summary.ServerSHA256 = fmt.Sprintf("%x", hash.Sum(nil))
	for _, profiled := range []bool{false, true} {
		repeats := o.Repeats
		if profiled {
			repeats = 1
		}
		for repeat := 1; repeat <= repeats; repeat++ {
			for _, scenario := range []string{"warm-hot", "distinct-keys", "capacity-pressure"} {
				if err := ctx.Err(); err != nil {
					return summary, err
				}
				trial, err := runTrial(ctx, o, scenario, repeat, profiled)
				summary.Trials = append(summary.Trials, trial)
				fmt.Printf("Trial %s: requests=%d errors=%d incorrect=%d p95=%.3fms source_loads=%.0f\n", trial.ID, trial.Requests, trial.Errors, trial.Incorrect, trial.P95, trial.SourceLoads)
				if err != nil {
					return summary, fmt.Errorf("%s: %w", trial.ID, err)
				}
			}
		}
	}
	summary.Status = "passed"
	return summary, nil
}

func runTrial(ctx context.Context, o Options, scenario string, repeat int, profiled bool) (trial Trial, result error) {
	phase := "baseline"
	if profiled {
		phase = "profile"
	}
	trial = Trial{ID: fmt.Sprintf("%s-%s-%d", phase, scenario, repeat), Scenario: scenario, Repeat: repeat, Profiled: profiled, ClientPID: os.Getpid(), Capacity: 64 << 20, WorkingKeys: 10000}
	if scenario == "warm-hot" {
		trial.WorkingKeys = 1
	}
	if scenario == "capacity-pressure" {
		trial.Capacity = 1 << 20
		trial.ValueBytes = 4096
		trial.WorkingKeys = 512
	}
	dir := filepath.Join(o.Dir, trial.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return trial, err
	}
	service, err := startServer(ctx, o.Server, dir, trial.Capacity, trial.ValueBytes, profiled)
	if err != nil {
		return trial, err
	}
	trial.ServerPID = service.pid
	defer func() {
		result = errors.Join(result, service.stop())
		if result != nil {
			trial.Failure = result.Error()
		}
	}()
	client := newClient(o.Workers)
	defer client.CloseIdleConnections()
	if err = waitReady(ctx, client, service.api); err != nil {
		return trial, err
	}
	warmCount := 1
	if scenario == "capacity-pressure" {
		warmCount = 512
	}
	if err = warmup(ctx, client, service.api, warmCount, trial.ValueBytes, o.Workers); err != nil {
		return trial, err
	}
	trial.WarmupRequests = warmCount
	trial.Before, err = metrics(ctx, client, service.api, filepath.Join(dir, "metrics-before.txt"))
	if err != nil {
		return trial, err
	}
	profileCtx, cancelProfiles := context.WithTimeout(ctx, time.Duration(o.Seconds+10)*time.Second)
	defer cancelProfiles()
	var profiles <-chan error
	if profiled {
		// Flush startup/warmup heap accounting before delta allocation profiles.
		if err = fetchProfile(profileCtx, service.debug+"/debug/pprof/heap?gc=1", filepath.Join(dir, "heap-before.pprof")); err != nil {
			return trial, err
		}
		profiles = captureProfiles(profileCtx, service.debug, dir, o.Seconds+1)
		// Start profiles before traffic; the extra second covers scheduling/drain.
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return trial, ctx.Err()
		}
	}
	err = load(ctx, client, service.api, o, dir, &trial)
	if err != nil {
		return trial, err
	}
	trial.After, err = metrics(ctx, client, service.api, filepath.Join(dir, "metrics-after.txt"))
	if err != nil {
		return trial, err
	}
	trial.SourceLoads = trial.After["source"] - trial.Before["source"]
	trial.CPUSeconds = trial.After["cpu"] - trial.Before["cpu"]
	trial.MaxObservedRSS = max(trial.MaxObservedRSS, trial.Before["rss"], trial.After["rss"])
	if profiled {
		// Flush post-traffic allocation samples before the delta window closes.
		// GC/capture overhead is intentionally outside all baseline runs.
		if err = fetchProfile(profileCtx, service.debug+"/debug/pprof/heap?gc=1", filepath.Join(dir, "heap.pprof")); err != nil {
			return trial, err
		}
		if err = <-profiles; err != nil {
			return trial, err
		}
	}
	if trial.Requests == 0 || trial.Errors != 0 || trial.Incorrect != 0 {
		return trial, errors.New("load failed value/status checks")
	}
	if trial.After["cache_bytes"] > float64(trial.Capacity) {
		return trial, errors.New("logical capacity exceeded")
	}
	if scenario == "warm-hot" && trial.SourceLoads != 0 {
		return trial, errors.New("warm hot key unexpectedly reloaded")
	}
	if scenario == "distinct-keys" && trial.SourceLoads != float64(trial.Requests) {
		return trial, errors.New("distinct-key cold loads did not match requests")
	}
	if scenario == "capacity-pressure" && (trial.SourceLoads == 0 || trial.After["removals"] <= trial.Before["removals"]) {
		return trial, errors.New("capacity run did not exercise eviction/reload")
	}
	return trial, nil
}
