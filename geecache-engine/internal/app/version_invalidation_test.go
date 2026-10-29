package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer"
)

func notifyVersion(handler http.Handler, version string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/invalidate?key=key", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set(peer.InvalidationVersionHeader, version)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestDirectReadVersionedInvalidationLifecycleAndOutOfOrderDelivery(t *testing.T) {
	store := &memoryProducts{values: map[string][]byte{"key": []byte(`{"v":1}`)}}
	nodes := productClusterWithRouting(t, store, 0, true)
	warm := func(version int64) {
		t.Helper()
		for _, node := range nodes {
			w := productRequest(node.api, "GET", "/api?key=key", "", "")
			if w.Code != 200 || w.Header().Get("ETag") != `"v`+strconv.FormatInt(version, 10)+`"` {
				t.Fatal(w.Code, w.Header(), w.Body.String())
			}
		}
	}
	mutate := func(method, body, condition string, version int64) {
		t.Helper()
		w := productRequest(nodes[0].api, method, "/api?key=key", body, "test-token", condition)
		var response mutationResponse
		if e := json.Unmarshal(w.Body.Bytes(), &response); e != nil {
			t.Fatal(e)
		}
		if w.Code != 200 || response.SourceVersion == nil || *response.SourceVersion != version || response.Invalidation == nil || response.Invalidation.Version != version || !response.Invalidation.Complete || len(response.Invalidation.Results) != 3 {
			t.Fatal("source version was not broadcast to all holders", w.Code, w.Body.String())
		}
	}
	warm(1)
	mutate("PUT", `{"v":2}`, `"v1"`, 2)
	warm(2)
	mutate("PUT", `{"v":3}`, `"v2"`, 3)
	warm(3)
	reads := store.reads
	for _, version := range []string{"2", "3", "1", "2"} {
		if w := notifyVersion(nodes[1].api, version); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		warm(3)
	}
	if store.reads != reads {
		t.Fatal("old or duplicate events evicted fresh values", store.reads, reads)
	}
	mutate("DELETE", "", `"v3"`, 4)
	for _, node := range nodes {
		if w := productRequest(node.api, "GET", "/api?key=key", "", ""); w.Code != 404 {
			t.Fatal("deleted cache survived", w.Code)
		}
	}
	mutate("PUT", `{"v":5}`, "*", 5)
	warm(5)
	reads = store.reads
	if w := notifyVersion(nodes[2].api, "4"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	warm(5)
	if store.reads != reads {
		t.Fatal("late deletion evicted recreated generation")
	}
}

func TestInvalidationVersionCannotBeInjectedIntoWritesOrMalformedNotifications(t *testing.T) {
	store := &memoryProducts{values: map[string][]byte{"key": []byte(`{"v":1}`)}}
	nodes := productClusterWithRouting(t, store, 0, true)
	_, _ = nodes[0].group.GetDirect(context.Background(), "key")
	for _, value := range []string{"", "0", "-1", "01", "2,3", "9223372036854775808"} {
		if w := notifyVersion(nodes[0].api, value); w.Code != 400 || nodes[0].group.Stats().Entries != 1 {
			t.Fatal("malformed notification changed cache", value, w.Code)
		}
	}
	req := httptest.NewRequest("PUT", "/api?key=key", strings.NewReader(`{"v":99}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("If-Match", `"v1"`)
	req.Header.Set(peer.InvalidationVersionHeader, "999")
	w := httptest.NewRecorder()
	nodes[0].api.ServeHTTP(w, req)
	if w.Code != 400 || store.versionLocked("key") != 1 {
		t.Fatal("client fabricated committed source version", w.Code)
	}
}

func TestCommittedVersionIsPartialWhenPeerHasOnlyLegacyInvalidation(t *testing.T) {
	store := &memoryProducts{values: map[string][]byte{"key": []byte(`{"v":1}`)}}
	nodes := productClusterWithRouting(t, store, 0, true)
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer old.Close()
	members := nodes[0].router.Members()
	members = append(members, peer.Member{ID: "old", Address: strings.TrimPrefix(old.URL, "http://")})
	if e := nodes[0].router.Update(members); e != nil {
		t.Fatal(e)
	}
	w := productRequest(nodes[0].api, "PUT", "/api?key=key", `{"v":2}`, "test-token", `"v1"`)
	var response mutationResponse
	if e := json.Unmarshal(w.Body.Bytes(), &response); e != nil {
		t.Fatal(e)
	}
	if w.Code != 202 || response.SourceCommitted == nil || !*response.SourceCommitted || response.SourceVersion == nil || *response.SourceVersion != 2 || response.Invalidation.Complete {
		t.Fatal(w.Code, w.Body.String())
	}
	found := false
	for _, r := range response.Invalidation.Results {
		if r.Node == "old" && r.Status == "unversioned" {
			found = true
		}
	}
	if !found {
		t.Fatal("legacy acknowledgement was counted as version-aware")
	}
}

type invalidCommitVersion struct{ *memoryProducts }

func (s invalidCommitVersion) PutIfVersion(ctx context.Context, key string, value []byte, expected int64) (int64, error) {
	_, e := s.memoryProducts.PutIfVersion(ctx, key, value, expected)
	return 0, e // Commit happened, but this adapter violated the version contract.
}
func TestInvalidCommitVersionRemainsUnknownAndUsesForceInvalidation(t *testing.T) {
	store := &memoryProducts{values: map[string][]byte{"key": []byte(`{"v":1}`)}}
	nodes := productClusterWithRouting(t, invalidCommitVersion{store}, 0, true)
	for _, node := range nodes {
		_, _ = node.group.GetDirect(context.Background(), "key")
	}
	w := productRequest(nodes[0].api, "PUT", "/api?key=key", `{"v":2}`, "test-token", `"v1"`)
	var response mutationResponse
	if e := json.Unmarshal(w.Body.Bytes(), &response); e != nil {
		t.Fatal(e)
	}
	if w.Code != 503 || response.SourceCommitted != nil || response.SourceVersion != nil || response.Invalidation == nil || response.Invalidation.Version != 0 || !response.Invalidation.Complete {
		t.Fatal("invalid commit version was reported as authoritative", w.Code, w.Body.String())
	}
	for _, node := range nodes {
		if node.group.Stats().Entries != 0 {
			t.Fatal("ambiguous source write did not force invalidate")
		}
		if v, e := node.group.GetDirect(context.Background(), "key"); e != nil || v.Version() != 2 {
			t.Fatal(v, e)
		}
	}
}
