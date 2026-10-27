package cache

import (
	"context"
	"time"
)

const cleanupBudget = 2 * time.Millisecond

type cleanupObserver interface {
	Cleanup(checked, removed int, elapsed time.Duration, busy bool)
}

// CleanExpired performs at most CleanupBatch checks and a 2ms best-effort work
// budget per area. Skip a busy lock instead of queueing behind foreground work.
// It does not change source versions, local epochs or key ownership.
func (g *Group) CleanExpired() {
	areas := []*cache{&g.mainCache}
	if g.opts.HotCacheBytes > 0 {
		areas = append(areas, &g.hotCache)
	}
	for _, area := range areas {
		start := time.Now()
		checked, removed, busy := area.cleanExpired(g.opts.CleanupBatch)
		if observer, ok := g.opts.Observer.(cleanupObserver); ok {
			observer.Cleanup(checked, removed, time.Since(start), busy)
		}
	}
}

func (c *cache) cleanExpired(limit int) (checked, removed int, busy bool) {
	if !c.mu.TryLock() {
		return 0, 0, true
	}
	defer c.mu.Unlock()
	if c.lru != nil {
		checked, removed = c.lru.PruneExpired(time.Now(), limit, cleanupBudget)
	}
	return
}

// RunMaintenance is explicit so constructing Groups in tests/libraries does not
// create immortal goroutines. The application starts one worker with its shared
// lifecycle; caller owns cancellation and may wait for this method to return.
func (g *Group) RunMaintenance(ctx context.Context) {
	if !g.maintenanceMu.TryLock() {
		return
	}
	defer g.maintenanceMu.Unlock()
	ticker := time.NewTicker(g.opts.CleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			g.CleanExpired()
		}
	}
}
