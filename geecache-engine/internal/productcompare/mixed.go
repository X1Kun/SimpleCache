package productcompare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

func readMixed(ctx context.Context, client *http.Client, n *node, op Operation, size, maxSubmitted, committed int) RequestResult {
	result, body := readBody(ctx, client, n.api, op, size)
	result.CommittedAtStart = committed
	if result.Error != "" {
		return result
	}
	var value struct {
		Key     string `json:"key"`
		Version int    `json:"version"`
	}
	if json.Unmarshal(body, &value) != nil || value.Key != op.Key || value.Version < 1 || value.Version > maxSubmitted || (op.Version > 0 && value.Version != op.Version) {
		result.Incorrect = true
		return result
	}
	result.ObservedVersion = value.Version
	result.Incorrect = !bytes.Equal(body, productValue(op.Key, value.Version, size))
	return result
}

// Each window starts 19 reads and one write together, then drains before the
// next window. Versions are serialized on one key; old declared versions are
// measured as staleness rather than incorrectly treated as corrupted data.
func mixedStage(ctx context.Context, client *http.Client, c *cluster, keys []string) (stage Stage, result error) {
	stage = Stage{Name: "mixed-95-5"}
	plan := mixedTrace(keys, c.options.MixedWindows, c.options.Seed)
	if err := writeJSONLines(filepath.Join(c.dir, "mixed-plan.jsonl"), plan); err != nil {
		return stage, err
	}
	stage.Before, result = collect(ctx, client, c.nodes, c.dir, "mixed-before")
	if result != nil {
		return stage, result
	}
	var committed atomic.Int64
	committed.Store(1)
	var results []RequestResult
	start := time.Now()
	for _, window := range plan {
		if err := ctx.Err(); err != nil {
			result = err
			break
		}
		barrier := make(chan struct{})
		type writeResult struct {
			mutation Mutation
			err      error
		}
		written := make(chan writeResult, 1)
		go func() {
			<-barrier
			mutation, err := mutate(ctx, client, c.nodes[window.Write.Node], c.token, "PUT", window.Write.Key, productValue(window.Write.Key, window.Write.Version, c.options.ValueBytes), int64(window.Write.Version-1))
			if err == nil {
				committed.Store(int64(window.Write.Version))
			}
			written <- writeResult{mutation, err}
		}()
		reads := make([]RequestResult, len(window.Reads))
		var next atomic.Int64
		var wg sync.WaitGroup
		for worker := 0; worker < c.options.Workers-1; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-barrier
				for {
					i := int(next.Add(1) - 1)
					if i >= len(window.Reads) || ctx.Err() != nil {
						return
					}
					op := window.Reads[i]
					known := int(committed.Load())
					if op.Version > 0 {
						known = 1
					}
					reads[i] = readMixed(ctx, client, c.nodes[op.Node], op, c.options.ValueBytes, window.Write.Version, known)
				}
			}()
		}
		close(barrier)
		wg.Wait()
		write := <-written
		stage.Mutations = append(stage.Mutations, write.mutation)
		for i := range reads {
			reads[i].Index = len(results)
			results = append(results, reads[i])
		}
		if write.err != nil {
			result = write.err
			break
		}
	}
	stage.Traffic = Traffic{Planned: len(plan) * 19, Elapsed: time.Since(start).Seconds(), TraceSHA256: valueHash(plan)}
	var samples []float64
	for _, r := range results {
		if !r.Attempted {
			continue
		}
		stage.Traffic.Attempted++
		samples = append(samples, r.Milliseconds)
		if r.Error != "" {
			stage.Traffic.Errors++
		}
		if r.Incorrect {
			stage.Traffic.Incorrect++
		}
		if r.ObservedVersion > 0 && r.ObservedVersion < r.CommittedAtStart {
			stage.Traffic.StaleAtStart++
			stage.Traffic.MaxVersionLag = max(stage.Traffic.MaxVersionLag, r.CommittedAtStart-r.ObservedVersion)
		}
	}
	stage.Traffic.Skipped = stage.Traffic.Planned - stage.Traffic.Attempted
	stage.Traffic.Latency = latency(samples)
	result = errors.Join(result, writeJSONLines(filepath.Join(c.dir, "mixed-requests.jsonl"), results))
	if stage.Traffic.Errors > 0 || stage.Traffic.Incorrect > 0 || stage.Traffic.Skipped > 0 {
		result = errors.Join(result, errors.New("mixed traffic had errors, corrupt values or skipped reads"))
	}
	var err error
	stage.After, err = collect(ctx, client, c.nodes, c.dir, "mixed-after")
	result = errors.Join(result, err)
	if err == nil {
		stage.Delta, err = metricDelta(stage.Before, stage.After)
		result = errors.Join(result, err)
	}
	return stage, result
}

func finalMixedStage(ctx context.Context, client *http.Client, c *cluster, key string) (stage Stage, result error) {
	stage = Stage{Name: "mixed-final-after-invalidation"}
	stage.Before, result = collect(ctx, client, c.nodes, c.dir, "mixed-final-before")
	if result != nil {
		return stage, result
	}
	mutation, err := mutate(ctx, client, c.nodes[0], c.token, "POST", key, nil, 0)
	stage.Mutations = append(stage.Mutations, mutation)
	if err != nil {
		return stage, err
	}
	trace := []Operation{}
	for node := 0; node < 3; node++ {
		trace = append(trace, Operation{Node: node, Key: key, Version: c.options.MixedWindows + 1})
	}
	stage.Traffic, result = traffic(ctx, client, c.nodes, trace, c.options.ValueBytes, 1, filepath.Join(c.dir, "mixed-final-requests.jsonl"))
	stage.After, err = collect(ctx, client, c.nodes, c.dir, "mixed-final-after")
	result = errors.Join(result, err)
	if err == nil {
		stage.Delta, err = metricDelta(stage.Before, stage.After)
		result = errors.Join(result, err)
	}
	return stage, result
}
