package cache

import (
	"context"
	"errors"

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
