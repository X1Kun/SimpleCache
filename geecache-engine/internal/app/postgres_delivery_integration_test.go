package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/outbox"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/source/postgres"
	"github.com/jackc/pgx/v5"
)

func deliveryPostgresFixture(t *testing.T) (context.Context, *postgres.Store, *pgx.Conn, string, string) {
	t.Helper()
	dsn := os.Getenv("SIMPLECACHE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SIMPLECACHE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	audit, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("dedicated database connection failed")
	}
	var unique [12]byte
	if _, err := rand.Read(unique[:]); err != nil {
		t.Fatal(err)
	}
	key := "delivery-" + hex.EncodeToString(unique[:])
	schema := "delivery_" + hex.EncodeToString(unique[:])
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_, err := audit.Exec(cleanup, "DROP SCHEMA IF EXISTS "+quoted+" CASCADE")
		if err != nil {
			t.Error(err)
		}
		_ = audit.Close(cleanup)
	})
	if _, err := audit.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.Exec(ctx, "SET search_path TO "+quoted); err != nil {
		t.Fatal(err)
	}
	sql, err := os.ReadFile("../../../deploy/products/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audit.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	dsn = parsed.String()
	if _, err := audit.Exec(ctx, "INSERT INTO simplecache_products(key,value,version,deleted) VALUES ($1,$2,10,false)", key, responseV10); err != nil {
		t.Fatal(err)
	}
	store, err := postgres.Open(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return ctx, store, audit, key, dsn
}

func requireDatabaseVersion(t *testing.T, ctx context.Context, audit *pgx.Conn, key string, version int64, payload []byte) {
	t.Helper()
	var actual int64
	var value []byte
	var deleted bool
	if err := audit.QueryRow(ctx, "SELECT version,value,deleted FROM simplecache_products WHERE key=$1", key).Scan(&actual, &value, &deleted); err != nil {
		t.Fatal(err)
	}
	if actual != version || string(value) != string(payload) || deleted {
		t.Fatalf("unexpected authoritative row: version=%d value=%s deleted=%v", actual, value, deleted)
	}
}

func TestPostgresResponseLossRetry(t *testing.T) {
	ctx, store, audit, key, _ := deliveryPostgresFixture(t)
	checkLostWriteResponseRetry(t, store, key, func() { requireDatabaseVersion(t, ctx, audit, key, 11, responseV11) })
}

type writerEvent struct {
	Phase   string `json:"phase"`
	Key     string `json:"key"`
	Version int64  `json:"version"`
}
type commitBarrierProducts struct {
	*postgres.Store
	events *os.File
}

func (s *commitBarrierProducts) PutIfVersion(ctx context.Context, key string, value []byte, expected int64) (int64, error) {
	version, err := s.Store.PutIfVersion(ctx, key, value, expected)
	if err != nil {
		return version, err
	}
	// SQL RETURNING has acknowledged the committed generation. Do not return to
	// executeMutation: neither local invalidation nor fan-out can begin yet.
	if err := json.NewEncoder(s.events).Encode(writerEvent{"committed", key, version}); err != nil {
		return 0, err
	}
	<-make(chan struct{}) // Only SIGKILL releases this test-only crash barrier.
	return version, nil
}

// A subprocess-only fixture: the normal test suite must never execute it.
func TestCommitCrashWriterProcess(t *testing.T) {
	if os.Getenv("SIMPLECACHE_CONTRACT_WRITER") != "1" {
		t.Skip("subprocess-only crash fixture")
	}
	events := os.NewFile(3, "writer-events")
	control := os.NewFile(4, "writer-control")
	defer events.Close()
	defer control.Close()
	startup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := postgres.Open(startup, os.Getenv("SIMPLECACHE_TEST_DATABASE_URL"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	source := &commitBarrierProducts{Store: store, events: events}
	router := peer.NewDirectory("writer", time.Second, "crash-test-token")
	defer router.Close()
	if err := router.Update([]peer.Member{{ID: "survivor", Address: os.Getenv("SIMPLECACHE_CONTRACT_PEER")}}); err != nil {
		t.Fatal(err)
	}
	group := cache.NewGroup("products", 1<<20, source, cache.Options{})
	handler := APIHandler(group, func() bool { return true }, nil, APIOptions{Store: source, Router: router, Token: "crash-test-token", DirectReads: true})
	key := os.Getenv("SIMPLECACHE_CONTRACT_KEY")
	if err := json.NewEncoder(events).Encode(writerEvent{Phase: "ready", Key: key}); err != nil {
		t.Fatal(err)
	}
	var command [1]byte
	if _, err := io.ReadFull(control, command[:]); err != nil {
		t.Fatal(err)
	}
	response := productRequest(handler, http.MethodPut, contractPath(key), string(responseV11), "crash-test-token", `"v10"`)
	t.Fatalf("writer escaped commit barrier before forced crash: status=%d", response.Code)
}

func TestPostgresCommitBeforeBroadcastCrash(t *testing.T) { testCommitBeforeBroadcastCrash(t, false) }
func TestPostgresOutboxCrashRecovery(t *testing.T)        { testCommitBeforeBroadcastCrash(t, true) }
func testCommitBeforeBroadcastCrash(t *testing.T, replay bool) {
	if runtime.GOOS == "windows" {
		t.Skip("crash fixture requires inherited Unix file descriptors")
	}
	ctx, store, audit, key, fixtureDSN := deliveryPostgresFixture(t)
	counted := &countingProducts{MutableSource: store}
	ttl := 5 * time.Second
	if replay {
		ttl = time.Hour
	}
	survivor := cache.NewGroup("products", 1<<20, counted, cache.Options{TTL: ttl})
	var notifications atomic.Int64
	peerHandler := peer.NewHandlerWithToken("crash-test-token", survivor)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			notifications.Add(1)
		}
		peerHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	readEvents, writeEvents, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readEvents.Close(); _ = writeEvents.Close() })
	readControl, writeControl, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readControl.Close(); _ = writeControl.Close() })
	childLog := filepath.Join(t.TempDir(), "writer.log")
	output, err := os.Create(childLog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	child := exec.Command(os.Args[0], "-test.run=^TestCommitCrashWriterProcess$", "-test.timeout=30s")
	child.Env = append(os.Environ(), "SIMPLECACHE_CONTRACT_WRITER=1", "SIMPLECACHE_TEST_DATABASE_URL="+fixtureDSN, "SIMPLECACHE_CONTRACT_KEY="+key, "SIMPLECACHE_CONTRACT_PEER="+strings.TrimPrefix(server.URL, "http://"))
	child.ExtraFiles = []*os.File{writeEvents, readControl}
	child.Stdout = output
	child.Stderr = output
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	_ = writeEvents.Close()
	_ = readControl.Close()
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = child.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = child.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("writer process did not stop")
		}
	})
	events := make(chan writerEvent, 2)
	decodeErrors := make(chan error, 1)
	go func() {
		decoder := json.NewDecoder(readEvents)
		for {
			var event writerEvent
			if err := decoder.Decode(&event); err != nil {
				decodeErrors <- err
				return
			}
			events <- event
		}
	}()
	await := func(phase string) writerEvent {
		t.Helper()
		select {
		case event := <-events:
			if event.Phase != phase || event.Key != key {
				t.Fatal("unexpected writer barrier", event)
			}
			return event
		case err := <-decodeErrors:
			log, _ := os.ReadFile(childLog)
			t.Fatalf("writer event failed: %v\n%s", err, log)
		case <-done:
			log, _ := os.ReadFile(childLog)
			t.Fatalf("writer exited before %s: %v\n%s", phase, waitErr, log)
		case <-ctx.Done():
			t.Fatal("writer barrier timed out", phase, ctx.Err())
		}
		return writerEvent{}
	}
	// Finish child initialization first so the TTL window measures the crash,
	// not process startup. No mutation is allowed before this parent command.
	await("ready")
	cached, err := survivor.GetDirect(ctx, key)
	if err != nil || cached.Version() != 10 || cached.String() != string(responseV10) {
		t.Fatal(cached, err)
	}
	if _, err := writeControl.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	event := await("committed")
	if event.Version != 11 {
		t.Fatal(event)
	}
	requireDatabaseVersion(t, ctx, audit, key, 11, responseV11)
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("writer did not exit", ctx.Err())
	}
	status, ok := child.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("writer was not forcibly killed", waitErr, child.ProcessState)
	}
	if notifications.Load() != 0 {
		t.Fatal("broadcast occurred before the crash barrier", notifications.Load())
	}
	old, err := survivor.GetDirect(ctx, key)
	if err != nil || old.Version() != 10 || old.String() != string(responseV10) || counted.reads.Load() != 1 {
		t.Fatal("missed event did not preserve the warm stale copy", old, err, counted.reads.Load())
	}
	if replay {
		relayContext, stopRelay := context.WithCancel(ctx)
		relayDone := make(chan struct{})
		var relayErr error
		config := Config{PeerAddr: "127.0.0.1:0", SelfID: "relay", DiscoveryMode: "static", SourceMode: "postgres", AdminToken: "crash-test-token", CacheBytes: 1 << 20, SourceConcurrency: 1, Members: []peer.Member{{ID: "survivor", Address: strings.TrimPrefix(server.URL, "http://")}}}
		go func() { defer close(relayDone); relayErr = RunWithSource(relayContext, config, store) }()
		defer func() {
			stopRelay()
			select {
			case <-relayDone:
				if relayErr != nil {
					t.Error(relayErr)
				}
			case <-time.After(5 * time.Second):
				t.Error("relay runtime did not join on shutdown")
			}
		}()
		// Delivery is asynchronous; bounded polling observes durable completion.
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			stats, err := store.Stats(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if stats.Pending == 0 {
				break
			}
			select {
			case <-ticker.C:
			case <-relayDone:
				t.Fatal("relay exited before delivery", relayErr)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		fresh, err := survivor.GetDirect(ctx, key)
		if err != nil || fresh.Version() != 11 || fresh.String() != string(responseV11) || notifications.Load() < 1 || cached.RemainingTTL() <= 0 {
			t.Fatal("durable replay failed before TTL", fresh, err)
		}
		requireDatabaseVersion(t, ctx, audit, key, 11, responseV11)
		t.Log("new application runtime replayed durable event from killed writer before original TTL expired")
		return
	}

	remaining := old.RemainingTTL()
	if remaining <= 0 {
		t.Fatal("fixture TTL window expired before stale observation")
	}
	// Wait for this exact entry's inherited deadline, not an arbitrary sleep or
	// a commit-relative freshness SLA. Lookup performs expiry without a cleaner.
	expiry := time.NewTimer(remaining)
	defer expiry.Stop()
	select {
	case <-expiry.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	fresh, err := survivor.GetDirect(ctx, key)
	if err != nil || fresh.Version() != 11 || fresh.String() != string(responseV11) || counted.reads.Load() != 2 {
		t.Fatal("expiry did not reload authoritative value", fresh, err, counted.reads.Load())
	}
	if notifications.Load() != 0 {
		t.Fatal("recovery used a notification rather than expiry")
	}
	requireDatabaseVersion(t, ctx, audit, key, 11, responseV11)
	t.Log("writer killed after SQL commit and before broadcast; unchanged survivor served v10, then TTL/source reload recovered v11")
}

// Drop the acknowledgement after successful network delivery, then let a new
// worker reclaim the expired lease. This models the send-before-ack crash gap.
type loseOutboxAck struct{ outbox.Queue }

func (q loseOutboxAck) Finish(context.Context, outbox.Event, bool, time.Duration) (bool, error) {
	return false, errors.New("simulated sender death before durable acknowledgement")
}

func TestPostgresOutboxDuplicateDelivery(t *testing.T) {
	ctx, store, audit, key, _ := deliveryPostgresFixture(t)
	if version, err := store.PutIfVersion(ctx, key, responseV11, 10); err != nil || version != 11 {
		t.Fatal(version, err)
	}
	counted := &countingProducts{MutableSource: store}
	receiver := cache.NewGroup("products", 1<<20, counted, cache.Options{TTL: time.Hour})
	// A newer version is already cached when the old event is redelivered.
	newer := []byte(`{"value":"newer"}`)
	if version, err := store.PutIfVersion(ctx, key, newer, 11); err != nil || version != 12 {
		t.Fatal(version, err)
	}
	if _, err := receiver.GetDirect(ctx, key); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(peer.NewHandlerWithToken("relay-token", receiver))
	defer server.Close()
	router := peer.NewDirectory("sender", time.Second, "relay-token")
	defer router.Close()
	if err := router.Update([]peer.Member{{ID: "receiver", Address: strings.TrimPrefix(server.URL, "http://")}}); err != nil {
		t.Fatal(err)
	}
	local := cache.NewGroup("products", 1024, store, cache.Options{})
	sends := 0
	send := func(ctx context.Context, e outbox.Event) error {
		sends++
		if e.Version != 11 {
			t.Fatal("fixture claimed wrong event", e)
		}
		report, err := router.InvalidateVersion(ctx, local, e.Key, e.Version)
		if err == nil && !report.Complete {
			return errors.New("partial")
		}
		return err
	}
	first := outbox.Worker{Queue: loseOutboxAck{store}, Send: send}
	if worked, err := first.Step(ctx); err == nil || !worked {
		t.Fatal("ack loss was not exercised", worked, err)
	}
	if _, err := audit.Exec(ctx, "UPDATE simplecache_outbox SET leased_until=clock_timestamp()-interval '1 second' WHERE version=11"); err != nil {
		t.Fatal(err)
	}
	restarted := outbox.Worker{Queue: store, Send: send}
	if worked, err := restarted.Step(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	value, err := receiver.GetDirect(ctx, key)
	if err != nil || value.Version() != 12 || value.String() != string(newer) || counted.reads.Load() != 1 || sends != 2 {
		t.Fatal("duplicate/old event evicted newer cache", value, err, counted.reads.Load(), sends)
	}
	var attempts int64
	var complete bool
	if err := audit.QueryRow(ctx, "SELECT attempts,delivered_at IS NOT NULL FROM simplecache_outbox WHERE version=11").Scan(&attempts, &complete); err != nil || attempts != 2 || !complete {
		t.Fatal(attempts, complete, err)
	}
}
