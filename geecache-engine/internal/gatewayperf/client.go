// Package gatewayperf runs bounded, deterministic traffic from a client Pod.
// It contains no cluster administration or production cache behavior.
package gatewayperf

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Endpoint        string `json:"endpoint"`
	Host            string `json:"host"`
	Prefix          string `json:"prefix"`
	Scenario        string `json:"scenario"`
	Keys            int    `json:"keys"`
	ValueBytes      int    `json:"value_bytes"`
	Reads           int    `json:"reads"`
	Workers         int    `json:"workers"`
	ThinkMillis     int    `json:"think_ms"`
	Seed            int64  `json:"seed"`
	Windows         int    `json:"windows"`
	DurationSeconds int    `json:"duration_seconds"`
}
type Payload struct {
	Key     string `json:"key"`
	Version int64  `json:"version"`
	Padding string `json:"padding"`
}

func Key(prefix string, index int) string { return fmt.Sprintf("%s-%04d", prefix, index) }
func Value(key string, version int64, size int) []byte {
	p := Payload{Key: key, Version: version}
	base, _ := json.Marshal(p)
	padding := strings.Builder{}
	for i := 0; padding.Len() < size-len(base); i++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d", key, version, i)))
		padding.WriteString(hex.EncodeToString(sum[:]))
	}
	if size > len(base) {
		p.Padding = padding.String()[:size-len(base)]
	}
	b, _ := json.Marshal(p)
	return b
}
func Validate(c Config) error {
	u, e := url.Parse(c.Endpoint)
	if e != nil || u.Scheme != "http" || u.Host == "" || u.RawQuery != "" || u.User != nil || u.Path != "" || u.Fragment != "" {
		return fmt.Errorf("invalid endpoint")
	}
	if c.Host == "" || c.Prefix == "" || len(c.Prefix) > 80 || c.Keys < 1 || c.Keys > 10000 || c.Workers < 1 || c.Workers > 128 || c.ValueBytes < 128 || c.ValueBytes > 1<<20 || c.Reads < 1 || c.Reads > 1_000_000 || c.ThinkMillis < 0 || c.ThinkMillis > 100 || c.Windows < 0 || c.Windows > 10000 || c.DurationSeconds < 0 || c.DurationSeconds > 120 {
		return fmt.Errorf("invalid bounded workload options")
	}
	switch c.Scenario {
	case "warm-hot", "capacity", "zipf", "cold-key", "mixed", "ttl":
	default:
		return fmt.Errorf("invalid scenario")
	}
	if c.Scenario == "mixed" && c.Windows == 0 {
		return fmt.Errorf("mixed workload requires windows")
	}
	if c.Scenario == "ttl" && c.DurationSeconds < 5 {
		return fmt.Errorf("TTL workload requires a duration")
	}
	return nil
}

type Record struct {
	Index            int     `json:"index"`
	Method           string  `json:"method"`
	KeyIndex         int     `json:"key_index"`
	Attempted        bool    `json:"attempted"`
	Status           int     `json:"status"`
	ElapsedMS        float64 `json:"elapsed_ms"`
	Node             string  `json:"node,omitempty"`
	Version          int64   `json:"version,omitempty"`
	CommittedAtStart int64   `json:"committed_at_start,omitempty"`
	Error            string  `json:"error,omitempty"`
	Incorrect        bool    `json:"incorrect"`
	StaleAtStart     bool    `json:"stale_at_start"`
}
type Percentiles struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50_ms"`
	P95   float64 `json:"p95_ms"`
	P99   float64 `json:"p99_ms"`
}

func Quantiles(values []float64) Percentiles {
	if len(values) == 0 {
		return Percentiles{}
	}
	v := append([]float64(nil), values...)
	sort.Float64s(v)
	at := func(p float64) float64 { return v[int(math.Ceil(p*float64(len(v))))-1] }
	return Percentiles{len(v), at(.5), at(.95), at(.99)}
}

type Summary struct {
	Config           Config      `json:"config"`
	GoVersion        string      `json:"go_version"`
	GOMAXPROCS       int         `json:"gomaxprocs"`
	TraceSHA256      string      `json:"trace_sha256"`
	Planned          int         `json:"planned"`
	Attempted        int         `json:"attempted"`
	Skipped          int         `json:"skipped"`
	Success          int         `json:"success"`
	Errors           int         `json:"errors"`
	Incorrect        int         `json:"incorrect"`
	Rejections       int         `json:"http_503"`
	StaleAtStart     int         `json:"stale_at_start"`
	Writes           int         `json:"writes"`
	ElapsedSeconds   float64     `json:"elapsed_seconds"`
	SuccessPerSecond float64     `json:"success_per_second"`
	Latency          Percentiles `json:"latency_all_attempts"`
	ReadLatency      Percentiles `json:"read_latency"`
	WriteLatency     Percentiles `json:"write_latency"`
	StatusCounts     map[int]int `json:"statuses"`
}

func readTrace(c Config) []int {
	trace := make([]int, c.Reads)
	rng := rand.New(rand.NewSource(c.Seed))
	z := rand.NewZipf(rng, 1.2, 1, uint64(c.Keys-1))
	for i := range trace {
		switch c.Scenario {
		case "warm-hot", "cold-key":
			trace[i] = 0
		case "zipf", "mixed":
			trace[i] = int(z.Uint64())
		default:
			trace[i] = i % c.Keys
		}
	}
	return trace
}

type runner struct {
	c        Config
	client   *http.Client
	token    string
	expected [][]byte
}

func (r *runner) request(ctx context.Context, index, keyIndex int, method string, expected, min, max int64) Record {
	out := Record{Index: index, KeyIndex: keyIndex, Method: method, Attempted: true, CommittedAtStart: min}
	key := Key(r.c.Prefix, keyIndex)
	var body io.Reader
	if method == "PUT" {
		body = bytes.NewReader(Value(key, expected+1, r.c.ValueBytes))
	}
	started := time.Now()
	req, e := http.NewRequestWithContext(ctx, method, r.c.Endpoint+"/api?key="+strings.ReplaceAll(url.QueryEscape(key), "+", "%20"), body)
	if e != nil {
		out.Error = "request construction failed"
		return out
	}
	req.Host = r.c.Host
	if method == "PUT" {
		req.Header.Set("Authorization", "Bearer "+r.token)
		req.Header.Set("If-Match", `"v`+strconv.FormatInt(expected, 10)+`"`)
		req.Header.Set("Content-Type", "application/json")
	}
	response, e := r.client.Do(req)
	if e != nil {
		out.ElapsedMS = float64(time.Since(started)) / float64(time.Millisecond)
		out.Error = "transport or deadline failure"
		return out
	}
	data, e := io.ReadAll(io.LimitReader(response.Body, int64(r.c.ValueBytes+4097)))
	_ = response.Body.Close()
	out.ElapsedMS = float64(time.Since(started)) / float64(time.Millisecond)
	out.Status = response.StatusCode
	out.Node = response.Header.Get("X-SimpleCache-Node")
	if e != nil {
		out.Error = "response read failed"
		return out
	}
	if response.StatusCode != 200 {
		out.Error = "non-200 response"
		return out
	}
	if method == "PUT" {
		var result struct {
			Committed    *bool `json:"source_committed"`
			Version      int64 `json:"source_version"`
			Invalidation struct {
				Complete bool                            `json:"complete"`
				Version  int64                           `json:"version"`
				Results  []struct{ Node, Status string } `json:"results"`
			} `json:"invalidation"`
		}
		if json.Unmarshal(data, &result) != nil || result.Committed == nil || !*result.Committed || result.Version != expected+1 || result.Invalidation.Version != result.Version || !result.Invalidation.Complete || len(result.Invalidation.Results) != 3 {
			out.Incorrect = true
		} else {
			seen := map[string]bool{}
			for _, ack := range result.Invalidation.Results {
				if ack.Status != "ok" || seen[ack.Node] {
					out.Incorrect = true
				}
				seen[ack.Node] = true
			}
		}
		out.Version = result.Version
		return out
	}
	var value Payload
	if json.Unmarshal(data, &value) != nil || value.Key != key || value.Version < 1 {
		out.Incorrect = true
		return out
	}
	out.Version = value.Version
	if out.Node == "" {
		out.Incorrect = true
	}
	if response.Header.Get("ETag") != `"v`+strconv.FormatInt(value.Version, 10)+`"` || value.Version > max {
		out.Incorrect = true
	}
	if value.Version < min {
		out.StaleAtStart = true
		out.Incorrect = true
	}
	var wanted []byte
	if value.Version == 1 {
		wanted = r.expected[keyIndex]
	} else {
		wanted = Value(key, value.Version, r.c.ValueBytes)
	}
	if !bytes.Equal(data, wanted) {
		out.Incorrect = true
	}
	return out
}
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
		r.expected[i] = Value(Key(c.Prefix, i), 1, c.ValueBytes)
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
