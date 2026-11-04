package outbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

type queueFixture struct {
	event               Event
	found               bool
	claimErr, finishErr error
	owned               bool
	finishes            int
	delivered           bool
	retry               time.Duration
}

func (q *queueFixture) Claim(ctx context.Context, _ time.Duration) (Event, bool, error) {
	return q.event, q.found, q.claimErr
}
func (q *queueFixture) Finish(ctx context.Context, _ Event, delivered bool, retry time.Duration) (bool, error) {
	q.finishes++
	q.delivered = delivered
	q.retry = retry
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return q.owned, q.finishErr
}
func (q *queueFixture) Stats(context.Context) (Stats, error) { return Stats{}, nil }
func (q *queueFixture) PurgeDelivered(context.Context, time.Duration, int) (int64, error) {
	return 0, nil
}
func TestStepOutcomesAndBoundedBackoff(t *testing.T) {
	for _, name := range []string{"empty", "claim-error", "delivered", "retry", "finish-error", "lease-lost"} {
		t.Run(name, func(t *testing.T) {
			q := &queueFixture{event: Event{ID: 1, Key: "key", Version: 2, Attempt: 3}, found: true, owned: true}
			switch name {
			case "empty":
				q.found = false
			case "claim-error":
				q.claimErr = errors.New("claim")
			case "finish-error":
				q.finishErr = errors.New("finish")
			case "lease-lost":
				q.owned = false
			}
			sent := 0
			w := Worker{Queue: q, Send: func(ctx context.Context, e Event) error {
				sent++
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("delivery has no deadline")
				}
				if name == "retry" {
					return errors.New("partial delivery")
				}
				return nil
			}}
			worked, err := w.Step(context.Background())
			if name == "empty" || name == "claim-error" {
				if worked || sent != 0 || q.finishes != 0 {
					t.Fatal(worked, sent, q.finishes)
				}
			} else {
				if !worked || sent != 1 || q.finishes != 1 || q.delivered != (name != "retry") || q.retry != 4*time.Second {
					t.Fatal(worked, sent, q)
				}
			}
			if (err != nil) != (name == "claim-error" || name == "finish-error") {
				t.Fatal(err)
			}
		})
	}
	if retryDelay(1) != time.Second || retryDelay(1000000) != time.Minute {
		t.Fatal("unbounded retry backoff")
	}
}
func TestRunStopsAndLeavesCanceledDeliveryForLeaseRecovery(t *testing.T) {
	q := &queueFixture{event: Event{ID: 1, Attempt: 1}, found: true, owned: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan struct{})
	w := Worker{Queue: q, Send: func(ctx context.Context, _ Event) error { close(entered); <-ctx.Done(); return ctx.Err() }}
	go func() { defer close(done); w.Run(ctx) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not send")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
	if q.delivered {
		t.Fatal("canceled send marked delivered")
	}
}

type purgeFixture struct {
	queueFixture
	batches   int
	remaining int64
	cancel    context.CancelFunc
}

func (q *purgeFixture) PurgeDelivered(ctx context.Context, _ time.Duration, limit int) (int64, error) {
	if _, ok := ctx.Deadline(); !ok {
		panic("cleanup missing deadline")
	}
	q.batches++
	if q.cancel != nil {
		q.cancel()
		return int64(limit), nil
	}
	n := min(q.remaining, int64(limit))
	q.remaining -= n
	return n, nil
}
func TestMaintenanceCatchesUpAndHonorsCancellation(t *testing.T) {
	q := &purgeFixture{remaining: 2501}
	Worker{Queue: q}.maintain(context.Background())
	if q.remaining != 0 || q.batches != 3 {
		t.Fatal("cleanup failed to catch up", q)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q = &purgeFixture{cancel: cancel}
	Worker{Queue: q}.maintain(ctx)
	if q.batches != 1 {
		t.Fatal("cleanup ignored cancellation", q.batches)
	}
}
