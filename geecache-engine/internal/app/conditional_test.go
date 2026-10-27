package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestConditionalProductWritesAcrossPeersAndHotCopies(t *testing.T) {
	for _, hot := range []int64{0, 128 << 10} {
		t.Run(fmt.Sprint(hot), func(t *testing.T) {
			store := &memoryProducts{values: map[string][]byte{"key": []byte(`{"v":1}`)}}
			nodes := productClusterWithHotBudget(t, store, hot)
			for _, node := range nodes {
				w := productRequest(node.api, "GET", "/api?key=key", "", "")
				if w.Code != 200 || w.Header().Get("ETag") != `"v1"` {
					t.Fatalf("source/Peer/hot lost version: %d %v", w.Code, w.Header())
				}
				// Repeated reads also cover admission to the optional hot area.
				w = productRequest(node.api, "GET", "/api?key=key", "", "")
				if w.Header().Get("ETag") != `"v1"` {
					t.Fatal("cached version changed")
				}
			}
			apply := func(node int, method, body, condition string, want int, version int64) {
				t.Helper()
				w := productRequest(nodes[node].api, method, "/api?key=key", body, "test-token", condition)
				if w.Code != want {
					t.Fatalf("%s %s: %d %s", method, condition, w.Code, w.Body.String())
				}
				var result mutationResponse
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if want == http.StatusPreconditionFailed {
					if result.SourceCommitted == nil || *result.SourceCommitted || result.Invalidation != nil || result.SourceVersion != nil {
						t.Fatal("failed condition was reported as a write or broadcast")
					}
				} else if result.SourceVersion == nil || *result.SourceVersion != version {
					t.Fatalf("wrong committed version: %+v", result)
				}
			}
			apply(0, "PUT", `{"v":2}`, `"v1"`, 200, 2)
			apply(1, "PUT", `{"v":99}`, `"v1"`, 412, 0)
			apply(2, "DELETE", "", `"v1"`, 412, 0)
			for _, node := range nodes {
				w := productRequest(node.api, "GET", "/api?key=key", "", "")
				if w.Code != 200 || w.Body.String() != `{"v":2}` || w.Header().Get("ETag") != `"v2"` {
					t.Fatalf("stale edit changed a new value: %d %s %v", w.Code, w.Body.String(), w.Header())
				}
			}
			apply(1, "DELETE", "", `"v2"`, 200, 3)
			for _, node := range nodes {
				if w := productRequest(node.api, "GET", "/api?key=key", "", ""); w.Code != 404 {
					t.Fatal("deleted copy survived")
				}
			}
			apply(2, "PUT", `{"v":99}`, `"v1"`, 412, 0)
			apply(2, "PUT", `{"v":4}`, "*", 200, 4)
			apply(0, "DELETE", "", `"v2"`, 412, 0)
			apply(0, "PUT", `{"v":99}`, "*", 412, 0)
			v, err := store.GetVersioned(context.Background(), "key")
			if err != nil || v.Version != 4 || string(v.Value) != `{"v":4}` {
				t.Fatal(v, err)
			}
		})
	}
}

func TestConcurrentConditionalEditsHaveOneWinner(t *testing.T) {
	for _, create := range []bool{false, true} {
		t.Run(fmt.Sprint(create), func(t *testing.T) {
			store := &memoryProducts{values: map[string][]byte{}}
			condition, wantVersion := "*", int64(1)
			if !create {
				store.values["key"] = []byte(`{}`)
				condition, wantVersion = `"v1"`, 2
			}
			nodes := productCluster(t, store)
			const clients = 16
			barrier, results := make(chan struct{}), make(chan int, clients)
			var wg sync.WaitGroup
			for i := 0; i < clients; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-barrier
					w := productRequest(nodes[i%3].api, "PUT", "/api?key=key", fmt.Sprintf(`{"writer":%d}`, i), "test-token", condition)
					results <- w.Code
				}(i)
			}
			close(barrier)
			wg.Wait()
			close(results)
			wins, conflicts := 0, 0
			for status := range results {
				if status == 200 {
					wins++
				} else if status == 412 {
					conflicts++
				} else {
					t.Fatalf("unexpected status %d", status)
				}
			}
			v, err := store.GetVersioned(context.Background(), "key")
			if wins != 1 || conflicts != clients-1 || err != nil || v.Version != wantVersion {
				t.Fatalf("wins=%d conflicts=%d value=%+v error=%v", wins, conflicts, v, err)
			}
		})
	}
}

func TestMutationPreconditionsCannotBeBypassed(t *testing.T) {
	store := &memoryProducts{values: map[string][]byte{"key": []byte(`{"v":1}`)}}
	nodes := productCluster(t, store)
	for _, tc := range []struct {
		method        string
		match, absent []string
		status        int
	}{
		{"PUT", nil, nil, 428}, {"DELETE", nil, nil, 428},
		{"PUT", []string{"*"}, nil, 400}, {"PUT", []string{`W/"v1"`}, nil, 400},
		{"PUT", []string{`"v1", "v2"`}, nil, 400}, {"PUT", []string{`"v0"`}, nil, 400},
		{"PUT", []string{`"v01"`}, nil, 400}, {"PUT", []string{`"v9223372036854775808"`}, nil, 400},
		{"PUT", []string{`"v1"`}, []string{"*"}, 400}, {"DELETE", nil, []string{"*"}, 400},
		{"PUT", []string{`"v1"`, `"v2"`}, nil, 400}, {"PUT", nil, []string{"*", "*"}, 400},
	} {
		r := httptest.NewRequest(tc.method, "/api?key=key", strings.NewReader(`{"v":99}`))
		r.Header.Set("Authorization", "Bearer test-token")
		for _, h := range tc.match {
			r.Header.Add("If-Match", h)
		}
		for _, h := range tc.absent {
			r.Header.Add("If-None-Match", h)
		}
		w := httptest.NewRecorder()
		nodes[0].api.ServeHTTP(w, r)
		if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body.String())
		}
	}
	v, err := store.GetVersioned(context.Background(), "key")
	if err != nil || v.Version != 1 || string(v.Value) != `{"v":1}` {
		t.Fatal("unsafe request changed source", v, err)
	}
}
