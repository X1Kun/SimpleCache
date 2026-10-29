package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/telemetry"
)

type heldWriteSource struct {
	*memoryProducts
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *heldWriteSource) PutIfVersion(ctx context.Context, key string, value []byte, expected int64) (int64, error) {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
		return s.memoryProducts.PutIfVersion(ctx, key, value, expected)
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

type metricReading struct {
	count      uint64
	sum, value float64
}

func readMetric(t *testing.T, m *telemetry.Metrics, name, label string) (out metricReading) {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			matched := label == ""
			for _, pair := range metric.Label {
				if pair.GetValue() == label {
					matched = true
				}
			}
			if !matched {
				continue
			}
			if h := metric.Histogram; h != nil {
				out.count += h.GetSampleCount()
				out.sum += h.GetSampleSum()
			}
			if c := metric.Counter; c != nil {
				out.value += c.GetValue()
			}
			if g := metric.Gauge; g != nil {
				out.value += g.GetValue()
			}
		}
	}
	return
}

func TestWriteAndBroadcastTimingsDescribeSeparatePhases(t *testing.T) {
	store := &heldWriteSource{memoryProducts: &memoryProducts{values: map[string][]byte{}}, entered: make(chan struct{}), release: make(chan struct{}, 1)}
	defer close(store.release)
	group := cache.NewGroup("products", 1024, store, cache.Options{})
	m := telemetry.New()
	router := peer.NewRouterWithToken("local", time.Second, "test-token")
	defer router.Close()
	broadcast, releasePeer := make(chan struct{}), make(chan struct{}, 1)
	defer close(releasePeer)
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(broadcast) })
		select {
		case <-releasePeer:
		case <-r.Context().Done():
			return
		}
		w.Header().Set(peer.InvalidationVersionHeader, r.Header.Get(peer.InvalidationVersionHeader))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if err := router.Update([]peer.Member{{ID: "slow", Address: strings.TrimPrefix(server.URL, "http://")}}); err != nil {
		t.Fatal(err)
	}
	handler := APIHandler(group, func() bool { return true }, m, APIOptions{Store: store, Router: router, Token: "test-token"})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- productRequest(handler, "PUT", "/api?key=key", `{"v":1}`, "test-token", "*") }()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("write did not start")
	}
	if x := readMetric(t, m, "simplecache_write_requests_inflight", ""); x.value != 1 {
		t.Fatal("admitted write not accounted", x)
	}
	if x := readMetric(t, m, "simplecache_source_write_duration_seconds", "ok"); x.count != 0 {
		t.Fatal("unfinished source phase was reported", x)
	}
	store.release <- struct{}{}
	select {
	case <-broadcast:
	case <-time.After(time.Second):
		t.Fatal("broadcast did not start")
	}
	write := readMetric(t, m, "simplecache_source_write_duration_seconds", "ok")
	if write.count != 1 || write.sum <= 0 {
		t.Fatal("source duration was missing or zero", write)
	}
	if x := readMetric(t, m, "simplecache_invalidation_broadcast_duration_seconds", ""); x.count != 0 {
		t.Fatal("unfinished broadcast reported", x)
	}
	releasePeer <- struct{}{}
	select {
	case w := <-done:
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("write did not finish")
	}
	if x := readMetric(t, m, "simplecache_invalidation_broadcast_duration_seconds", "complete"); x.count != 1 || x.sum <= 0 {
		t.Fatal("broadcast duration missing", x)
	}
	if readMetric(t, m, "simplecache_source_unknown_commits_total", "").value != 0 || readMetric(t, m, "simplecache_write_requests_inflight", "").value != 0 {
		t.Fatal("success reported unknown commit or leaked write capacity")
	}
}

type failOnRead struct{ read bool }

func (r *failOnRead) Read([]byte) (int, error) {
	r.read = true
	return 0, errors.New("body must not be read")
}

func TestWriteAdmissionRejectsBeforeBodyAndKeepsHitsAndProbes(t *testing.T) {
	store := &heldWriteSource{memoryProducts: &memoryProducts{values: map[string][]byte{"warm": []byte(`{}`)}}, entered: make(chan struct{}), release: make(chan struct{}, 1)}
	defer close(store.release)
	group := cache.NewGroup("products", 1024, store, cache.Options{})
	if _, err := group.GetLocal(context.Background(), "warm"); err != nil {
		t.Fatal(err)
	}
	m := telemetry.New()
	router := peer.NewRouterWithToken("local", time.Second, "test-token")
	defer router.Close()
	handler := APIHandler(group, func() bool { return true }, m, APIOptions{Store: store, Router: router, Token: "test-token", MaxConcurrentWrites: 1})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- productRequest(handler, "PUT", "/api?key=first", `{}`, "test-token", "*") }()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("write did not start")
	}
	body := &failOnRead{}
	r := httptest.NewRequest("PUT", "/api?key=second", body)
	r.Header.Set("Authorization", "Bearer test-token")
	r.Header.Set("If-None-Match", "*")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 503 || w.Header().Get("Retry-After") != "1" || body.read {
		t.Fatal("write admission did not reject before buffering", w.Code, body.read)
	}
	for _, path := range []string{"/healthz", "/readyz", "/api?key=warm"} {
		if w := productRequest(handler, "GET", path, "", ""); w.Code != 200 {
			t.Fatal("hit/probe blocked", path, w.Code)
		}
	}
	store.release <- struct{}{}
	select {
	case w := <-done:
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("write did not recover")
	}
	store.release <- struct{}{}
	if w := productRequest(handler, "PUT", "/api?key=second", `{}`, "test-token", "*"); w.Code != 200 {
		t.Fatal("write admission did not recover", w.Code, w.Body.String())
	}
	if x := readMetric(t, m, "simplecache_admission_rejections_total", "write"); x.value != 1 {
		t.Fatal("rejection missing", x)
	}
	if x := readMetric(t, m, "simplecache_write_requests_inflight", ""); x.value != 0 {
		t.Fatal("write slot leaked", x)
	}
}

func TestKnownConflictIsNotAnUnknownCommit(t *testing.T) {
	store := &memoryProducts{values: map[string][]byte{"key": []byte(`{}`)}}
	group := cache.NewGroup("products", 1024, store, cache.Options{})
	m := telemetry.New()
	router := peer.NewRouterWithToken("local", time.Second, "test-token")
	defer router.Close()
	handler := APIHandler(group, func() bool { return true }, m, APIOptions{Store: store, Router: router, Token: "test-token"})
	if w := productRequest(handler, "PUT", "/api?key=key", `{}`, "test-token", `"v2"`); w.Code != 412 {
		t.Fatal(w.Code)
	}
	if readMetric(t, m, "simplecache_source_write_duration_seconds", "precondition_failed").count != 1 || readMetric(t, m, "simplecache_source_unknown_commits_total", "").value != 0 {
		t.Fatal("conflict counted as unknown")
	}
	store.writeErr = io.ErrUnexpectedEOF
	if w := productRequest(handler, "PUT", "/api?key=key", `{}`, "test-token", `"v1"`); w.Code != 503 {
		t.Fatal(w.Code)
	}
	if readMetric(t, m, "simplecache_source_unknown_commits_total", "").value != 1 {
		t.Fatal("unknown source outcome missing")
	}
}

func TestAPIMissOverloadHasRetryHintAndObservableCapacity(t *testing.T) {
	m := telemetry.New()
	started, release := make(chan struct{}, 1), make(chan struct{}, 1)
	defer close(release)
	group := cache.NewGroup("reads", 1024, cache.GetterFunc(func(ctx context.Context, key string) ([]byte, error) {
		if key == "cold" {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return []byte(key), nil
	}), cache.Options{Observer: m, MaxMissWaiters: 1})
	m.ObserveCache(group)
	if _, err := group.Get(context.Background(), "warm"); err != nil {
		t.Fatal(err)
	}
	handler := APIHandler(group, func() bool { return true }, m)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- productRequest(handler, "GET", "/api?key=cold", "", "") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("read did not start")
	}
	if readMetric(t, m, "simplecache_api_miss_waiters", "").value != 1 || readMetric(t, m, "simplecache_route_tasks_inflight", "").value != 1 {
		t.Fatal("miss capacity not observable")
	}
	w := productRequest(handler, "GET", "/api?key=excess", "", "")
	if w.Code != 503 || w.Header().Get("Retry-After") != "1" {
		t.Fatal("missing read overload/retry contract", w.Code, w.Header())
	}
	for _, path := range []string{"/healthz", "/readyz", "/api?key=warm"} {
		if w := productRequest(handler, "GET", path, "", ""); w.Code != 200 {
			t.Fatal("read pressure blocked hit/probe", path, w.Code)
		}
	}
	release <- struct{}{}
	select {
	case w := <-done:
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read did not finish")
	}
	if readMetric(t, m, "simplecache_admission_rejections_total", "api_waiter").value != 1 || readMetric(t, m, "simplecache_api_miss_waiters", "").value != 0 || readMetric(t, m, "simplecache_route_tasks_inflight", "").value != 0 {
		t.Fatal("rejection missing or read capacity leaked")
	}
}
