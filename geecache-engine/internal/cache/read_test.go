package cache

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadsShareSourceFlightAndAdmission(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	g := NewGroup(t.Name(), 1024, GetterFunc(func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		close(started)
		<-release
		return []byte("ok"), nil
	}), Options{MaxMissWaiters: 1, SourceTimeout: time.Second})
	done := make(chan error, 1)
	go func() { _, err := g.Get(context.Background(), "key"); done <- err }()
	<-started
	if _, err := g.Get(context.Background(), "other"); err != ErrOverloaded {
		t.Fatalf("admission: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if v, err := g.Get(context.Background(), "key"); err != nil || v.String() != "ok" || calls.Load() != 1 {
		t.Fatal(v, err, calls.Load())
	}
}
