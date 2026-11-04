package democlient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/demo"
)

func TestReadCanonicalKeysAndClassifiesResponses(t *testing.T) {
	key := "Auto-空 +/&?"
	var redirects int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			redirects++
			return
		}
		switch r.URL.Query().Get("key") {
		case "wrong":
			fmt.Fprint(w, "wrong")
		case "unavailable":
			w.WriteHeader(503)
		case "redirect":
			http.Redirect(w, r, "/redirected", 302)
		case "oversized":
			fmt.Fprint(w, strings.Repeat("x", maxBodyBytes+1))
		default:
			if r.URL.Query().Get("key") != key || strings.Contains(r.URL.RawQuery, "+") {
				t.Errorf("key encoding: %s", r.URL)
			}
			fmt.Fprint(w, demo.AutoValue(key, 512))
		}
	}))
	defer server.Close()
	c := NewClient(1, time.Second)
	defer c.CloseIdleConnections()
	for _, tc := range []struct {
		key           string
		failed, wrong bool
		status        int
	}{
		{key, false, false, 200}, {"wrong", false, true, 200}, {"unavailable", true, false, 503}, {"redirect", true, false, 302}, {"oversized", true, false, 200},
	} {
		got := Read(context.Background(), c, server.URL+"/", tc.key, 512)
		if got.Failed != tc.failed || got.Incorrect != tc.wrong || got.Status != tc.status || got.ElapsedMS <= 0 {
			t.Errorf("%q: %+v", tc.key, got)
		}
		if len(got.Detail) > 600 {
			t.Errorf("unbounded evidence: %d", len(got.Detail))
		}
	}
	if redirects != 0 {
		t.Fatal("followed redirect")
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenBody struct{ closed bool }

func (*brokenBody) Read([]byte) (int, error) { return 0, errors.New("private backend error") }
func (b *brokenBody) Close() error           { b.closed = true; return nil }

func TestReadFailureBoundaries(t *testing.T) {
	body := &brokenBody{}
	c := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return &http.Response{StatusCode: 200, Body: body}, nil })}
	got := Read(context.Background(), c, "http://localhost", "Auto-1", 0)
	if !got.Failed || got.Incorrect || !body.closed || strings.Contains(got.Detail, "private") {
		t.Fatalf("body failure: %+v closed=%v", got, body.closed)
	}
	for _, origin := range []string{"", "ftp://localhost", "http://user:secret@localhost", "http://localhost/api", "http://localhost?token=secret", "http://localhost/#fragment", "http://localhost?", "http://localhost/#"} {
		got = Read(context.Background(), c, origin, "Auto-1", 0)
		if !got.Failed || got.Status != 0 || strings.Contains(got.Detail, "secret") {
			t.Errorf("origin accepted/leaked: %+v", got)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c = NewClient(1, time.Second)
	defer c.CloseIdleConnections()
	got = Read(ctx, c, "http://127.0.0.1:1", "Auto-1", 0)
	if !got.Failed || got.Incorrect {
		t.Fatalf("canceled: %+v", got)
	}
}

func TestReadCancelsInflightRequest(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	c := NewClient(1, time.Second)
	defer c.CloseIdleConnections()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan Result, 1)
	go func() { result <- Read(ctx, c, server.URL, "Auto-1", 0) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case got := <-result:
		if !got.Failed || got.Incorrect {
			t.Fatalf("canceled: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request did not honor cancellation")
	}
}
