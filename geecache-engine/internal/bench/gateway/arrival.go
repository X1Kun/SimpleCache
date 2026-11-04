package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// ArrivalCounts partitions every planned request, including client-side drops.
// Percentiles contain attempted requests only; dropped requests have no latency.
type ArrivalCounts struct {
	Dispatched     int            `json:"dispatched"`
	Dispatch       Percentiles    `json:"schedule_to_dispatch"`
	Planned        int            `json:"planned"`
	Attempted      int            `json:"attempted"`
	Success        int            `json:"success"`
	Errors         int            `json:"errors"`
	Incorrect      int            `json:"incorrect"`
	Dropped        int            `json:"dropped"`
	DropReasons    map[string]int `json:"drop_reasons"`
	Statuses       map[int]int    `json:"statuses"`
	Queue          Percentiles    `json:"schedule_to_start"`
	Service        Percentiles    `json:"request_latency"`
	Total          Percentiles    `json:"schedule_to_completion"`
	SuccessLatency Percentiles    `json:"successful_request_latency"`
	ErrorLatency   Percentiles    `json:"failed_request_latency"`
}
type ArrivalStage struct {
	Name    string                   `json:"name"`
	Rate    int                      `json:"rate"`
	Seconds int                      `json:"seconds"`
	Counts  ArrivalCounts            `json:"counts"`
	Cohorts map[string]ArrivalCounts `json:"cohorts"`
}
type ArrivalReport struct {
	Scheduler            string         `json:"scheduler"`
	MaxScheduleLagMillis int            `json:"max_schedule_lag_ms"`
	MaxCatchUp           int            `json:"max_catch_up"`
	StartedUnix          float64        `json:"started_unix"`
	FinishedUnix         float64        `json:"finished_unix"`
	Status               string         `json:"status"`
	Scope                string         `json:"scope"`
	Stages               []ArrivalStage `json:"stages"`
	Counts               ArrivalCounts  `json:"counts"`
}
type arrivalJob struct {
	index, key, stage int
	offset            time.Duration
	cohort            string
	missing           bool
	dispatched        time.Time
}

func validateArrival(c Config) error {
	if c.MaxScheduleLagMillis < 0 || c.MaxScheduleLagMillis > 50 || c.MaxCatchUp < 0 || c.MaxCatchUp > 64 {
		return fmt.Errorf("arrival scheduler requires 0-50ms lag and 0-64 catch-up slots")
	}

	if len(c.Stages) == 0 || len(c.Stages) > 12 || c.MaxPending < 0 || c.MaxPending > 4096 || c.ThinkMillis != 0 || c.Windows != 0 || c.DurationSeconds != 0 {
		return fmt.Errorf("arrival requires 1-12 stages, 0-4096 pending slots and no closed-loop timing options")
	}
	total, seconds := 0, 0
	names := map[string]bool{}
	for _, s := range c.Stages {
		if s.Name == "" || len(s.Name) > 48 || names[s.Name] || s.Rate < 1 || s.Rate > 10000 || s.Seconds < 1 || s.Seconds > 120 {
			return fmt.Errorf("invalid arrival stage")
		}
		names[s.Name] = true
		total += s.Rate * s.Seconds
		seconds += s.Seconds
	}
	if total > 100000 || seconds > 120 {
		return fmt.Errorf("arrival exceeds 100000 requests or 120s schedule")
	}
	switch c.Distribution {
	case "hot", "scan", "zipf", "hot-scan", "hit-miss", "distinct":
	default:
		return fmt.Errorf("invalid arrival distribution")
	}
	if (c.Distribution == "hot-scan" || c.Distribution == "hit-miss") && c.Keys < 2 {
		return fmt.Errorf("mixed cohorts require at least two keys")
	}
	if c.Distribution == "distinct" && total > c.Keys {
		return fmt.Errorf("distinct arrivals require at least one key per planned request")
	}
	return nil
}

// All choices are made before timing starts; concurrency never changes the trace.
func arrivalPlan(c Config) []arrivalJob {
	rng := rand.New(rand.NewSource(c.Seed))
	z := rand.NewZipf(rng, 1.2, 1, uint64(c.Keys-1))
	var jobs []arrivalJob
	var base time.Duration
	for stage, s := range c.Stages {
		for n := 0; n < s.Rate*s.Seconds; n++ {
			i := len(jobs)
			j := arrivalJob{index: i, stage: stage, offset: base + time.Duration(n)*time.Second/time.Duration(s.Rate), cohort: c.Distribution}
			switch c.Distribution {
			case "hot":
				j.key = 0
			case "scan", "distinct":
				j.key = i % c.Keys
			case "zipf":
				j.key = int(z.Uint64())
			case "hot-scan":
				if i%2 == 0 {
					j.cohort = "hot"
				} else {
					j.cohort = "scan"
					j.key = 1 + (i/2)%(c.Keys-1)
				}
			case "hit-miss":
				j.key = (i / 2) % c.Keys
				j.missing = i%2 == 1
				if j.missing {
					j.cohort = "missing"
				} else {
					j.cohort = "existing"
				}
			}
			jobs = append(jobs, j)
		}
		base += time.Duration(s.Seconds) * time.Second
	}
	return jobs
}

func runArrival(ctx context.Context, c Config, dir string) (Summary, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return Summary{}, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = c.Workers
	transport.MaxIdleConnsPerHost = c.Workers
	transport.MaxIdleConns = c.Workers
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: 4 * time.Second}
	defer client.CloseIdleConnections()
	runner := runner{c: c, client: client}
	jobs := arrivalPlan(c)
	encoded, _ := json.Marshal(jobsForHash(jobs))
	digest := sha256.Sum256(encoded)
	records := make([]Record, len(jobs))
	for _, j := range jobs {
		records[j.index] = Record{Index: j.index, Method: "GET", KeyIndex: j.key, Stage: c.Stages[j.stage].Name, Cohort: j.cohort, ScheduledMS: float64(j.offset) / float64(time.Millisecond), DropReason: "canceled"}
	}
	queue := make(chan arrivalJob, c.MaxPending)
	var wg, ready sync.WaitGroup
	ready.Add(c.Workers)
	var start time.Time
	for n := 0; n < c.Workers; n++ {
		wg.Go(func() {
			ready.Done()
			for j := range queue {
				if ctx.Err() != nil {
					records[j.index].Dispatched = true
					records[j.index].DispatchMS = float64(j.dispatched.Sub(start.Add(j.offset))) / float64(time.Millisecond)
					continue
				}
				began := time.Now()
				r := runner.requestExpected(ctx, j.index, j.key, "GET", 0, 1, 1, j.missing)
				r.Dispatched = true
				r.DispatchMS = float64(j.dispatched.Sub(start.Add(j.offset))) / float64(time.Millisecond)
				r.Stage = c.Stages[j.stage].Name
				r.Cohort = j.cohort
				r.ScheduledMS = float64(j.offset) / float64(time.Millisecond)
				r.QueueMS = float64(began.Sub(start.Add(j.offset))) / float64(time.Millisecond)
				r.TotalMS = float64(time.Since(start.Add(j.offset))) / float64(time.Millisecond)
				records[j.index] = r
			}
		})
	}
	ready.Wait()
	start = time.Now()
	catchUp := 0
dispatch:
	for _, j := range jobs {
		due := start.Add(j.offset)
		if delay := time.Until(due); delay > 0 {
			catchUp = 0
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				break dispatch
			case <-timer.C:
			}
		}
		if ctx.Err() != nil {
			break
		}
		interval := time.Second / time.Duration(c.Stages[j.stage].Rate)
		lag := time.Since(due)
		records[j.index].DispatchMS = float64(lag) / float64(time.Millisecond)
		reason := arrivalDispatchDrop(lag, interval, c, catchUp)
		if reason != "" {
			records[j.index].DropReason = reason
			continue
		}
		catchUp++
		j.dispatched = time.Now()

		select {
		case queue <- j:
		default:
			records[j.index].DropReason = "client_capacity"
		}
	}
	close(queue)
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	report := ArrivalReport{Scheduler: "bounded-catch-up-v2", MaxScheduleLagMillis: c.MaxScheduleLagMillis, MaxCatchUp: catchUpLimit(c), StartedUnix: float64(start.UnixNano()) / 1e9, FinishedUnix: float64(time.Now().UnixNano()) / 1e9, Status: "passed", Scope: "Open-loop read-only version-1 fixture; stage/cohort attribution uses planned arrival time; final drain included in elapsed time"}
	report.Counts = arrivalCounts(records)
	for _, s := range c.Stages {
		var subset []Record
		groups := map[string][]Record{}
		for _, r := range records {
			if r.Stage == s.Name {
				subset = append(subset, r)
				groups[r.Cohort] = append(groups[r.Cohort], r)
			}
		}
		stage := ArrivalStage{Name: s.Name, Rate: s.Rate, Seconds: s.Seconds, Counts: arrivalCounts(subset), Cohorts: map[string]ArrivalCounts{}}
		for k, v := range groups {
			stage.Cohorts[k] = arrivalCounts(v)
		}
		report.Stages = append(report.Stages, stage)
	}
	counts := report.Counts
	if counts.Incorrect > 0 || ctx.Err() != nil {
		report.Status = "failed"
	} else if counts.Dropped > 0 {
		report.Status = "client-limited"
	} else if counts.Errors > 0 {
		report.Status = "completed-with-errors"
	}
	summary := Summary{Config: c, GoVersion: runtime.Version(), GOMAXPROCS: runtime.GOMAXPROCS(0), TraceSHA256: hex.EncodeToString(digest[:]), Planned: counts.Planned, Attempted: counts.Attempted, Skipped: counts.Dropped, Success: counts.Success, Errors: counts.Errors, Incorrect: counts.Incorrect, ElapsedSeconds: elapsed, StatusCounts: counts.Statuses, Arrival: &report, Latency: counts.Service, ReadLatency: counts.Service}
	summary.Rejections = counts.Statuses[503]
	if elapsed > 0 {
		summary.SuccessPerSecond = float64(counts.Success) / elapsed
	}
	if err := writeArrival(dir, records, summary); err != nil {
		return summary, err
	}
	if report.Status != "passed" {
		return summary, fmt.Errorf("arrival outcome: %s; raw evidence retained", report.Status)
	}
	return summary, nil
}

// Encode exported fields explicitly: arrivalJob itself is internal state.
func jobsForHash(jobs []arrivalJob) any {
	type trace struct {
		Key, Stage int
		Offset     time.Duration
		Cohort     string
		Missing    bool
	}
	out := make([]trace, len(jobs))
	for i, j := range jobs {
		out[i] = trace{j.key, j.stage, j.offset, j.cohort, j.missing}
	}
	return out
}
func arrivalCounts(records []Record) ArrivalCounts {
	c := ArrivalCounts{Planned: len(records), DropReasons: map[string]int{}, Statuses: map[int]int{}}
	var dispatch, q, service, total, good, bad []float64
	for _, r := range records {
		if r.Dispatched {
			c.Dispatched++
			dispatch = append(dispatch, r.DispatchMS)
		}
		if !r.Attempted {
			c.Dropped++
			c.DropReasons[r.DropReason]++
			continue
		}
		c.Attempted++
		c.Statuses[r.Status]++
		q = append(q, r.QueueMS)
		service = append(service, r.ElapsedMS)
		total = append(total, r.TotalMS)
		if r.Incorrect {
			c.Incorrect++
		}
		if r.Error != "" {
			c.Errors++
		}
		if r.Error == "" && !r.Incorrect {
			c.Success++
			good = append(good, r.ElapsedMS)
		} else {
			bad = append(bad, r.ElapsedMS)
		}
	}
	c.Dispatch = Quantiles(dispatch)
	c.Queue = Quantiles(q)
	c.Service = Quantiles(service)
	c.Total = Quantiles(total)
	c.SuccessLatency = Quantiles(good)
	c.ErrorLatency = Quantiles(bad)
	return c
}
func writeArrival(dir string, records []Record, summary Summary) error {
	f, err := os.Create(filepath.Join(dir, "requests.jsonl"))
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(f)
	for _, r := range records {
		if err := encoder.Encode(r); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), data, 0644); err != nil {
		return err
	}
	var md strings.Builder
	fmt.Fprintf(&md, "# Arrival experiment\n\nOutcome: **%s**. %s\n\nClient drops invalidate a clean capacity claim. Expected missing-key 404 responses count as successful outcomes.\n\n| Stage | Cohort | Planned | Attempted | Successful | Errors | Incorrect | Dropped | Queue P95 ms | Request P95 ms | Total P95 ms |\n| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n", summary.Arrival.Status, summary.Arrival.Scope)
	for _, stage := range summary.Arrival.Stages {
		names := make([]string, 0, len(stage.Cohorts))
		for name := range stage.Cohorts {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			c := stage.Cohorts[name]
			fmt.Fprintf(&md, "| %s | %s | %d | %d | %d | %d | %d | %d | %.3f | %.3f | %.3f |\n", stage.Name, name, c.Planned, c.Attempted, c.Success, c.Errors, c.Incorrect, c.Dropped, c.Queue.P95, c.Service.P95, c.Total.P95)
		}
	}
	return os.WriteFile(filepath.Join(dir, "report.md"), []byte(md.String()), 0644)
}

func catchUpLimit(c Config) int {
	if c.MaxCatchUp == 0 {
		return 4
	}
	return c.MaxCatchUp
}

// A wall-clock lag budget is independent of the rate. Keep the old single-slot
// policy when zero is configured, and bound dispatch bursts even within budget.
func arrivalDispatchDrop(lag, interval time.Duration, c Config, catchUp int) string {
	budget := interval
	if c.MaxScheduleLagMillis > 0 {
		budget = time.Duration(c.MaxScheduleLagMillis) * time.Millisecond
	}
	if lag >= budget {
		return "scheduler_late"
	}
	if catchUp >= catchUpLimit(c) {
		return "catch_up_limit"
	}
	return ""
}
