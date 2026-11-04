package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/app"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/source/postgres"
	"github.com/jackc/pgx/v5"
)

// This is the production application assembly (including relay), one node and
// real PostgreSQL. It qualifies the experiment path, not Gateway capacity.
func TestArrivalRuntimeIntegration(t *testing.T) {
	dsn := os.Getenv("SIMPLECACHE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SIMPLECACHE_TEST_DATABASE_URL is not set")
	}
	ctx, stop := context.WithTimeout(context.Background(), 90*time.Second)
	defer stop()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := fmt.Sprintf("arrival_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	if _, err = admin.Exec(ctx, "SET search_path TO "+quoted); err != nil {
		t.Fatal(err)
	}
	sql, err := os.ReadFile("../../../../deploy/products/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	store, err := postgres.Open(ctx, parsed.String(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reserve := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := l.Addr().String()
		l.Close()
		return address
	}
	peerAddr, apiAddr := reserve(), reserve()
	lifecycle, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- app.RunWithSource(lifecycle, app.Config{PeerAddr: peerAddr, APIAddr: apiAddr, SelfID: "arrival", DiscoveryMode: "static", API: true, CacheBytes: 1 << 20, TTL: time.Minute, SourceConcurrency: 2, SourceMaxPending: 2, MaxMissWaiters: 16, Members: []peer.Member{{ID: "arrival", Address: peerAddr}}, SourceMode: "postgres", AdminToken: "test-only-token", ReadRoutingMode: "gateway"}, store)
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(8 * time.Second):
			t.Error("runtime shutdown timed out")
		}
	}()
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	ready := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		r, err := client.Get("http://" + apiAddr + "/readyz")
		if err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("runtime did not start")
	}
	report := os.Getenv("SIMPLECACHE_ARRIVAL_REPORT_DIR")
	if report == "" {
		report = t.TempDir()
	} else {
		report = filepath.Join(report, schema)
	}
	if err := os.MkdirAll(report, 0755); err != nil {
		t.Fatal(err)
	}
	var databaseVersion string
	if err := admin.QueryRow(ctx, "SHOW server_version").Scan(&databaseVersion); err != nil {
		t.Fatal(err)
	}
	metadata := map[string]any{"postgres_version": databaseVersion, "scope": "single-node production assembly; client/app/test runner share process; not Gateway capacity or server-only resource measurement", "cache_bytes": 1 << 20, "source_concurrency": 2, "source_pending": 2, "relay_enabled": true, "fixture_schema": schema, "behaviours_use_fresh_prefixes": true, "runtime_reused_between_behaviours": true}
	encoded, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(report, "environment.json"), encoded, 0644); err != nil {
		t.Fatal(err)
	}
	t.Log("Experiment report:", report)
	for _, distribution := range []string{"hot", "distinct", "scan", "hot-scan", "hit-miss"} {
		c := arrivalConfig("http://" + apiAddr)
		c.Prefix = "experiment-" + distribution
		c.Distribution = distribution
		c.Keys = 1024
		c.ValueBytes = 4096
		c.Workers = 8
		c.MaxPending = 16
		c.Stages = []RateStage{{Name: "baseline", Rate: 30, Seconds: 1}, {Name: "pressure", Rate: 120, Seconds: 1}, {Name: "recovery", Rate: 30, Seconds: 1}}
		var fixture strings.Builder
		if err := WriteFixture(&fixture, c); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, fixture.String()); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(report, distribution)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		sampleCtx, stopSample := context.WithCancel(ctx)
		sampleDone := make(chan error, 1)
		go func() {
			sampleDone <- sampleArrivalMetrics(sampleCtx, client, "http://"+apiAddr, filepath.Join(dir, "metrics.promlog"))
		}()
		summary, runErr := Run(ctx, c, dir)
		stopSample()
		sampleErr := <-sampleDone
		if sampleErr != nil {
			t.Fatal(sampleErr)
		}
		if summary.Arrival == nil || summary.Incorrect != 0 || summary.Arrival.Status == "failed" || (runErr != nil && summary.Arrival.Status == "passed") {
			t.Fatalf("%s: summary=%+v error=%v", distribution, summary, runErr)
		}
		// Client limits and HTTP errors remain reported; this is a functional path
		// check and must not silently turn a noisy run into a clean baseline.
		t.Logf("%s: status=%s attempted=%d dropped=%d errors=%d", distribution, summary.Arrival.Status, summary.Attempted, summary.Skipped, summary.Errors)
	}
}
func sampleArrivalMetrics(ctx context.Context, client *http.Client, base, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	start := time.Now()
	for {
		req, err := http.NewRequestWithContext(ctx, "GET", base+"/metrics", nil)
		if err != nil {
			return err
		}
		response, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		_, err = fmt.Fprintf(f, "# sample_elapsed_seconds %.6f\n", time.Since(start).Seconds())
		if err == nil {
			_, err = io.Copy(f, response.Body)
		}
		response.Body.Close()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
