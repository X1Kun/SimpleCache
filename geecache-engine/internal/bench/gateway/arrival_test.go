package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func arrivalConfig(endpoint string) Config {
	c := config(endpoint)
	c.Scenario = "arrival"
	c.Distribution = "hit-miss"
	c.MaxPending = 4
	c.Stages = []RateStage{{Name: "baseline", Rate: 10, Seconds: 1}}
	return c
}
func TestArrivalValidationAndDeterministicCohorts(t *testing.T) {
	c := arrivalConfig("http://localhost:8000")
	if err := Validate(c); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(arrivalPlan(c), arrivalPlan(c)) {
		t.Fatal("trace changed")
	}
	jobs := arrivalPlan(c)
	if len(jobs) != 10 || !jobs[1].missing || jobs[0].missing {
		t.Fatal(jobs)
	}
	for _, distribution := range []string{"hot", "scan", "zipf", "hot-scan", "distinct"} {
		c.Distribution = distribution
		c.Keys = 16
		if err := Validate(c); err != nil {
			t.Fatal(distribution, err)
		}
		jobs := arrivalPlan(c)
		if distribution == "distinct" {
			for i, j := range jobs {
				if j.key != i {
					t.Fatal(jobs)
				}
			}
		}
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Stages = nil }, func(c *Config) { c.Stages[0].Rate = 10001 }, func(c *Config) { c.MaxPending = 4097 }, func(c *Config) { c.ThinkMillis = 1 }, func(c *Config) { c.Stages = append(c.Stages, c.Stages[0]) }, func(c *Config) { c.Distribution = "unknown" }, func(c *Config) { c.Distribution = "distinct"; c.Keys = 1 },
	} {
		c := arrivalConfig("http://localhost:8000")
		mutate(&c)
		if Validate(c) == nil {
			t.Fatal("invalid arrival accepted", c)
		}
	}
}
func TestArrivalCohortsAndRawEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		w.Header().Set("X-SimpleCache-Node", "node")
		if strings.Contains(key, "-missing-") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write(Value(key, 1, 128))
	}))
	defer server.Close()
	dir := t.TempDir()
	summary, err := Run(context.Background(), arrivalConfig(server.URL), dir)
	if err != nil || summary.Attempted != 10 || summary.Success != 10 || summary.Arrival.Status != "passed" {
		t.Fatal(summary, err)
	}
	groups := summary.Arrival.Stages[0].Cohorts
	if groups["missing"].Success != 5 || groups["existing"].Success != 5 {
		t.Fatal(groups)
	}
	data, err := os.ReadFile(filepath.Join(dir, "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r.TotalMS < r.QueueMS || r.TotalMS < r.ElapsedMS || r.DropReason != "" {
			t.Fatal(r)
		}
	}
}
func TestArrivalBoundsWorkAndRetainsDrops(t *testing.T) {
	var active, maxActive atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maxActive.Load(); n > old; old = maxActive.Load() {
			if maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
		http.Error(w, "busy", 503)
	}))
	defer server.Close()
	c := arrivalConfig(server.URL)
	c.Workers = 1
	c.MaxPending = 1
	c.Stages[0].Rate = 100
	dir := t.TempDir()
	summary, err := Run(context.Background(), c, dir)
	if err == nil || summary.Skipped == 0 || maxActive.Load() != 1 || summary.Attempted+summary.Skipped != 100 || summary.Errors != summary.Attempted {
		t.Fatal(summary, err, maxActive.Load())
	}
	if summary.Arrival.Status != "client-limited" {
		t.Fatal(summary.Arrival)
	}
	if _, err := os.Stat(filepath.Join(dir, "summary.json")); err != nil {
		t.Fatal("lost failure evidence", err)
	}
}
func TestArrivalCancellationAndIncorrectPayload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	summary, err := Run(ctx, arrivalConfig("http://localhost:8000"), t.TempDir())
	if err == nil || summary.Skipped != 10 || summary.Attempted != 0 || summary.Arrival.Status != "failed" {
		t.Fatal(summary, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-SimpleCache-Node", "node")
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write(Value("wrong-key", 1, 128))
	}))
	defer server.Close()
	summary, err = Run(context.Background(), arrivalConfig(server.URL), t.TempDir())
	if err == nil || summary.Incorrect == 0 || summary.Arrival.Status != "failed" {
		t.Fatal(summary, err)
	}
}

func TestArrivalActiveCancellationStillSavesSchedule(t *testing.T) {
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := arrivalConfig(server.URL)
	dir := t.TempDir()
	type outcome struct {
		summary Summary
		err     error
	}
	done := make(chan outcome, 1)
	go func() { s, e := Run(ctx, c, dir); done <- outcome{s, e} }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not enter")
	}
	cancel()
	select {
	case out := <-done:
		if out.err == nil || out.summary.Attempted == 0 || out.summary.Attempted+out.summary.Skipped != 10 || out.summary.Arrival.Status != "failed" {
			t.Fatal(out)
		}
		if _, err := os.Stat(filepath.Join(dir, "requests.jsonl")); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not drain workers")
	}
}

func TestArrivalHTTPFailuresAreNotCleanRuns(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "busy", 503) }))
	defer server.Close()
	c := arrivalConfig(server.URL)
	c.Stages[0].Rate = 2
	summary, err := Run(context.Background(), c, t.TempDir())
	if err == nil || summary.Arrival.Status != "completed-with-errors" || summary.Success != 0 || summary.Errors != 2 || summary.Skipped != 0 {
		t.Fatal(summary, err)
	}
}

func TestArrivalLagBudgetAndCatchUpAreIndependentOfRate(t *testing.T) {
	c := Config{MaxScheduleLagMillis: 5, MaxCatchUp: 4}
	interval := time.Second / 1500
	if arrivalDispatchDrop(time.Millisecond, interval, Config{}, 0) != "scheduler_late" {
		t.Fatal("legacy policy changed")
	}
	if arrivalDispatchDrop(time.Millisecond, interval, c, 0) != "" {
		t.Fatal("small jitter was rejected")
	}
	if arrivalDispatchDrop(5*time.Millisecond, interval, c, 0) != "scheduler_late" {
		t.Fatal("budget exceeded")
	}
	if arrivalDispatchDrop(time.Millisecond, interval, c, 4) != "catch_up_limit" {
		t.Fatal("unbounded catch-up")
	}
	for _, rate := range []int{100, 1500, 10000} {
		if arrivalDispatchDrop(2*time.Millisecond, time.Second/time.Duration(rate), c, 0) != "" {
			t.Fatal(rate)
		}
	}
}

func TestArrivalSchedulerOptionsValidation(t *testing.T) {
	for _, change := range []func(*Config){
		func(c *Config) { c.MaxScheduleLagMillis = -1 }, func(c *Config) { c.MaxScheduleLagMillis = 51 },
		func(c *Config) { c.MaxCatchUp = -1 }, func(c *Config) { c.MaxCatchUp = 65 },
		func(c *Config) { c.Scenario = "warm-hot"; c.Stages = nil; c.MaxScheduleLagMillis = 5 },
	} {
		c := arrivalConfig("http://localhost:8000")
		change(&c)
		if Validate(c) == nil {
			t.Fatal("invalid scheduler options accepted", c)
		}
	}
}

func TestArrivalEightSlotBudgetStillRejectsExpiredWork(t *testing.T) {
	c := Config{MaxScheduleLagMillis: 5, MaxCatchUp: 8}
	interval := time.Second / 1500
	if arrivalDispatchDrop(4*time.Millisecond, interval, c, 7) != "" {
		t.Fatal("eighth slot inside budget rejected")
	}
	if arrivalDispatchDrop(4*time.Millisecond, interval, c, 8) != "catch_up_limit" {
		t.Fatal("burst cap bypassed")
	}
	if arrivalDispatchDrop(5*time.Millisecond, interval, c, 0) != "scheduler_late" {
		t.Fatal("lag budget bypassed")
	}
}
