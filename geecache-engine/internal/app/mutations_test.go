package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer"
)

type memoryProducts struct {
	mu       sync.Mutex
	values   map[string][]byte
	versions map[string]int64
	reads    int
	writeErr error
	onPut    func()
}

func (s *memoryProducts) versionLocked(key string) int64 {
	if v := s.versions[key]; v > 0 {
		return v
	}
	if _, ok := s.values[key]; ok {
		return 1
	}
	return 0
}
func (s *memoryProducts) Get(ctx context.Context, key string) ([]byte, error) {
	v, err := s.GetVersioned(ctx, key)
	return v.Value, err
}
func (s *memoryProducts) GetVersioned(_ context.Context, key string) (cache.SourceValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	v, ok := s.values[key]
	if !ok {
		return cache.SourceValue{}, cache.ErrNotFound
	}
	return cache.SourceValue{Value: append([]byte(nil), v...), Version: s.versionLocked(key)}, nil
}
func (s *memoryProducts) putLocked(key string, value []byte) int64 {
	version := s.versionLocked(key) + 1
	if s.versions == nil {
		s.versions = map[string]int64{}
	}
	if s.values == nil {
		s.values = map[string][]byte{}
	}
	s.versions[key] = version
	s.values[key] = append([]byte(nil), value...)
	if s.onPut != nil {
		s.onPut()
	}
	return version
}
func (s *memoryProducts) Put(_ context.Context, key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	s.putLocked(key, value)
	return nil
}
func (s *memoryProducts) PutIfVersion(_ context.Context, key string, value []byte, expected int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	_, exists := s.values[key]
	if (expected == 0 && exists) || (expected > 0 && (!exists || expected != s.versionLocked(key))) {
		return 0, cache.ErrPreconditionFailed
	}
	return s.putLocked(key, value), nil
}
func (s *memoryProducts) DeleteIfVersion(_ context.Context, key string, expected int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	if _, ok := s.values[key]; !ok || expected != s.versionLocked(key) {
		return 0, cache.ErrPreconditionFailed
	}
	version := s.versionLocked(key) + 1
	if s.versions == nil {
		s.versions = map[string]int64{}
	}
	s.versions[key] = version
	delete(s.values, key)
	return version, nil
}

type productNode struct {
	group  *cache.Group
	router *peer.Router
	api    http.Handler
}

func productCluster(t *testing.T, store MutableSource) []productNode {
	t.Helper()
	return productClusterWithHotBudget(t, store, 0)
}

func productClusterWithHotBudget(t *testing.T, store MutableSource, hotBytes int64) []productNode {
	t.Helper()
	const token = "test-token"
	var nodes []productNode
	var members []peer.Member
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("node-%d", i)
		router := peer.NewRouterWithToken(id, time.Second, token)
		t.Cleanup(router.Close)
		group := cache.NewGroup("products", 1<<20, store, cache.Options{Peers: router, HotCacheBytes: hotBytes})
		server := httptest.NewServer(peer.NewHandlerWithToken(token, group))
		t.Cleanup(server.Close)
		members = append(members, peer.Member{ID: id, Address: strings.TrimPrefix(server.URL, "http://")})
		api := APIHandler(group, func() bool { return true }, nil, APIOptions{Store: store, Router: router, Token: token})
		nodes = append(nodes, productNode{group, router, api})
	}
	for _, node := range nodes {
		if err := node.router.Update(members); err != nil {
			t.Fatal(err)
		}
	}
	return nodes
}

func productRequest(handler http.Handler, method, path, body, token string, condition ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if len(condition) > 0 {
		if condition[0] == "*" {
			req.Header.Set("If-None-Match", "*")
		} else {
			req.Header.Set("If-Match", condition[0])
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestProductWriteBroadcastReadDeleteLifecycle(t *testing.T) {
	for _, hotBytes := range []int64{0, 128 << 10} {
		t.Run(fmt.Sprintf("hot-bytes-%d", hotBytes), func(t *testing.T) {
			productWriteBroadcastReadDeleteLifecycle(t, hotBytes)
		})
	}
}

func productWriteBroadcastReadDeleteLifecycle(t *testing.T, hotBytes int64) {
	t.Helper()
	store := &memoryProducts{values: map[string][]byte{"product:42": []byte(`{"price":100}`)}}
	nodes := productClusterWithHotBudget(t, store, hotBytes)
	for _, node := range nodes {
		w := productRequest(node.api, "GET", "/api?key=product:42", "", "")
		if w.Code != 200 || w.Body.String() != `{"price":100}` {
			t.Fatalf("read: %d %s", w.Code, w.Body.String())
		}
	}
	if store.reads != 1 {
		t.Fatalf("stable routing should share one source fill, got %d", store.reads)
	}
	if hotBytes > 0 {
		totalHot := 0
		for _, node := range nodes {
			totalHot += node.group.Stats().HotEntries
		}
		if totalHot != 2 {
			t.Fatalf("expected two actual HTTP peer replicas before mutation, got %d", totalHot)
		}
	}
	// Populate fallback/historical copies when a node has no hot copy.
	for _, node := range nodes {
		if _, err := node.group.GetLocal(context.Background(), "product:42"); err != nil {
			t.Fatal(err)
		}
	}
	w := productRequest(nodes[0].api, "PUT", "/api?key=product:42", `{"price":120}`, "test-token", `"v1"`)
	var response mutationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || response.SourceCommitted == nil || !*response.SourceCommitted ||
		response.Invalidation == nil || !response.Invalidation.Complete || len(response.Invalidation.Results) != 3 {
		t.Fatalf("write: %d %s", w.Code, w.Body.String())
	}
	for _, node := range nodes {
		if node.group.Stats().Entries != 0 {
			t.Fatal("a fallback/historical copy was not invalidated")
		}
	}
	readsBefore := store.reads
	for _, node := range nodes {
		w := productRequest(node.api, "GET", "/api?key=product:42", "", "")
		if w.Code != 200 || w.Body.String() != `{"price":120}` {
			t.Fatalf("stale read after acknowledgement: %d %s", w.Code, w.Body.String())
		}
	}
	if store.reads != readsBefore+1 {
		t.Fatal("new value was not cached at the owner")
	}
	// A newly added product must not be rejected by a startup Bloom snapshot.
	w = productRequest(nodes[1].api, "PUT", "/api?key=product:new", `{"price":1}`, "test-token", "*")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = productRequest(nodes[2].api, "GET", "/api?key=product:new", "", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = productRequest(nodes[1].api, "DELETE", "/api?key=product:42", "", "test-token", `"v2"`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, node := range nodes {
		if w := productRequest(node.api, "GET", "/api?key=product:42", "", ""); w.Code != 404 {
			t.Fatalf("deleted product returned %d", w.Code)
		}
	}
}

func TestMutationValidationAndManualInvalidation(t *testing.T) {
	store := &memoryProducts{values: map[string][]byte{"key": []byte(`{"v":1}`)}}
	nodes := productCluster(t, store)
	for _, tc := range []struct {
		method, path, body, token string
		status                    int
	}{
		{"PUT", "/api?key=key", `{}`, "", 401},
		{"DELETE", "/api?key=key", "", "wrong", 401},
		{"PUT", "/api", `{}`, "test-token", 400},
		{"PUT", "/api?key=key", `invalid`, "test-token", 400},
		{"PUT", "/api?key=key", strings.Repeat("x", cache.MaxValueBytes+1), "test-token", 413},
		{"GET", "/invalidate?key=key", "", "test-token", 405},
		{"POST", "/invalidate?key=key", "", "", 401},
	} {
		w := productRequest(nodes[0].api, tc.method, tc.path, tc.body, tc.token)
		if w.Code != tc.status {
			t.Fatalf("%s %s: %d want %d", tc.method, tc.path, w.Code, tc.status)
		}
	}
	for _, node := range nodes {
		_, _ = node.group.GetLocal(context.Background(), "key")
	}
	w := productRequest(nodes[0].api, "POST", "/invalidate?key=key", "", "test-token")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if len(store.values) != 1 {
		t.Fatal("manual invalidation changed the source")
	}
	for _, node := range nodes {
		if node.group.Stats().Entries != 0 {
			t.Fatal("manual invalidation missed a node")
		}
	}
}

func TestCommittedWriteReportsPartialBroadcast(t *testing.T) {
	store := &memoryProducts{values: map[string][]byte{}}
	nodes := productCluster(t, store)
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", 503)
	}))
	defer failed.Close()
	members := nodes[0].router.Members()
	members = append(members, peer.Member{ID: "failed", Address: strings.TrimPrefix(failed.URL, "http://")})
	if err := nodes[0].router.Update(members); err != nil {
		t.Fatal(err)
	}
	w := productRequest(nodes[0].api, "PUT", "/api?key=key", `{"v":2}`, "test-token", "*")
	var response mutationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 202 || response.SourceCommitted == nil || !*response.SourceCommitted || response.Invalidation.Complete {
		t.Fatalf("partial broadcast masked: %d %s", w.Code, w.Body.String())
	}
	v, err := store.Get(context.Background(), "key")
	if err != nil || string(v) != `{"v":2}` {
		t.Fatal("database commit was not retained")
	}
}

func TestWriteErrorIsAmbiguousAndStillInvalidates(t *testing.T) {
	store := &memoryProducts{values: map[string][]byte{"key": []byte(`{"v":1}`)}, writeErr: errors.New("connection lost")}
	nodes := productCluster(t, store)
	_, _ = nodes[1].group.GetLocal(context.Background(), "key")
	w := productRequest(nodes[0].api, "PUT", "/api?key=key", `{"v":2}`, "test-token", `"v1"`)
	var response mutationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 503 || response.SourceCommitted != nil || !response.Invalidation.Complete || nodes[1].group.Stats().Entries != 0 {
		t.Fatalf("ambiguous write: %d %s", w.Code, w.Body.String())
	}
}

func TestCallerDisconnectDoesNotCancelPostCommitBroadcast(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &memoryProducts{values: map[string][]byte{"key": []byte(`{"v":1}`)}, onPut: cancel}
	nodes := productCluster(t, store)
	_, _ = nodes[2].group.GetLocal(context.Background(), "key")
	req := httptest.NewRequest("PUT", "/api?key=key", strings.NewReader(`{"v":2}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("If-Match", `"v1"`)
	w := httptest.NewRecorder()
	nodes[0].api.ServeHTTP(w, req)
	if w.Code != 200 || ctx.Err() == nil || nodes[2].group.Stats().Entries != 0 {
		t.Fatalf("post-commit broadcast canceled: %d %s", w.Code, w.Body.String())
	}
}

func TestSlowHealthyPeerDoesNotTriggerEarlyFallback(t *testing.T) {
	owner := cache.NewGroup("products", 1024, cache.GetterFunc(func(ctx context.Context, _ string) ([]byte, error) {
		timer := time.NewTimer(500 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return []byte("owner"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}), cache.Options{})
	server := httptest.NewServer(peer.NewHandler(owner))
	defer server.Close()
	router := peer.NewRouter("entry", peerReadTimeout)
	defer router.Close()
	if err := router.Update([]peer.Member{{ID: "owner", Address: strings.TrimPrefix(server.URL, "http://")}}); err != nil {
		t.Fatal(err)
	}
	entry := cache.NewGroup("products", 1024, cache.GetterFunc(func(context.Context, string) ([]byte, error) {
		return nil, errors.New("unexpected fallback")
	}), cache.Options{Peers: router})
	v, err := entry.Get(context.Background(), "key")
	if err != nil || v.String() != "owner" {
		t.Fatalf("premature fallback: %q %v", v.String(), err)
	}
}

func TestPostgresConfigurationRequiresExplicitCredentials(t *testing.T) {
	t.Setenv("SOURCE_MODE", "postgres")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CACHE_ADMIN_TOKEN", "")
	if _, err := FromEnv(8001, true); err == nil {
		t.Fatal("missing PostgreSQL credentials accepted")
	}
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	if _, err := FromEnv(8001, true); err == nil {
		t.Fatal("writable mode without an admin token accepted")
	}
	t.Setenv("CACHE_ADMIN_TOKEN", "test-token")
	if _, err := FromEnv(8001, true); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOURCE_MODE", "unknown")
	if _, err := FromEnv(8001, true); err == nil {
		t.Fatal("unknown source accepted")
	}
}
