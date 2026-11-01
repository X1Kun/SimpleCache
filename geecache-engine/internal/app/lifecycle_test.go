package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
)

func TestShutdownDrainsActiveRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	g := cache.NewGroup("shutdown", 1024, cache.GetterFunc(func(ctx context.Context, _ string) ([]byte, error) {
		close(started)
		select {
		case <-release:
			return []byte("value"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}), cache.Options{SourceTimeout: 2 * time.Second})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value, err := g.GetLocal(r.Context(), "key")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_, _ = w.Write(value.ByteSlice())
	})}
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ready atomic.Bool
	done := make(chan error, 1)
	go func() { done <- serve(ctx, []*http.Server{server}, []net.Listener{listener}, &ready) }()
	type outcome struct {
		status int
		body   string
		err    error
	}
	response := make(chan outcome, 1)
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		res, err := client.Get("http://" + listener.Addr().String())
		if err != nil {
			response <- outcome{err: err}
			return
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		response <- outcome{res.StatusCode, string(body), err}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not enter source")
	}
	cancel()
	deadline := time.After(time.Second)
	for ready.Load() {
		select {
		case <-deadline:
			t.Fatal("shutdown did not clear readiness")
		case <-time.After(time.Millisecond):
		}
	}
	unblock()
	select {
	case got := <-response:
		if got.err != nil || got.status != 200 || got.body != "value" {
			t.Fatalf("active request aborted: %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request did not complete")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}

func TestBindServersClosesEarlierListenersOnPartialFailure(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	_ = reservation.Close()
	listeners, err := bindServers([]*http.Server{{Addr: address}, {Addr: address}})
	if err == nil || listeners != nil {
		for _, listener := range listeners {
			_ = listener.Close()
		}
		t.Fatal("duplicate binding unexpectedly succeeded")
	}
	reopened, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal("partial binding leaked the first listener", err)
	}
	_ = reopened.Close()
}

func TestDataServerTimesOutBlockedResponse(t *testing.T) {
	finished := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := make([]byte, 1<<20)
		for i := 0; i < 64; i++ {
			if _, err := w.Write(chunk); err != nil {
				finished <- err
				return
			}
		}
		finished <- nil
	})
	server := httptest.NewUnstartedServer(handler)
	server.Config = newDataServer("", handler)
	server.Start()
	defer server.Close()
	conn, err := net.DialTCP("tcp", nil, server.Listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatal("expected response write timeout", err)
		}
	case <-time.After(responseWriteTimeout + 3*time.Second):
		t.Fatal("blocked response outlived write deadline")
	}
}
