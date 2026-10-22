package productcompare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer"
)

type Latency struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50_ms"`
	P95   float64 `json:"p95_ms"`
	P99   float64 `json:"p99_ms"`
}

type RequestResult struct {
	Index            int       `json:"index"`
	Operation        Operation `json:"operation"`
	Attempted        bool      `json:"attempted"`
	Status           int       `json:"status"`
	Milliseconds     float64   `json:"elapsed_ms"`
	Incorrect        bool      `json:"incorrect"`
	Error            string    `json:"error,omitempty"`
	ObservedVersion  int       `json:"observed_version,omitempty"`
	CommittedAtStart int       `json:"committed_at_start,omitempty"`
}

type Traffic struct {
	Planned       int     `json:"planned"`
	Attempted     int     `json:"attempted"`
	Skipped       int     `json:"skipped"`
	Errors        int     `json:"errors"`
	Incorrect     int     `json:"incorrect_values"`
	Elapsed       float64 `json:"elapsed_seconds"`
	Latency       Latency `json:"latency_all_attempts"`
	TraceSHA256   string  `json:"trace_sha256"`
	StaleAtStart  int     `json:"stale_at_start"`
	MaxVersionLag int     `json:"max_version_lag"`
}

func newClient(workers int) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConns = workers*3 + 3
	transport.MaxIdleConnsPerHost = workers + 3
	return &http.Client{Transport: transport, Timeout: 4 * time.Second}
}

func latency(samples []float64) Latency {
	if len(samples) == 0 {
		return Latency{}
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	percentile := func(p float64) float64 { return sorted[int(math.Ceil(p*float64(len(sorted))))-1] }
	return Latency{len(sorted), percentile(.5), percentile(.95), percentile(.99)}
}

func read(ctx context.Context, client *http.Client, origin string, op Operation, expected []byte) RequestResult {
	result, body := readBody(ctx, client, origin, op, len(expected))
	if result.Error == "" {
		result.Incorrect = !bytes.Equal(body, expected)
	}
	return result
}

func readBody(ctx context.Context, client *http.Client, origin string, op Operation, limit int) (RequestResult, []byte) {
	result := RequestResult{Operation: op, Attempted: true}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, "GET", origin+"/api?key="+url.QueryEscape(op.Key), nil)
	if err != nil {
		result.Error = "request construction failed"
		result.Milliseconds = float64(time.Since(start)) / float64(time.Millisecond)
		return result, nil
	}
	response, err := client.Do(req)
	if err != nil {
		result.Error = "request transport failed"
		result.Milliseconds = float64(time.Since(start)) / float64(time.Millisecond)
		return result, nil
	}
	result.Status = response.StatusCode
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(limit+1)))
	_ = response.Body.Close()
	result.Milliseconds = float64(time.Since(start)) / float64(time.Millisecond)
	if err != nil {
		result.Error = "response body failed"
	} else if response.StatusCode != 200 {
		result.Error = "non-200 read response"
	}
	return result, body
}

func traffic(ctx context.Context, client *http.Client, nodes []*node, trace []Operation, valueBytes, workers int, path string) (Traffic, error) {
	stats := Traffic{Planned: len(trace), TraceSHA256: traceHash(trace)}
	expected := map[string][]byte{}
	for _, op := range trace {
		key := fmt.Sprintf("%s:%d", op.Key, op.Version)
		if _, exists := expected[key]; !exists {
			expected[key] = productValue(op.Key, op.Version, valueBytes)
		}
	}
	results := make([]RequestResult, len(trace))
	var next atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(trace) || ctx.Err() != nil {
					return
				}
				op := trace[i]
				results[i] = read(ctx, client, nodes[op.Node].api, op, expected[fmt.Sprintf("%s:%d", op.Key, op.Version)])
				results[i].Index = i
			}
		}()
	}
	wg.Wait()
	stats.Elapsed = time.Since(start).Seconds()
	var samples []float64
	for i, result := range results {
		if !result.Attempted {
			results[i] = RequestResult{Index: i, Operation: trace[i]}
			stats.Skipped++
			continue
		}
		stats.Attempted++
		if result.Error != "" {
			stats.Errors++
		}
		if result.Incorrect {
			stats.Incorrect++
		}
		samples = append(samples, result.Milliseconds)
	}
	stats.Latency = latency(samples)
	if path != "" {
		if err := writeJSONLines(path, results); err != nil {
			return stats, err
		}
	}
	if stats.Attempted == 0 || stats.Skipped > 0 || stats.Errors > 0 || stats.Incorrect > 0 {
		return stats, errors.New("traffic did not complete with correct values and HTTP statuses")
	}
	return stats, ctx.Err()
}

func writeJSONLines[T any](path string, values []T) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	for _, value := range values {
		if err := encoder.Encode(value); err != nil {
			_ = file.Close()
			return err
		}
	}
	return file.Close()
}

type Mutation struct {
	Method       string                   `json:"method"`
	Key          string                   `json:"key"`
	Status       int                      `json:"status"`
	Milliseconds float64                  `json:"elapsed_ms"`
	Committed    *bool                    `json:"source_committed"`
	Invalidation *peer.InvalidationReport `json:"invalidation"`
}

func mutate(ctx context.Context, client *http.Client, n *node, token, method, key string, value []byte) (Mutation, error) {
	result := Mutation{Method: method, Key: key}
	path := "/api?key="
	if method == "POST" {
		path = "/invalidate?key="
	}
	req, err := http.NewRequestWithContext(ctx, method, n.api+path+url.QueryEscape(key), bytes.NewReader(value))
	if err != nil {
		return result, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	start := time.Now()
	response, err := client.Do(req)
	result.Milliseconds = float64(time.Since(start)) / float64(time.Millisecond)
	if err != nil {
		return result, errors.New("mutation transport failed")
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()
	result.Status, result.Milliseconds = response.StatusCode, float64(time.Since(start))/float64(time.Millisecond)
	var decoded struct {
		Committed    *bool                    `json:"source_committed"`
		Invalidation *peer.InvalidationReport `json:"invalidation"`
	}
	if readErr != nil || json.Unmarshal(body, &decoded) != nil {
		return result, errors.New("invalid mutation response")
	}
	result.Committed, result.Invalidation = decoded.Committed, decoded.Invalidation
	if method == "PUT" && (decoded.Committed == nil || !*decoded.Committed) {
		return result, errors.New("source write did not confirm commit")
	}
	if result.Status != 200 || decoded.Invalidation == nil || !decoded.Invalidation.Complete {
		return result, errors.New("mutation did not acknowledge complete invalidation")
	}
	seen := map[string]bool{}
	for _, ack := range decoded.Invalidation.Results {
		if ack.Status != "ok" || seen[ack.Node] {
			return result, errors.New("invalid node acknowledgement")
		}
		seen[ack.Node] = true
	}
	for _, id := range nodeIDs {
		if !seen[id] {
			return result, errors.New("mutation did not notify every known node")
		}
	}
	if len(seen) != 3 {
		return result, errors.New("unexpected mutation membership")
	}
	return result, nil
}

func runTrafficStage(ctx context.Context, client *http.Client, c *cluster, name string, trace []Operation, workers int) (stage Stage, result error) {
	stage = Stage{Name: name}
	stage.Before, result = collect(ctx, client, c.nodes, c.dir, name+"-before")
	if result != nil {
		return stage, result
	}
	stage.Traffic, result = traffic(ctx, client, c.nodes, trace, c.options.ValueBytes, workers, filepath.Join(c.dir, name+"-requests.jsonl"))
	var metricsErr error
	stage.After, metricsErr = collect(ctx, client, c.nodes, c.dir, name+"-after")
	result = errors.Join(result, metricsErr)
	if metricsErr == nil {
		stage.Delta, metricsErr = metricDelta(stage.Before, stage.After)
		result = errors.Join(result, metricsErr)
	}
	return stage, result
}
