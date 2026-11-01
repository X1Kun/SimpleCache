package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/bench/diagnostics"
)

func collectPerformance(dir string, s *Stage) *diagnostics.Summary {
	b, err := os.ReadFile(filepath.Join(dir, "performance.json"))
	if err != nil {
		s.Failures = append(s.Failures, "missing performance evidence: "+err.Error())
		return nil
	}
	var summary diagnostics.Summary
	if err = json.Unmarshal(b, &summary); err != nil {
		s.Failures = append(s.Failures, "invalid performance evidence: "+err.Error())
		return nil
	}
	passed, failed, skipped, pending := 0, 0, 0, 0
	seen := map[string]bool{}
	expected := map[string]bool{}
	for _, scenario := range []string{"warm-hot", "distinct-keys", "capacity-pressure"} {
		for repeat := 1; repeat <= summary.Repeats && repeat <= 5; repeat++ {
			expected[fmt.Sprintf("baseline-%s-%d", scenario, repeat)] = true
		}
		expected["profile-"+scenario+"-1"] = true
	}
	for _, trial := range summary.Trials {
		phase := "baseline"
		if trial.Profiled {
			phase = "profile"
		}
		identity := fmt.Sprintf("%s-%s-%d", phase, trial.Scenario, trial.Repeat)
		if trial.ServerPID <= 0 || trial.ClientPID <= 0 || trial.ServerPID == trial.ClientPID || trial.Requests == 0 || trial.Errors != 0 || trial.Incorrect != 0 || trial.Failure != "" || seen[trial.ID] || !expected[trial.ID] || identity != trial.ID {
			failed++
			s.Failures = append(s.Failures, "performance trial failed: "+trial.ID+" "+trial.Failure)
		} else {
			passed++
		}
		seen[trial.ID] = true
	}
	if summary.Status != "passed" || summary.Failure != "" || len(summary.Trials) != 3*summary.Repeats+3 || summary.Repeats < 1 || (!summary.Quick && summary.Repeats < 3) {
		s.Failures = append(s.Failures, "incomplete or failed performance run: "+summary.Failure)
	}
	if summary.Seconds < 1 || summary.Seconds > 30 || (!summary.Quick && summary.Seconds < 10) || summary.Workers < 1 || summary.Workers > 32 || summary.Repeats > 5 || summary.SourceDelayMS != 100 || summary.ServerGOMAXPROCS != 2 || summary.ThinkMS != 1 || len(summary.ServerSHA256) != 64 {
		s.Failures = append(s.Failures, "invalid performance configuration metadata")
	}
	s.Passed, s.Failed, s.Skipped, s.Pending = &passed, &failed, &skipped, &pending
	s.CountSource = "diagnostic-trials"
	return &summary
}

func renderPerformance(md *strings.Builder, summary diagnostics.Summary) {
	md.WriteString("\n## Separate-process investigation\n\n")
	fmt.Fprintf(md, "Server binary SHA256: `%s`\n\n", summary.ServerSHA256)
	fmt.Fprintf(md, "%d seconds per trial, %d unprofiled repetitions per scenario, %d workers, %dms think time, %dms synthetic source delay, server GOMAXPROCS=%d. Each trial starts a fresh server with TTL=1 hour.\n\n", summary.Seconds, summary.Repeats, summary.Workers, summary.ThinkMS, summary.SourceDelayMS, summary.ServerGOMAXPROCS)
	if summary.Quick {
		md.WriteString("**QUICK FUNCTIONAL SMOKE ONLY: not a performance baseline.**\n\n")
	}
	md.WriteString("Profiles are separate diagnostic trials and are excluded from the baseline aggregates. Closed-loop completion rates are not maximum service capacity. RSS is sampled, not a guaranteed peak; GOMAXPROCS is not a cgroup CPU quota.\n\n")
	md.WriteString("| Scenario | Baseline runs | P95 median [min,max] ms | P99 median [min,max] ms | CPU ms/request median |\n| --- | --- | --- | --- | --- |\n")
	for _, scenario := range []string{"warm-hot", "distinct-keys", "capacity-pressure"} {
		var p95, p99, cpu []float64
		for _, t := range summary.Trials {
			if t.Scenario == scenario && !t.Profiled && t.Failure == "" && t.Requests > 0 {
				p95 = append(p95, t.P95)
				p99 = append(p99, t.P99)
				cpu = append(cpu, 1000*t.CPUSeconds/float64(t.Requests))
			}
		}
		if len(p95) == 0 {
			continue
		}
		sort.Float64s(p95)
		sort.Float64s(p99)
		sort.Float64s(cpu)
		fmt.Fprintf(md, "| %s | %d | %.3f [%.3f, %.3f] | %.3f [%.3f, %.3f] | %.4f |\n", scenario, len(p95), median(p95), p95[0], p95[len(p95)-1], median(p99), p99[0], p99[len(p99)-1], median(cpu))
	}
	md.WriteString("\nAggregate values are medians/ranges of per-run percentiles, not pooled request percentiles or confidence intervals.\n\n| Trial | Server/client PID | Requests | Errors/wrong | P50/P95/P99 ms | Source loads | CPU seconds | Max observed RSS MiB |\n| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, t := range summary.Trials {
		fmt.Fprintf(md, "| [%s](%s/server.log) | %d/%d | %d | %d/%d | %.3f/%.3f/%.3f | %.0f | %.3f | %.2f |\n", t.ID, t.ID, t.ServerPID, t.ClientPID, t.Requests, t.Errors, t.Incorrect, t.P50, t.P95, t.P99, t.SourceLoads, t.CPUSeconds, t.MaxObservedRSS/(1<<20))
	}
	md.WriteString("\nWarm hot runs preload Auto-0; distinct-key runs preload Auto-0 but read fresh Auto-1 onward without wrapping; capacity runs warm a 512-key x 4KiB working set against a 1MiB cache. Warmup is excluded from metrics deltas. Source values/Bloom construction remain part of process memory. Measurements include loopback transport and periodic metrics scrapes. CPU/allocs/mutex/block captures span the diagnostic window; forced GC flushes warmup heap accounting before capture and heap retention is captured after traffic with forced GC. Allocation tops explicitly use alloc_space; retention tops use inuse_space. Profile instrumentation/GC costs remain in diagnostic trials, not baselines. Profile directories include the matched server binary at the run root, raw metrics, and top summaries.\n\nNo cache-path optimization or improvement claim is implied by this baseline; investigate server-only profiles before making a comparable before/after change.\n")
}

func median(sorted []float64) float64 {
	i := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[i-1] + sorted[i]) / 2
	}
	return sorted[i]
}
