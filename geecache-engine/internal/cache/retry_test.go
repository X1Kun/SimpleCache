package cache

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestStaleSourceRetryKeepsDeadlineAdmissionAndVersionFloor(t *testing.T) {
	for _, mode := range []string{"peer", "direct", "route"} {
		for _, recover := range []bool{false, true} {
			t.Run(mode+"/"+map[bool]string{false: "still-stale", true: "recovers"}[recover], func(t *testing.T) {
				var calls atomic.Int32
				var firstDeadline time.Time
				var g *Group
				g = NewGroup(t.Name(), 1024, versionGetterFunc(func(ctx context.Context, _ string) (SourceValue, error) {
					n := calls.Add(1)
					deadline, _ := ctx.Deadline()
					if n == 1 {
						firstDeadline = deadline
						_ = g.InvalidateVersion("key", 2)
						return sourceValue(1), nil
					}
					if deadline != firstDeadline {
						t.Error("retry extended source deadline")
					}
					if recover {
						return sourceValue(2), nil
					}
					return sourceValue(1), nil
				}), Options{Limiter: NewSourceLimiterWithQueue(1, 0)})
				get := g.GetDirect
				if mode == "peer" {
					get = g.GetLocal
				}
				if mode == "route" {
					get = g.Get
				}
				v, err := get(context.Background(), "key")
				if recover {
					if err != nil || v.Version() != 2 || g.Stats().Entries != 1 {
						t.Fatal(v, err, g.Stats())
					}
				} else {
					if !errors.Is(err, ErrStaleVersion) || g.Stats().Entries != 0 {
						t.Fatal(v, err, g.Stats())
					}
				}
				if calls.Load() != 2 || len(g.opts.Limiter.slots) != 0 {
					t.Fatal("unbounded retry or leaked admission", calls.Load())
				}
			})
		}
	}
}
func TestSourceRetryDoesNotRetryOtherErrorsOrCancellation(t *testing.T) {
	for _, failure := range []error{ErrOverloaded, ErrNotFound, context.DeadlineExceeded} {
		var calls atomic.Int32
		g := NewGroup("errors", 1024, GetterFunc(func(context.Context, string) ([]byte, error) { calls.Add(1); return nil, failure }), Options{})
		if _, err := g.GetDirect(context.Background(), "key"); !errors.Is(err, failure) || calls.Load() != 1 {
			t.Fatal(err, calls.Load())
		}
	}
}

type retryFloorObserver struct{ onRetry func() }

func (o retryFloorObserver) Observe(operation, result string, _ time.Duration) {
	if operation == "source_retry" {
		o.onRetry()
	}
}
func (retryFloorObserver) Inflight(float64) {}

func TestRetryRetainsFloorObservedDuringRejection(t *testing.T) {
	for _, mode := range []string{"direct", "peer", "route"} {
		for _, retryVersion := range []int64{1, 2} {
			t.Run(fmt.Sprintf("%s/version-%d", mode, retryVersion), func(t *testing.T) {
				var calls atomic.Int32
				var g *Group
				observer := retryFloorObserver{onRetry: func() {
					// Retire version 2 precisely after rejection and before retry readBarrier.
					_ = g.InvalidateVersion("unrelated", 1)
					if g.knownVersion("key") != 0 {
						t.Error("fixture did not retire floor")
					}
				}}
				g = NewGroup(t.Name(), 1024, versionGetterFunc(func(context.Context, string) (SourceValue, error) {
					if calls.Add(1) == 1 {
						_ = g.InvalidateVersion("key", 2)
						return sourceValue(1), nil
					}
					return sourceValue(retryVersion), nil
				}), Options{Observer: observer})
				g.floors.capacity = 1
				get := g.GetDirect
				if mode == "peer" {
					get = g.GetLocal
				}
				if mode == "route" {
					get = g.Get
				}
				value, err := get(context.Background(), "key")
				if retryVersion == 1 {
					if !errors.Is(err, ErrStaleVersion) || g.Stats().Entries != 0 {
						t.Fatalf("accepted previously rejected version: value=%v err=%v stats=%+v", value, err, g.Stats())
					}
				} else if err != nil || value.Version() != 2 || g.mainCache.version("key") != 2 {
					t.Fatal(value, err, g.Stats())
				}
				if calls.Load() != 2 {
					t.Fatal("unexpected source calls", calls.Load())
				}
			})
		}
	}
}
