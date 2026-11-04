package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

func Run(ctx context.Context, c Config, dir string) (Summary, error) {
	if e := Validate(c); e != nil {
		return Summary{}, e
	}
	if e := os.MkdirAll(dir, 0755); e != nil {
		return Summary{}, e
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConns = c.Workers + 4
	transport.MaxIdleConnsPerHost = c.Workers + 4
	transport.MaxConnsPerHost = c.Workers + 4
	client := &http.Client{Transport: transport, Timeout: 4 * time.Second}
	defer client.CloseIdleConnections()
	r := runner{c: c, client: client, token: os.Getenv("CACHE_ADMIN_TOKEN"), expected: make([][]byte, c.Keys)}
	for i := range r.expected {
		value, err := c.productValue(i, 1)
		if err != nil {
			return Summary{}, err
		}
		r.expected[i] = value
	}
	trace := readTrace(c)
	traceBytes, _ := json.Marshal(struct {
		Scenario          string
		Reads             []int
		Windows, Duration int
	}{c.Scenario, trace, c.Windows, c.DurationSeconds})
	traceHash := sha256.Sum256(traceBytes)
	records := make([]Record, len(trace))
	start := time.Now()
	if c.Scenario == "mixed" {
		if r.token == "" {
			return Summary{}, fmt.Errorf("mixed workload requires a token")
		}
		records = nil
		var committed atomic.Int64
		committed.Store(1)
		for window := 0; window < c.Windows; window++ {
			before := int64(window + 1)
			proposed := before + 1
			batch := make([]Record, 20)
			var wg sync.WaitGroup
			slots := make(chan struct{}, c.Workers)
			gate := make(chan struct{})
			wg.Go(func() {
				<-gate
				slots <- struct{}{}
				defer func() { <-slots }()
				batch[0] = r.request(ctx, window*20, 0, "PUT", before, before, proposed)
				if batch[0].Status == 200 && batch[0].Error == "" && !batch[0].Incorrect {
					committed.Store(proposed)
				}
			})
			for j := 1; j < 20; j++ {
				j := j
				wg.Go(func() {
					<-gate
					slots <- struct{}{}
					defer func() { <-slots }()
					k := trace[(window*19+j-1)%len(trace)]
					lo, hi := int64(1), int64(1)
					if k == 0 {
						lo, hi = committed.Load(), proposed
					}
					batch[j] = r.request(ctx, window*20+j, k, "GET", 0, lo, hi)
				})
			}
			close(gate)
			wg.Wait()
			records = append(records, batch...)
			if batch[0].Status != 200 || batch[0].Error != "" || batch[0].Incorrect {
				break
			} // Never blindly retry an ambiguous write.
			if c.ThinkMillis > 0 {
				time.Sleep(time.Duration(c.ThinkMillis) * time.Millisecond)
			}
		}
	} else if c.Scenario == "ttl" {
		records = nil
		deadline := start.Add(time.Duration(c.DurationSeconds) * time.Second)
		for i := 0; time.Now().Before(deadline) && ctx.Err() == nil; i++ {
			records = append(records, r.request(ctx, i, 0, "GET", 0, 1, 1))
			time.Sleep(100 * time.Millisecond)
		}
	} else {
		var next atomic.Int64
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for worker := 0; worker < c.Workers; worker++ {
			wg.Go(func() {
				<-gate
				for {
					index := int(next.Add(1) - 1)
					if index >= len(trace) || ctx.Err() != nil {
						return
					}
					records[index] = r.request(ctx, index, trace[index], "GET", 0, 1, 1)
					if c.ThinkMillis > 0 {
						time.Sleep(time.Duration(c.ThinkMillis) * time.Millisecond)
					}
				}
			})
		}
		close(gate)
		wg.Wait()
	}
	elapsed := time.Since(start).Seconds()
	summary := Summary{Config: c, GoVersion: runtime.Version(), GOMAXPROCS: runtime.GOMAXPROCS(0), TraceSHA256: hex.EncodeToString(traceHash[:]), ElapsedSeconds: elapsed, StatusCounts: map[int]int{}, Planned: len(trace)}
	if c.Scenario == "mixed" {
		summary.Planned = c.Windows * 20
	}
	if c.Scenario == "ttl" {
		summary.Planned = len(records)
	}
	all, reads, writes := []float64{}, []float64{}, []float64{}
	for _, record := range records {
		if !record.Attempted {
			continue
		}
		summary.Attempted++
		summary.StatusCounts[record.Status]++
		all = append(all, record.ElapsedMS)
		if record.Method == "PUT" {
			summary.Writes++
			writes = append(writes, record.ElapsedMS)
		} else {
			reads = append(reads, record.ElapsedMS)
		}
		if record.Error != "" {
			summary.Errors++
		}
		if record.Incorrect {
			summary.Incorrect++
		}
		if record.Status == 503 {
			summary.Rejections++
		}
		if record.StaleAtStart {
			summary.StaleAtStart++
		}
		if record.Error == "" && !record.Incorrect {
			summary.Success++
		}
	}
	summary.Skipped = summary.Planned - summary.Attempted
	summary.Latency = Quantiles(all)
	summary.ReadLatency = Quantiles(reads)
	summary.WriteLatency = Quantiles(writes)
	if elapsed > 0 {
		summary.SuccessPerSecond = float64(summary.Success) / elapsed
	}
	file, e := os.Create(filepath.Join(dir, "requests.jsonl"))
	if e != nil {
		return summary, e
	}
	encoder := json.NewEncoder(file)
	for _, record := range records {
		if e := encoder.Encode(record); e != nil {
			_ = file.Close()
			return summary, e
		}
	}
	if e := file.Close(); e != nil {
		return summary, e
	}
	b, _ := json.MarshalIndent(summary, "", "  ")
	if e := os.WriteFile(filepath.Join(dir, "summary.json"), b, 0644); e != nil {
		return summary, e
	}
	if summary.Attempted == 0 || summary.Skipped > 0 || summary.Errors > 0 || summary.Incorrect > 0 {
		return summary, fmt.Errorf("workload has errors, incorrect values or skipped requests")
	}
	return summary, ctx.Err()
}
