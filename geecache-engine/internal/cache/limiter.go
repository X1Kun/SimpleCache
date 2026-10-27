package cache

import "context"

const DefaultSourcePending = 128

// One limiter is shared by all groups created by a server. The queue is bounded
// separately from executing Getters; waiting consumes the caller's deadline.
type SourceLimiter struct {
	slots   chan struct{}
	waiting chan struct{}
}

func NewSourceLimiter(limit int) *SourceLimiter {
	return NewSourceLimiterWithQueue(limit, DefaultSourcePending)
}

func NewSourceLimiterWithQueue(limit, maxPending int) *SourceLimiter {
	if limit <= 0 || maxPending < 0 {
		panic("invalid source concurrency or queue capacity")
	}
	return &SourceLimiter{slots: make(chan struct{}, limit), waiting: make(chan struct{}, maxPending)}
}
func (l *SourceLimiter) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case l.slots <- struct{}{}:
		return nil
	default:
	}
	select {
	case l.waiting <- struct{}{}:
		defer func() { <-l.waiting }()
	default:
		return ErrOverloaded
	}
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (l *SourceLimiter) release() { <-l.slots }
