package peer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
)

func TestBroadcastInvalidatesEveryKnownCopyAndReportsFailures(t *testing.T) {
	const token = "test-token"
	newGroup := func() *cache.Group {
		return cache.NewGroup("products", 1024, cache.GetterFunc(func(context.Context, string) ([]byte, error) {
			return []byte("value"), nil
		}), cache.Options{Peers: forbiddenPicker{}})
	}
	local, remote := newGroup(), newGroup()
	for _, group := range []*cache.Group{local, remote} {
		if _, err := group.GetLocal(context.Background(), "a/b +中文"); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(NewHandlerWithToken(token, remote))
	defer server.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer broken.Close()
	router := NewRouterWithToken("local", 50*time.Millisecond, token)
	defer router.Close()
	if err := router.Update([]Member{
		{ID: "remote", Address: strings.TrimPrefix(server.URL, "http://")},
		{ID: "broken", Address: strings.TrimPrefix(broken.URL, "http://")},
	}); err != nil {
		t.Fatal(err)
	}
	report, err := router.Invalidate(context.Background(), local, "a/b +中文")
	if err != nil || report.Complete || len(report.Results) != 3 {
		t.Fatalf("partial failure not reported: %+v %v", report, err)
	}
	if local.Stats().Entries != 0 || remote.Stats().Entries != 0 {
		t.Fatal("local or fallback copy survived invalidation")
	}
	for _, result := range report.Results {
		if result.Node == "broken" && result.Status == "ok" {
			t.Fatal("failed peer reported success")
		}
	}
}

func TestPeerInvalidationRequiresTokenAndNeverForwards(t *testing.T) {
	g := cache.NewGroup("products", 1024, cache.GetterFunc(func(context.Context, string) ([]byte, error) {
		return []byte("value"), nil
	}), cache.Options{Peers: forbiddenPicker{}})
	_, _ = g.GetLocal(context.Background(), "key")
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
		t.Fatal("read-only peer accepted mutation")
	}
}
