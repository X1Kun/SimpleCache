package peer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
)

type fixedVersionSource struct{ version int64 }

func (s fixedVersionSource) Get(context.Context, string) ([]byte, error) { return []byte("value"), nil }
func (s fixedVersionSource) GetVersioned(context.Context, string) (cache.SourceValue, error) {
	return cache.SourceValue{Value: []byte("value"), Version: s.version}, nil
}

func TestVersionedPeerNotificationsValidateBeforeInvalidatingAndAcknowledge(t *testing.T) {
	g := cache.NewGroup("products", 1024, fixedVersionSource{4}, cache.Options{})
	handler := NewHandlerWithToken("token", g)
	_, _ = g.GetDirect(context.Background(), "key")
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
	if _, e := g.GetDirect(context.Background(), "key"); !errors.Is(e, cache.ErrStaleVersion) {
		t.Fatal("notification version did not reach group", e)
	}
	if e := client.Invalidate(context.Background(), "products", "key"); e != nil {
		t.Fatal("legacy force request no longer supported", e)
	}
}

func TestVersionedBroadcastReportsLegacyPeersWithoutClaimingVersionSupport(t *testing.T) {
	local := cache.NewGroup("products", 1024, fixedVersionSource{1}, cache.Options{})
	remote := cache.NewGroup("products", 1024, fixedVersionSource{1}, cache.Options{})
	_, _ = local.GetDirect(context.Background(), "key")
	_, _ = remote.GetDirect(context.Background(), "key")
	modern := httptest.NewServer(NewHandlerWithToken("token", remote))
	defer modern.Close()
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(InvalidationVersionHeader) != "2" {
			t.Errorf("missing committed version: %v", r.Header)
		}
		w.WriteHeader(204) // Older binaries delete the key but do not retain a floor.
	}))
	defer legacy.Close()
	router := NewDirectory("local", time.Second, "token")
	defer router.Close()
	if e := router.Update([]Member{{ID: "modern", Address: strings.TrimPrefix(modern.URL, "http://")}, {ID: "legacy", Address: strings.TrimPrefix(legacy.URL, "http://")}}); e != nil {
		t.Fatal(e)
	}
	report, e := router.InvalidateVersion(context.Background(), local, "key", 2)
	if e != nil || report.Version != 2 || report.Complete || len(report.Results) != 3 {
		t.Fatal(report, e)
	}
	statuses := map[string]string{}
	for _, r := range report.Results {
		statuses[r.Node] = r.Status
	}
	if statuses["local"] != "ok" || statuses["modern"] != "ok" || statuses["legacy"] != "unversioned" {
		t.Fatal(report)
	}
	if local.Stats().Entries != 0 || remote.Stats().Entries != 0 {
		t.Fatal("local or remote entry survived")
	}
}

func TestVersionedInvalidationClientRequiresMatchingAcknowledgement(t *testing.T) {
	for _, ack := range []string{"", "1", "03", "3,3"} {
		t.Run(ack, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(InvalidationVersionHeader, ack)
				w.WriteHeader(204)
			}))
			defer server.Close()
			if e := clientFor(server).InvalidateVersion(context.Background(), "products", "key", 3); !errors.Is(e, ErrUnversionedInvalidation) {
				t.Fatal(e)
			}
		})
	}
}
