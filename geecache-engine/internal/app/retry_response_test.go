package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var responseV10 = []byte(`{"value":"old"}`)
var responseV11 = []byte(`{"value":"new"}`)

func contractPath(key string) string { return "/api?" + url.Values{"key": {key}}.Encode() }

// The first write fully completes (including fan-out) into a private response
// buffer. Only delivery is lost; this intentionally excludes the crash window.
func checkLostWriteResponseRetry(t *testing.T, store MutableSource, key string, audit func()) {
	t.Helper()
	nodes := productClusterWithRouting(t, store, 0, true)
	generated := make(chan *httptest.ResponseRecorder, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buffered := httptest.NewRecorder()
		nodes[0].api.ServeHTTP(buffered, r)
		generated <- buffered
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("cannot drop response: %v", err)
			return
		}
		_ = connection.Close()
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, server.URL+contractPath(key), strings.NewReader(string(responseV11)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("If-Match", `"v10"`)
	reply, deliveryErr := server.Client().Do(req)
	if reply != nil {
		_ = reply.Body.Close()
	}
	if deliveryErr == nil {
		t.Fatal("response was not lost")
	}
	var buffered *httptest.ResponseRecorder
	select {
	case buffered = <-generated:
	case <-ctx.Done():
		t.Fatal("server did not finish write", ctx.Err())
	}
	var first mutationResponse
	if err := json.Unmarshal(buffered.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if buffered.Code != 200 || first.SourceCommitted == nil || !*first.SourceCommitted || first.SourceVersion == nil || *first.SourceVersion != 11 || first.Invalidation == nil || !first.Invalidation.Complete {
		t.Fatal("first write did not complete before delivery loss", buffered.Code, buffered.Body.String())
	}
	retry := productRequest(nodes[1].api, http.MethodPut, contractPath(key), string(responseV11), "test-token", `"v10"`)
	var second mutationResponse
	if err := json.Unmarshal(retry.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if retry.Code != http.StatusPreconditionFailed || second.SourceCommitted == nil || *second.SourceCommitted || second.SourceVersion != nil || second.Invalidation != nil {
		t.Fatal("old condition replayed a successful mutation/result", retry.Code, retry.Body.String())
	}
	audit()
}

func TestLostWriteResponseRetryKeepsOneCommittedVersion(t *testing.T) {
	key := "response-loss"
	store := &memoryProducts{values: map[string][]byte{key: responseV10}, versions: map[string]int64{key: 10}}
	writes := 0
	store.onPut = func() { writes++ }
	checkLostWriteResponseRetry(t, store, key, func() {
		value, err := store.GetVersioned(context.Background(), key)
		if err != nil || value.Version != 11 || string(value.Value) != string(responseV11) || writes != 1 {
			t.Fatalf("mutation repeated: version=%d value=%s writes=%d err=%v", value.Version, value.Value, writes, err)
		}
	})
}

type deliveredValue struct {
	version int64
	body    string
}
type referenceVersionClient map[string]deliveredValue

// Test-only reference policy. Source versions are compared per key; this is not
// a production UI implementation or a server-side linearizability guarantee.
func (c referenceVersionClient) accept(key string, value deliveredValue) {
	if current, ok := c[key]; !ok || value.version >= current.version {
		c[key] = value
	}
}
func deliveredResponse(response *http.Response) (deliveredValue, error) {
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return deliveredValue{}, err
	}
	if response.StatusCode != 200 {
		return deliveredValue{}, fmt.Errorf("read status %d", response.StatusCode)
	}
	var version int64
	if _, err := fmt.Sscanf(response.Header.Get("ETag"), `"v%d"`, &version); err != nil || response.Header.Get("ETag") != versionETag(version) {
		return deliveredValue{}, fmt.Errorf("invalid source ETag")
	}
	return deliveredValue{version: version, body: string(body)}, nil
}

func TestLateResponseDoesNotRollBackGuardedClientOrServerCache(t *testing.T) {
	key := "response-order"
	store := &memoryProducts{values: map[string][]byte{key: responseV10}, versions: map[string]int64{key: 10}}
	nodes := productClusterWithRouting(t, store, 0, true)
	generated := make(chan deliveredValue, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	delayed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buffered := httptest.NewRecorder()
		nodes[0].api.ServeHTTP(buffered, r)
		value, err := deliveredResponse(buffered.Result())
		if err != nil {
			t.Error(err)
			return
		}
		generated <- value // Cache lookup and response generation have already finished.
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		for name, values := range buffered.Header() {
			w.Header()[name] = append([]string(nil), values...)
		}
		w.WriteHeader(buffered.Code)
		_, _ = w.Write(buffered.Body.Bytes())
	}))
	t.Cleanup(delayed.Close)
	t.Cleanup(unblock)
	live := httptest.NewServer(nodes[0].api)
	t.Cleanup(live.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type result struct {
		value deliveredValue
		err   error
	}
	late := make(chan result, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, delayed.URL+contractPath(key), nil)
		response, err := delayed.Client().Do(req)
		if err != nil {
			late <- result{err: err}
			return
		}
		value, err := deliveredResponse(response)
		late <- result{value, err}
	}()
	select {
	case old := <-generated:
		if old.version != 10 || old.body != string(responseV10) {
			t.Fatal(old)
		}
	case <-ctx.Done():
		t.Fatal("old response was not generated", ctx.Err())
	}
	write := productRequest(nodes[1].api, http.MethodPut, contractPath(key), string(responseV11), "test-token", `"v10"`)
	if write.Code != 200 {
		t.Fatal(write.Code, write.Body.String())
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, live.URL+contractPath(key), nil)
	response, err := live.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := deliveredResponse(response)
	if err != nil || fresh.version != 11 || fresh.body != string(responseV11) {
		t.Fatal(fresh, err)
	}
	guarded := referenceVersionClient{}
	unsafe := map[string]deliveredValue{}
	guarded.accept(key, fresh)
	unsafe[key] = fresh
	store.mu.Lock()
	reads := store.reads
	store.mu.Unlock()
	unblock()
	var old result
	select {
	case old = <-late:
	case <-ctx.Done():
		t.Fatal("old response did not arrive", ctx.Err())
	}
	if old.err != nil || old.value.version != 10 || old.value.body != string(responseV10) {
		t.Fatal(old)
	}
	unsafe[key] = old.value
	guarded.accept(key, old.value)
	if unsafe[key].version != 10 {
		t.Fatal("negative control did not reproduce client rollback")
	}
	if guarded[key] != fresh {
		t.Fatal("client regressed after an old response", guarded[key])
	}
	// Other keys have independent version sequences, including lower generations.
	guarded.accept("other", deliveredValue{version: 1, body: "other"})
	if guarded["other"].version != 1 || guarded[key].version != 11 {
		t.Fatal("client compared versions across keys")
	}
	final, err := nodes[0].group.GetDirect(ctx, key)
	if err != nil || final.Version() != 11 || final.String() != string(responseV11) {
		t.Fatal("late delivery changed server cache", final, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.reads != reads {
		t.Fatal("final cache check reloaded and hid an eviction/regression", strconv.Itoa(store.reads))
	}
}
