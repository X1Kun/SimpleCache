package cluster

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
)

func TestMemberInvalidationRequiresTokenAndNeverForwards(t *testing.T) {
	g := cache.NewGroup("products", 1024, cache.GetterFunc(func(context.Context, string) ([]byte, error) {
		return []byte("value"), nil
	}), cache.Options{})
	_, _ = g.Get(context.Background(), "key")
	for _, tc := range []struct {
		token  string
		status int
	}{{"", 401}, {"wrong", 401}, {"shared-token", 204}, {"shared-token", 204}} {
		req := httptest.NewRequest(http.MethodDelete, BasePath+"products/key", nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		w := httptest.NewRecorder()
		NewHandlerWithToken("shared-token", g).ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("status=%d want=%d", w.Code, tc.status)
		}
	}
	if g.Stats().Entries != 0 {
		t.Fatal("cache not invalidated")
	}
	w := httptest.NewRecorder()
	NewHandler(g).ServeHTTP(w, httptest.NewRequest(http.MethodDelete, BasePath+"products/key", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatal("read-only member accepted mutation")
	}
}

func TestMemberTransportNeverServesReads(t *testing.T) {
	g := cache.NewGroup("products", 1024, cache.GetterFunc(func(context.Context, string) ([]byte, error) {
		t.Fatal("notification transport loaded source")
		return nil, nil
	}), cache.Options{})
	h := NewHandlerWithToken("token", g)
	for _, path := range []string{BasePath + "products/key", "/_geecache/products/key"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 405 || w.Header().Get("Allow") != "DELETE" {
			t.Fatal(path, w.Code, w.Header())
		}
	}
}

func TestVersionedMemberNotificationsValidateBeforeInvalidatingAndAcknowledge(t *testing.T) {
	g := cache.NewGroup("products", 1024, fixedVersionSource{4}, cache.Options{})
	handler := NewHandlerWithToken("token", g)
	_, _ = g.Get(context.Background(), "key")
	for _, raw := range []string{"", "0", "-1", "01", "+2", "2, 3", "invalid", "9223372036854775808"} {
		req := httptest.NewRequest("DELETE", BasePath+"products/key", nil)
		req.Header.Set("Authorization", "Bearer token")
		req.Header.Set(InvalidationVersionHeader, raw)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 400 || g.Stats().Entries != 1 {
			t.Fatalf("invalid version %q mutated state: %d %+v", raw, w.Code, g.Stats())
		}
	}
	req := httptest.NewRequest("DELETE", BasePath+"products/key", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Add(InvalidationVersionHeader, "3")
	req.Header.Add(InvalidationVersionHeader, "4")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 400 || g.Stats().Entries != 1 {
		t.Fatal(w.Code, g.Stats())
	}
	for _, v := range []int64{3, 4, 2} {
		req := httptest.NewRequest("DELETE", BasePath+"products/key", nil)
		req.Header.Set("Authorization", "Bearer token")
		req.Header.Set(InvalidationVersionHeader, strconv.FormatInt(v, 10))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 204 || w.Header().Get(InvalidationVersionHeader) != strconv.FormatInt(v, 10) || g.Stats().Entries != 1 {
			t.Fatal("old event removed newer cache or lacked acknowledgement", w.Code, w.Header(), g.Stats())
		}
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := clientFor(server)
	client.token = "token"
	if e := client.InvalidateVersion(context.Background(), "products", "key", 5); e != nil {
		t.Fatal(e)
	}
	if _, e := g.Get(context.Background(), "key"); !errors.Is(e, cache.ErrStaleVersion) {
		t.Fatal("notification version did not reach group", e)
	}
	if e := client.Invalidate(context.Background(), "products", "key"); e != nil {
		t.Fatal("legacy force request no longer supported", e)
	}
}
