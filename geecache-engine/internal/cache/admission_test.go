package cache

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func waitAdmission(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !ready() {
		select {
		case <-deadline.C:
			t.Fatal("admission state did not converge")
		case <-tick.C:
		}
	}
}

func TestSourceQueueCancellationAndRecovery(t *testing.T) {
	l := NewSourceLimiterWithQueue(1, 1)
	if err := l.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.acquire(ctx) }()
	waitAdmission(t, func() bool { return len(l.waiting) == 1 })
	bounded, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if err := l.acquire(bounded); !errors.Is(err, ErrOverloaded) {
		t.Fatalf("full queue: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(l.waiting) != 0 || len(l.slots) != 1 {
		t.Fatal("canceled waiter leaked capacity")
	}
	l.release()
	if err := l.acquire(bounded); err != nil {
		t.Fatal("did not recover", err)
	}
	l.release()
	zero := NewSourceLimiterWithQueue(1, 0)
	if err := zero.acquire(bounded); err != nil {
		t.Fatal(err)
	}
	if err := zero.acquire(bounded); !errors.Is(err, ErrOverloaded) {
		t.Fatal("zero queue admitted waiting", err)
	}
	zero.release()
}

func TestMissAdmissionPreservesHitsAndPeerLane(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{}, 1)
	defer close(release)
	g := NewGroup("admit", 1<<20, GetterFunc(func(ctx context.Context, key string) ([]byte, error) {
		if key == "cold" {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return []byte(key), nil
	}), Options{MaxMissWaiters: 1, Limiter: NewSourceLimiterWithQueue(2, 0), SourceTimeout: 3 * time.Second, RouteTimeout: 4 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := g.Get(ctx, "warm"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := g.Get(ctx, "cold"); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := g.Get(ctx, "excess"); !errors.Is(err, ErrOverloaded) {
		t.Fatal("excess miss was not rejected", err)
	}
	if v, err := g.Get(ctx, "warm"); err != nil || v.String() != "warm" {
		t.Fatal("hit was rejected", err)
	}
	if v, err := g.GetLocal(ctx, "peer-cold"); err != nil || v.String() != "peer-cold" {
		t.Fatal("API waiters blocked the Peer lane", err)
	}
	release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := g.Get(ctx, "excess"); err != nil {
		t.Fatal("miss did not recover", err)
	}
	if stats := g.AdmissionStats(); stats != (AdmissionStats{}) {
		t.Fatal("admission capacity leaked", stats)
	}
}

func TestCanceledCallerCannotBypassRouteWorkBound(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{}, 1)
	defer close(release)
	var calls atomic.Int32
	g := NewGroup("detached", 1<<20, GetterFunc(func(ctx context.Context, key string) ([]byte, error) {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			return []byte(key), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}), Options{MaxMissWaiters: 1, SourceTimeout: 3 * time.Second, RouteTimeout: 4 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	caller, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, err := g.Get(caller, "same"); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	stats := g.AdmissionStats()
	if stats.APIWaiters != 0 || stats.RouteTasks != 1 {
		t.Fatal("shared work was not accounted separately", stats)
	}
	if _, err := g.Get(ctx, "different"); !errors.Is(err, ErrOverloaded) {
		t.Fatal("new task bypassed route bound", err)
	}
	go func() { _, err := g.Get(ctx, "same"); done <- err }()
	waitAdmission(t, func() bool { return g.AdmissionStats().APIWaiters == 1 })
	release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal("same-key waiter could not reuse a full route slot", err)
	}
	if calls.Load() != 1 || g.AdmissionStats() != (AdmissionStats{}) {
		t.Fatal("duplicate work or leaked capacity", calls.Load(), g.AdmissionStats())
	}
}

type entryEvents struct{ lookups chan string }

func (o entryEvents) Observe(operation, result string, _ time.Duration) {
	if operation == "lookup_api" || operation == "lookup_peer" {
		o.lookups <- operation
	}
}
func (entryEvents) Inflight(float64) {}

func TestReadEntriesOwnExactlyOneWaiterLane(t *testing.T) {
	for _, name := range []string{"api", "peer", "fallback"} {
		t.Run(name, func(t *testing.T) {
			events := entryEvents{lookups: make(chan string, 4)}
			var g *Group
			observed := make(chan AdmissionStats, 1)
			g = NewGroup(name, 1024, GetterFunc(func(context.Context, string) ([]byte, error) {
				observed <- g.AdmissionStats()
				return []byte("value"), nil
			}), Options{MaxMissWaiters: 1, Observer: events})
			get := g.GetDirect
			wantAPI, wantPeer, wantRoute := 1, 0, 0
			wantLabel := "lookup_api"
			if name == "peer" {
				get = g.GetLocal
				wantAPI, wantPeer = 0, 1
				wantLabel = "lookup_peer"
			}
			if name == "fallback" {
				get = g.Get
				wantRoute = 1
			}
			if _, err := get(context.Background(), "key"); err != nil {
				t.Fatal("fallback acquired a duplicate lane", err)
			}
			state := <-observed
			if state.APIWaiters != wantAPI || state.PeerWaiters != wantPeer || state.RouteTasks != wantRoute {
				t.Fatal(state)
			}
			if len(events.lookups) != 1 || <-events.lookups != wantLabel {
				t.Fatal("lookup metrics changed across entry types")
			}
			state = g.AdmissionStats()
			if state.APIWaiters != 0 || state.PeerWaiters != 0 || state.RouteTasks != 0 {
				t.Fatal("waiter leaked", state)
			}
		})
	}
}
