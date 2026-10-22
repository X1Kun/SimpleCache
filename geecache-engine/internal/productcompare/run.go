// Package productcompare runs bounded PostgreSQL-backed comparisons using fresh
// cache subprocesses. Database objects and child processes belong to one run.
package productcompare

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
)

type Options struct {
	Server       string `json:"server_binary"`
	Dir          string `json:"artifact_dir"`
	DatabaseURL  string `json:"-"`
	Repeats      int    `json:"repeats"`
	Workers      int    `json:"workers"`
	Keys         int    `json:"working_keys"`
	ValueBytes   int    `json:"value_bytes"`
	CacheBytes   int64  `json:"cache_bytes_per_node"`
	Passes       int    `json:"capacity_passes"`
	Seed         int64  `json:"seed"`
	Quick        bool   `json:"quick_smoke_only"`
	ZipfReads    int    `json:"zipf_reads"`
	MixedWindows int    `json:"mixed_windows"`
}

type Summary struct {
	Status          string           `json:"status"`
	Failure         string           `json:"failure,omitempty"`
	Options         Options          `json:"options"`
	ClientPID       int              `json:"client_pid"`
	ServerSHA256    string           `json:"server_binary_sha256"`
	DatabaseVersion string           `json:"postgres_version"`
	Schema          string           `json:"owned_schema"`
	SchemaDropped   bool             `json:"owned_schema_dropped"`
	WorkingSetBytes int64            `json:"logical_working_set_bytes"`
	OwnerBytes      map[string]int64 `json:"coordinated_owner_bytes"`
	Trials          []Trial          `json:"trials"`
}

type Trial struct {
	ID           string  `json:"id"`
	Mode         string  `json:"mode"`
	Scenario     string  `json:"scenario"`
	Repeat       int     `json:"repeat"`
	Status       string  `json:"status"`
	Failure      string  `json:"failure,omitempty"`
	PIDs         []int   `json:"initial_server_pids"`
	RestartedPID int     `json:"restarted_server_pid,omitempty"`
	WarmupReads  int     `json:"warmup_reads"`
	WorkingKeys  int     `json:"working_keys"`
	WorkingBytes int64   `json:"working_set_bytes"`
	Stages       []Stage `json:"stages"`
}

type Stage struct {
	Name        string             `json:"name"`
	Failure     string             `json:"failure,omitempty"`
	Traffic     Traffic            `json:"traffic"`
	Before      []Snapshot         `json:"before"`
	After       []Snapshot         `json:"after"`
	Delta       map[string]float64 `json:"delta,omitempty"`
	Mutations   []Mutation         `json:"mutations,omitempty"`
	GateWaiters int                `json:"postgres_lock_waiters,omitempty"`
	GateMS      float64            `json:"postgres_lock_duration_ms,omitempty"`
}

func (o Options) Validate() error {
	if o.Server == "" || o.Dir == "" || o.DatabaseURL == "" || o.Repeats < 1 || o.Repeats > 5 || (!o.Quick && o.Repeats < 3) || o.Workers < 3 || o.Workers > 32 || o.Keys < 8 || o.Keys > 4096 || o.ValueBytes < 512 || o.ValueBytes > 16384 || o.CacheBytes < 1<<20 || o.CacheBytes > 128<<20 || o.Passes < 1 || o.Passes > 5 {
		return errors.New("require server/dir/database URL, 3-5 repeats (quick permits 1), 3-32 workers, 8-4096 keys, 512-16384 byte values, 1-128MiB cache/node and 1-5 passes")
	}
	if o.ZipfReads < 192 || o.ZipfReads > 100000 || o.MixedWindows < 1 || o.MixedWindows > 200 {
		return errors.New("require 192-100000 Zipf reads and 1-200 mixed windows")
	}
	placement, total := owners(keysFor(o.Seed, o.Keys), o.ValueBytes)
	if total <= o.CacheBytes || total > 3*o.CacheBytes {
		return errors.New("working set must exceed one cache and fit within the total three-node capacity")
	}
	for _, id := range nodeIDs {
		if placement[id] == 0 || placement[id] > o.CacheBytes {
			return errors.New("actual hash placement must fit each owner and exercise all three owners")
		}
	}
	return nil
}

func databaseURL(raw, schema, application string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return "", errors.New("database must be a PostgreSQL URL")
	}
	params := parsed.Query()
	if schema != "" {
		params.Set("search_path", schema)
	}
	if application != "" {
		params.Set("application_name", application)
	}
	parsed.RawQuery = params.Encode()
	return parsed.String(), nil
}

func Run(ctx context.Context, o Options) (summary Summary, result error) {
	summary = Summary{Status: "failed", Options: o, ClientPID: os.Getpid()}
	if err := os.MkdirAll(o.Dir, 0755); err != nil {
		return summary, err
	}
	marker, err := os.OpenFile(filepath.Join(o.Dir, "run.lock"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return summary, errors.New("artifact directory has already been used or is unwritable")
	}
	_ = marker.Close()
	var database *pgx.Conn
	defer func() {
		if database != nil {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			if summary.Schema != "" {
				_, err := database.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{summary.Schema}.Sanitize()+" CASCADE")
				if err == nil {
					summary.SchemaDropped = true
				} else {
					result = errors.Join(result, errors.New("owned schema cleanup failed"))
				}
			}
			result = errors.Join(result, database.Close(cleanup))
			stop()
		}
		if result != nil {
			summary.Status = "failed"
			summary.Failure = result.Error()
		}
		result = errors.Join(result, writeReport(o.Dir, summary))
	}()
	if err := o.Validate(); err != nil {
		return summary, err
	}
	summary.OwnerBytes, summary.WorkingSetBytes = owners(keysFor(o.Seed, o.Keys), o.ValueBytes)
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	server, err := filepath.Abs(o.Server)
	if err != nil {
		return summary, err
	}
	o.Server = server
	summary.Options.Server = server
	binary, err := os.Open(o.Server)
	if err != nil {
		return summary, err
	}
	hash := sha256.New()
	_, hashErr := io.Copy(hash, binary)
	if err := errors.Join(hashErr, binary.Close()); err != nil {
		return summary, err
	}
	summary.ServerSHA256 = fmt.Sprintf("%x", hash.Sum(nil))
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return summary, err
	}
	runID := fmt.Sprintf("%x", random[:8])
	token := fmt.Sprintf("%x", random[8:])
	dsn, err := databaseURL(o.DatabaseURL, "", "sc-compare-"+runID)
	if err != nil {
		return summary, err
	}
	connect, cancel := context.WithTimeout(ctx, 5*time.Second)
	database, err = pgx.Connect(connect, dsn)
	cancel()
	if err != nil {
		database = nil
		return summary, errors.New("could not connect to comparison PostgreSQL database")
	}
	if err := database.QueryRow(ctx, "SHOW server_version").Scan(&summary.DatabaseVersion); err != nil {
		return summary, errors.New("could not read PostgreSQL version")
	}
	schema := "sc_compare_" + runID
	if _, err := database.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		return summary, errors.New("could not create owned comparison schema")
	}
	summary.Schema = schema
	table := pgx.Identifier{schema, "simplecache_products"}.Sanitize()
	if _, err := database.Exec(ctx, "CREATE TABLE "+table+" (key text PRIMARY KEY CHECK (octet_length(key) BETWEEN 1 AND 256), value bytea NOT NULL CHECK (octet_length(value)<=1048576), updated_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		return summary, errors.New("could not create comparison product table")
	}
	isolated, err := databaseURL(dsn, schema, "")
	if err != nil {
		return summary, err
	}
	keys := keysFor(o.Seed, o.Keys)
	summary.OwnerBytes, summary.WorkingSetBytes = owners(keys, o.ValueBytes)
	client := newClient(o.Workers)
	defer client.CloseIdleConnections()
	for repeat := 1; repeat <= o.Repeats; repeat++ {
		for _, scenario := range scenarios {
			modes := modesFor(scenario)
			if repeat%2 == 0 {
				for i, j := 0, len(modes)-1; i < j; i, j = i+1, j-1 {
					modes[i], modes[j] = modes[j], modes[i]
				}
			}
			scenarioKeys := keys
			if scenario == "mixed-updates" {
				scenarioKeys = keysFor(o.Seed, min(o.Keys*4, 4096))
			}
			for _, mode := range modes {
				if err := seed(ctx, database, table, scenarioKeys, o.ValueBytes); err != nil {
					return summary, err
				}
				trial, err := runTrial(ctx, o, client, database, isolated, schema, runID, token, mode, scenario, repeat, scenarioKeys)
				summary.Trials = append(summary.Trials, trial)
				fmt.Printf("%s: %s\n", trial.ID, trial.Status)
				if err != nil {
					return summary, fmt.Errorf("%s: %w", trial.ID, err)
				}
			}
		}
	}
	summary.Status = "passed"
	return summary, nil
}

func seed(ctx context.Context, database *pgx.Conn, table string, keys []string, valueBytes int) error {
	tx, err := database.Begin(ctx)
	if err != nil {
		return errors.New("could not begin dataset reset")
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "TRUNCATE "+table); err != nil {
		return errors.New("could not reset owned dataset")
	}
	batch := &pgx.Batch{}
	for _, key := range keys {
		batch.Queue("INSERT INTO "+table+" (key,value) VALUES ($1,$2)", key, productValue(key, 1, valueBytes))
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return errors.New("could not seed owned dataset")
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.New("could not commit owned dataset")
	}
	return nil
}

func runTrial(ctx context.Context, o Options, client *http.Client, database *pgx.Conn, dsn, schema, runID, token, mode, scenario string, repeat int, keys []string) (trial Trial, result error) {
	trial = Trial{ID: fmt.Sprintf("%02d-%s-%s", repeat, scenario, mode), Mode: mode, Scenario: scenario, Repeat: repeat, Status: "failed"}
	trial.WorkingKeys = len(keys)
	_, trial.WorkingBytes = owners(keys, o.ValueBytes)
	dir := filepath.Join(o.Dir, trial.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return trial, err
	}
	defer func() {
		if result != nil {
			trial.Failure = result.Error()
		} else {
			trial.Status = "passed"
		}
	}()
	work, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	application := fmt.Sprintf("sc-%s-%d-%s-%s", runID, repeat, scenario, mode)
	c, err := startCluster(work, o, mode, dir, dsn, token, application)
	if err != nil {
		return trial, err
	}
	defer func() { result = errors.Join(result, c.stop()) }()
	for _, n := range c.nodes {
		trial.PIDs = append(trial.PIDs, n.pid)
	}
	appendStage := func(stage Stage, err error) error {
		if err != nil {
			stage.Failure = err.Error()
		}
		trial.Stages = append(trial.Stages, stage)
		return err
	}
	switch scenario {
	case "zipf-read":
		warm := capacityTrace(keys, 1)
		if _, err := traffic(work, client, c.nodes, warm, o.ValueBytes, o.Workers, filepath.Join(dir, "warmup-requests.jsonl")); err != nil {
			return trial, err
		}
		trial.WarmupReads = len(warm)
		stage, err := runTrafficStage(work, client, c, "zipf", zipfTrace(keys, o.ZipfReads, o.Seed), o.Workers)
		return trial, appendStage(stage, err)
	case "mixed-updates":
		warm := zipfTrace(keys, 512, o.Seed)
		if _, err := traffic(work, client, c.nodes, warm, o.ValueBytes, o.Workers, filepath.Join(dir, "warmup-requests.jsonl")); err != nil {
			return trial, err
		}
		trial.WarmupReads = len(warm)
		stage, err := mixedStage(work, client, c, keys)
		if err := appendStage(stage, err); err != nil {
			return trial, err
		}
		stage, err = finalMixedStage(work, client, c, keys[0])
		return trial, appendStage(stage, err)
	case "cold-key":
		stage, err := coldStage(work, client, c, database, dsn, schema, keys[0])
		return trial, appendStage(stage, err)
	case "capacity":
		warm := capacityTrace(keys, 1)
		if _, err := traffic(work, client, c.nodes, warm, o.ValueBytes, o.Workers, filepath.Join(dir, "warmup-requests.jsonl")); err != nil {
			return trial, err
		}
		trial.WarmupReads = len(warm)
		stage, err := runTrafficStage(work, client, c, "steady", capacityTrace(keys, o.Passes), o.Workers)
		return trial, appendStage(stage, err)
	case "updates":
		warm := capacityTrace(keys[:4], 1)
		if _, err := traffic(work, client, c.nodes, warm, o.ValueBytes, o.Workers, filepath.Join(dir, "warmup-requests.jsonl")); err != nil {
			return trial, err
		}
		trial.WarmupReads = len(warm)
		stage, err := updateStage(work, client, c, keys[:4])
		return trial, appendStage(stage, err)
	case "node-failure":
		key := failureKey(keys)
		if key == "" {
			return trial, errors.New("dataset has no key owned by node-2")
		}
		if _, err := traffic(work, client, c.nodes, []Operation{{Node: 2, Key: key, Version: 1}}, o.ValueBytes, 1, filepath.Join(dir, "warmup-requests.jsonl")); err != nil {
			return trial, err
		}
		trial.WarmupReads = 1
		if err := c.nodes[2].stop(); err != nil {
			return trial, err
		}
		stage, err := runTrafficStage(work, client, c, "stopped-owner", burstTrace(key, 96, []int{0, 1}), o.Workers)
		if err := appendStage(stage, err); err != nil {
			return trial, err
		}
		if err := c.startNode(work, 2, "restarted"); err != nil {
			return trial, err
		}
		trial.RestartedPID = c.nodes[2].pid
		stage, err = recoveryStage(work, client, c, key)
		return trial, appendStage(stage, err)
	}
	return trial, errors.New("unknown scenario")
}

func coldStage(ctx context.Context, client *http.Client, c *cluster, database *pgx.Conn, dsn, schema, key string) (stage Stage, result error) {
	stage = Stage{Name: "locked-cold-burst"}
	stage.Before, result = collect(ctx, client, c.nodes, c.dir, "cold-before")
	if result != nil {
		return stage, result
	}
	observer, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return stage, errors.New("could not open lock observer")
	}
	defer observer.Close(context.Background())
	tx, err := database.Begin(ctx)
	if err != nil {
		return stage, errors.New("could not begin cold-key gate")
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "LOCK TABLE "+pgx.Identifier{schema, "simplecache_products"}.Sanitize()+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return stage, errors.New("could not acquire cold-key gate")
	}
	start := time.Now()
	type outcome struct {
		traffic Traffic
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		stats, err := traffic(ctx, client, c.nodes, burstTrace(key, 192, []int{0, 1, 2}), c.options.ValueBytes, c.options.Workers, filepath.Join(c.dir, "cold-requests.jsonl"))
		done <- outcome{stats, err}
	}()
	expected := 1
	if c.mode == "independent" {
		expected = 3
	}
	applications := []string{c.applicationPrefix + "-node-0", c.applicationPrefix + "-node-1", c.applicationPrefix + "-node-2"}
	gate, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	for {
		err := observer.QueryRow(gate, "SELECT count(*) FROM pg_stat_activity WHERE application_name = ANY($1::text[]) AND wait_event_type='Lock'", applications).Scan(&stage.GateWaiters)
		if err != nil {
			result = errors.New("cold-key lock observation failed or exceeded 500ms")
			break
		}
		if stage.GateWaiters == expected {
			break
		}
		select {
		case <-gate.Done():
			result = errors.New("expected PostgreSQL lock waiters not observed within 500ms")
		case <-time.After(5 * time.Millisecond):
		}
		if result != nil {
			break
		}
	}
	cancel()
	result = errors.Join(result, tx.Commit(ctx))
	stage.GateMS = float64(time.Since(start)) / float64(time.Millisecond)
	completed := <-done
	stage.Traffic = completed.traffic
	result = errors.Join(result, completed.err)
	stage.After, err = collect(ctx, client, c.nodes, c.dir, "cold-after")
	result = errors.Join(result, err)
	if err == nil {
		stage.Delta, err = metricDelta(stage.Before, stage.After)
		result = errors.Join(result, err)
	}
	if result == nil && stage.Delta["source"] != float64(expected) {
		result = errors.New("cold-key source loads did not match the observed loading processes")
	}
	return stage, result
}

func updateStage(ctx context.Context, client *http.Client, c *cluster, keys []string) (stage Stage, result error) {
	stage = Stage{Name: "post-update-reads"}
	stage.Before, result = collect(ctx, client, c.nodes, c.dir, "updates-before")
	if result != nil {
		return stage, result
	}
	var results []RequestResult
	var trace []Operation
	start := time.Now()
	for i, key := range keys {
		mutation, err := mutate(ctx, client, c.nodes[i%3], c.token, "PUT", key, productValue(key, 2, c.options.ValueBytes))
		stage.Mutations = append(stage.Mutations, mutation)
		if err != nil {
			result = err
			break
		}
		for node := 0; node < 3; node++ {
			op := Operation{Node: node, Key: key, Version: 2}
			trace = append(trace, op)
			r := read(ctx, client, c.nodes[node].api, op, productValue(key, 2, c.options.ValueBytes))
			r.Index = len(results)
			results = append(results, r)
		}
	}
	stage.Traffic = Traffic{Planned: len(keys) * 3, Attempted: len(results), Skipped: len(keys)*3 - len(results), Elapsed: time.Since(start).Seconds(), TraceSHA256: traceHash(trace)}
	var samples []float64
	for _, r := range results {
		samples = append(samples, r.Milliseconds)
		if r.Error != "" {
			stage.Traffic.Errors++
		}
		if r.Incorrect {
			stage.Traffic.Incorrect++
		}
	}
	stage.Traffic.Latency = latency(samples)
	result = errors.Join(result, writeJSONLines(filepath.Join(c.dir, "updates-requests.jsonl"), results))
	if stage.Traffic.Skipped > 0 || stage.Traffic.Errors > 0 || stage.Traffic.Incorrect > 0 {
		result = errors.Join(result, errors.New("post-update reads were incomplete or stale"))
	}
	var err error
	stage.After, err = collect(ctx, client, c.nodes, c.dir, "updates-after")
	result = errors.Join(result, err)
	if err == nil {
		stage.Delta, err = metricDelta(stage.Before, stage.After)
		result = errors.Join(result, err)
	}
	return stage, result
}

func recoveryStage(ctx context.Context, client *http.Client, c *cluster, key string) (stage Stage, result error) {
	stage = Stage{Name: "explicit-invalidation-recovery"}
	stage.Before, result = collect(ctx, client, c.nodes, c.dir, "recovery-before")
	if result != nil {
		return stage, result
	}
	for attempt := 0; attempt < 3; attempt++ {
		mutation, err := mutate(ctx, client, c.nodes[0], c.token, "POST", key, nil)
		stage.Mutations = append(stage.Mutations, mutation)
		result = err
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return stage, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if result != nil {
		return stage, result
	}
	var err error
	stage.Traffic, err = traffic(ctx, client, c.nodes, burstTrace(key, 96, []int{0, 1, 2}), c.options.ValueBytes, c.options.Workers, filepath.Join(c.dir, "recovery-requests.jsonl"))
	result = err
	stage.After, err = collect(ctx, client, c.nodes, c.dir, "recovery-after")
	result = errors.Join(result, err)
	if err == nil {
		stage.Delta, err = metricDelta(stage.Before, stage.After)
		result = errors.Join(result, err)
	}
	return stage, result
}
