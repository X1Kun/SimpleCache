package productcompare

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func median(values []float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	if len(sorted)%2 == 0 {
		return (sorted[len(sorted)/2-1] + sorted[len(sorted)/2]) / 2
	}
	return sorted[len(sorted)/2]
}

func reportMarkdown(summary Summary) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# PostgreSQL cache comparison\n\nStatus: **%s**\n\n", summary.Status)
	if summary.Failure != "" {
		fmt.Fprintf(&out, "Failure: %s\n\n", summary.Failure)
	}
	if summary.Options.Quick {
		out.WriteString("**Quick functional smoke only; this is not a performance baseline.**\n\n")
	}
	fmt.Fprintf(&out, "Three cache subprocesses, %d logical bytes each (%d total), %d keys × %d value bytes; actual key/value working set %d bytes. Seed %d; %d workers; %d capacity passes; %d repeats.\n\n", summary.Options.CacheBytes, 3*summary.Options.CacheBytes, summary.Options.Keys, summary.Options.ValueBytes, summary.WorkingSetBytes, summary.Options.Seed, summary.Options.Workers, summary.Options.Passes, summary.Options.Repeats)
	fmt.Fprintf(&out, "PostgreSQL %s; client PID %d; fixture SHA256 `%s`. Owned schema `%s`; dropped: %t. Per-owner logical bytes: %v.\n\n", summary.DatabaseVersion, summary.ClientPID, summary.ServerSHA256, summary.Schema, summary.SchemaDropped, summary.OwnerBytes)
	out.WriteString("## Measurement contract\n\n")
	out.WriteString("- All modes use the real PostgreSQL source, cache policy, API/peer handlers and metrics. The loopback fixture owns its startup/shutdown and is not the deployed executable. Every trial starts fresh cache processes with GOMAXPROCS=2, 32 source slots/connections per node, and one-hour source-cache TTL. The opt-in coordinated-hot variant reserves one eighth of the same total budget for peer replicas with at most one-second TTL.\n")
	out.WriteString("- Independent mode disables peer **reads** only. All modes know the same three members and broadcast the same writes/invalidations. Odd repeats run independent first; even repeats reverse the variant order. coordinated-global uses the former single epoch; coordinated uses 256 fixed epoch stripes.\n")
	out.WriteString("- The owned dataset is reset before every trial. PostgreSQL/OS buffer caches are not cleared; data can be memory-resident in PostgreSQL. The logical dataset exceeds one cache, not necessarily one process's RSS.\n")
	out.WriteString("- Paired stages use the same indexed key/node/version trace, value bytes and worker bound. Closed-loop completion order and actual arrival timing can differ. Warmup and dataset preparation are excluded from source deltas and read percentiles. Raw JSONL requests and before/after Prometheus text are retained.\n")
	out.WriteString("- Source loads count actual Getter calls across all participating cache processes, not every SQL statement issued by PostgreSQL, setup, or health checks. Source loads/request is not a cache hit ratio: SingleFlight also reduces loads. API and peer lookup hit/miss counters remain separate in JSON.\n")
	out.WriteString("- Peer requests, CPU and cumulative Go allocation deltas, and RSS/heap at stage boundaries are recorded. Allocation totals cover the whole stage, including writes and metric collection; they are not retained memory. RSS is a sum of snapshots, not a continuously sampled peak. HTTP bodies, protobuf headers and TCP overhead are not measured as byte traffic.\n")
	out.WriteString("- Cold-key latency includes a controlled PostgreSQL table-lock interval (at most 500ms to observe loading processes), so use it to verify fill coalescing, not as ordinary database latency. Updates are four sequential commits followed by reads after acknowledgements, not a strong-consistency stress test.\n")
	out.WriteString("- Zipf reads use s=1.2 over 512 keys by default. Mixed stages use four times that key count (capped at 4096), 30 windows of 19 reads and one overlapping write, then drain each window. Writes target the hottest key. Older declared versions are recorded as stale-at-start only when older than an acknowledged write known at request start. This is a bounded overlap test, not sustained YCSB or a linearizability checker.\n")
	out.WriteString("- Fault stages stop only node-2, keep stale static membership, and send requests to survivors. Recovery restarts an empty node and explicitly invalidates the key; up to three explicit retry attempts are recorded. Stage snapshots split at the restart so reset counters are never subtracted across PIDs. This does not test blackholed networks or Kubernetes convergence.\n\n")
	out.WriteString("## Raw trial stages\n\n| Trial | Keys | Stage | Reads | Errors / wrong / skipped | Stale at start | Source loads | Peer reads | Fallbacks | P50 / P95 / P99 ms | Seconds | RSS after MiB |\n| --- | ---: | --- | ---: | --- | ---: | ---: | ---: | ---: | --- | ---: | ---: |\n")
	for _, trial := range summary.Trials {
		for _, stage := range trial.Stages {
			loads, peers, fallback, rss := "N/A", "N/A", "N/A", "N/A"
			if stage.Delta != nil {
				loads = fmt.Sprintf("%.0f", stage.Delta["source"])
				peers = fmt.Sprintf("%.0f", stage.Delta["peer"])
				fallback = fmt.Sprintf("%.0f", stage.Delta["fallback"])
				rss = fmt.Sprintf("%.2f", stage.Delta["rss_after"]/(1<<20))
			}
			latency := "N/A"
			if stage.Traffic.Latency.Count > 0 {
				l := stage.Traffic.Latency
				latency = fmt.Sprintf("%.3f / %.3f / %.3f", l.P50, l.P95, l.P99)
			}
			fmt.Fprintf(&out, "| %s | %d | %s | %d | %d / %d / %d | %d | %s | %s | %s | %s | %.3f | %s |\n", trial.ID, trial.WorkingKeys, stage.Name, stage.Traffic.Attempted, stage.Traffic.Errors, stage.Traffic.Incorrect, stage.Traffic.Skipped, stage.Traffic.StaleAtStart, loads, peers, fallback, latency, stage.Traffic.Elapsed, rss)
		}
		if trial.Failure != "" {
			fmt.Fprintf(&out, "\nTrial `%s` failed: %s\n\n", trial.ID, trial.Failure)
		}
	}
	appendPairs(&out, summary, "independent", "coordinated")
	appendPairs(&out, summary, "coordinated-global", "coordinated")
	appendPairs(&out, summary, "coordinated", "coordinated-hot")
	out.WriteString("\nNo latency, throughput, or source-reduction threshold decides success. Correct values, complete requests, valid metrics, capacity bounds and bounded owned-process cleanup decide functional success. Mixed reads can return earlier declared versions: stale-at-start is measured, not mistaken for corruption; final reads after explicit invalidation must match the latest version.\n")
	return out.String()
}

func appendPairs(out *strings.Builder, summary Summary, left, right string) {
	fmt.Fprintf(out, "\n## Paired medians: %s / %s\n\nOnly passed stages with matching trace digests/repeats are paired. Percentiles are medians of trial observations, not pooled samples.\n\n| Scenario / stage | Pairs | Source loads | Peer reads | P95 ms | Discarded source fills | Stale at start | Go allocated MiB |\n| --- | ---: | --- | --- | --- | --- | --- | --- |\n", left, right)
	type pair struct{ independent, coordinated *Stage }
	pairs := map[string]map[int]*pair{}
	for _, trial := range summary.Trials {
		if trial.Status != "passed" {
			continue
		}
		for i := range trial.Stages {
			stage := &trial.Stages[i]
			if stage.Failure != "" || stage.Delta == nil || stage.Traffic.Attempted == 0 {
				continue
			}
			key := trial.Scenario + " / " + stage.Name
			if pairs[key] == nil {
				pairs[key] = map[int]*pair{}
			}
			if pairs[key][trial.Repeat] == nil {
				pairs[key][trial.Repeat] = &pair{}
			}
			if trial.Mode == left {
				pairs[key][trial.Repeat].independent = stage
			} else if trial.Mode == right {
				pairs[key][trial.Repeat].coordinated = stage
			}
		}
	}
	var names []string
	for name := range pairs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var il, cl, ip, cp, it, ct, id, cd, is, cs, ia, ca []float64
		for _, p := range pairs[name] {
			if p.independent == nil || p.coordinated == nil || p.independent.Traffic.TraceSHA256 != p.coordinated.Traffic.TraceSHA256 {
				continue
			}
			il = append(il, p.independent.Delta["source"])
			cl = append(cl, p.coordinated.Delta["source"])
			ip = append(ip, p.independent.Delta["peer"])
			cp = append(cp, p.coordinated.Delta["peer"])
			it = append(it, p.independent.Traffic.Latency.P95)
			ct = append(ct, p.coordinated.Traffic.Latency.P95)
			id = append(id, p.independent.Delta["discarded_source"])
			cd = append(cd, p.coordinated.Delta["discarded_source"])
			is = append(is, float64(p.independent.Traffic.StaleAtStart))
			cs = append(cs, float64(p.coordinated.Traffic.StaleAtStart))
			ia = append(ia, p.independent.Delta["alloc_bytes"]/(1<<20))
			ca = append(ca, p.coordinated.Delta["alloc_bytes"]/(1<<20))
		}
		if len(il) > 0 {
			fmt.Fprintf(out, "| %s | %d | %.0f / %.0f | %.0f / %.0f | %.3f / %.3f | %.0f / %.0f | %.0f / %.0f | %.2f / %.2f |\n", name, len(il), median(il), median(cl), median(ip), median(cp), median(it), median(ct), median(id), median(cd), median(is), median(cs), median(ia), median(ca))
		}
	}
}

func writeReport(dir string, summary Summary) error {
	markdownErr := os.WriteFile(filepath.Join(dir, "report.md"), []byte(reportMarkdown(summary)), 0644)
	if markdownErr != nil {
		summary.Status = "failed"
		summary.Failure = "could not write Markdown report"
	}
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return errors.Join(markdownErr, err)
	}
	jsonErr := os.WriteFile(filepath.Join(dir, "report.json"), append(data, '\n'), 0644)
	return errors.Join(markdownErr, jsonErr)
}
