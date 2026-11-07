package cluster

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

func clientFor(server *httptest.Server) *Client {
	return &Client{address: strings.TrimPrefix(server.URL, "http://"), http: server.Client(), timeout: time.Second}
}

func TestBroadcastInvalidatesEveryKnownCopyAndReportsFailures(t *testing.T) {
	const token = "test-token"
	newGroup := func() *cache.Group {
		return cache.NewGroup("products", 1024, cache.GetterFunc(func(context.Context, string) ([]byte, error) {
			return []byte("value"), nil
		}), cache.Options{})
	}
	local, remote := newGroup(), newGroup()
	for _, group := range []*cache.Group{local, remote} {
		if _, err := group.Get(context.Background(), "a/b +中文"); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(NewHandlerWithToken(token, remote))
	defer server.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer broken.Close()
	router := NewDirectory("local", 50*time.Millisecond, token)
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
			t.Fatal("failed member reported success")
		}
	}
}

func TestVersionedBroadcastReportsLegacyMembersWithoutClaimingVersionSupport(t *testing.T) {
	local := cache.NewGroup("products", 1024, fixedVersionSource{1}, cache.Options{})
	remote := cache.NewGroup("products", 1024, fixedVersionSource{1}, cache.Options{})
	_, _ = local.Get(context.Background(), "key")
	_, _ = remote.Get(context.Background(), "key")
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
