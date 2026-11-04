// Package outbox delivers durable source-version events at least once.
package outbox

import (
	"context"
	"time"
)

const (
	LeaseDuration       = 10 * time.Second
	storageTimeout      = time.Second
	deliveryTimeout     = 2 * time.Second
	pollInterval        = time.Second
	maintenanceInterval = 30 * time.Second
	Retention           = 7 * 24 * time.Hour
)

type Event struct {
	ID      int64
	Key     string
	Version int64
	Attempt int64
}
type Stats struct {
	PurgeEligible int64
	TableBytes    int64
	Pending       int64
	OldestAge     time.Duration
}

// Attempt is a monotonic lease token. Finish must reject expired/reclaimed work.
type Queue interface {
	Claim(context.Context, time.Duration) (Event, bool, error)
	Finish(context.Context, Event, bool, time.Duration) (bool, error)
	Stats(context.Context) (Stats, error)
	PurgeDelivered(context.Context, time.Duration, int) (int64, error)
}
type Sender func(context.Context, Event) error
type Observer interface {
	OutboxEvent(operation, result string)
	OutboxBacklog(pending int64, oldest time.Duration)
}
type Worker struct {
	Queue    Queue
	Send     Sender
	Observer Observer
}

func (w Worker) observe(operation, result string) {
	if w.Observer != nil {
		w.Observer.OutboxEvent(operation, result)
	}
}
func retryDelay(attempt int64) time.Duration {
	delay := time.Second
	for n := int64(1); n < attempt && delay < time.Minute; n++ {
		delay *= 2
	}
	return min(delay, time.Minute)
}

// Step completes one claim/send/finish cycle. It never holds a DB transaction
// across network I/O; an unacknowledged lease is eligible again after expiration.
func (w Worker) Step(ctx context.Context) (bool, error) {
	storage, cancel := context.WithTimeout(ctx, storageTimeout)
	event, found, err := w.Queue.Claim(storage, LeaseDuration)
	cancel()
	if err != nil {
		w.observe("claim", "error")
		return false, err
	}
	if !found {
		return false, nil
	}
	w.observe("claim", "ok")
	delivery, stop := context.WithTimeout(ctx, deliveryTimeout)
	sendErr := w.Send(delivery, event)
	stop()
	result := "delivered"
	if sendErr != nil {
		result = "retry"
	}
	finish, done := context.WithTimeout(ctx, storageTimeout)
	owned, err := w.Queue.Finish(finish, event, sendErr == nil, retryDelay(event.Attempt))
	done()
	if err != nil {
		w.observe("finish", "error")
		return true, err
	}
	if !owned {
		w.observe("finish", "lease_lost")
		return true, nil
	}
	w.observe("finish", result)
	return true, nil
}
func (w Worker) maintain(ctx context.Context) {
	work, cancel := context.WithTimeout(ctx, storageTimeout)
	stats, err := w.Queue.Stats(work)
	cancel()
	if err != nil {
		w.observe("stats", "error")
	} else if w.Observer != nil {
		w.Observer.OutboxBacklog(stats.Pending, stats.OldestAge)
		if observer, ok := w.Observer.(interface{ OutboxStorage(int64, int64) }); ok {
			observer.OutboxStorage(stats.PurgeEligible, stats.TableBytes)
		}
	}
	work, cancel = context.WithTimeout(ctx, storageTimeout)
	defer cancel()
	// Share one deadline across all batches so cleanup cannot starve delivery.
	for work.Err() == nil {
		removed, err := w.Queue.PurgeDelivered(work, Retention, 1000)
		if err != nil {
			w.observe("purge", "error")
			return
		}
		w.observe("purge", "ok")
		if removed < 1000 {
			return
		}
	}
}

// Run owns no resources and returns after cancellation. App must join it before
// closing its Queue or HTTP transport. A single worker bounds per-process work.
func (w Worker) Run(ctx context.Context) {
	nextMaintenance := time.Time{}
	for ctx.Err() == nil {
		if time.Now().After(nextMaintenance) {
			w.maintain(ctx)
			nextMaintenance = time.Now().Add(maintenanceInterval)
		}
		worked, err := w.Step(ctx)
		if ctx.Err() != nil {
			return
		}
		if worked && err == nil {
			continue
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
