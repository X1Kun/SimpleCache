package cache

import (
	"context"
	"time"
)

type Options struct {
	TTL           time.Duration
	KnownKeys     []string
	RouteTimeout  time.Duration
	SourceTimeout time.Duration
	Limiter       *SourceLimiter
	Peers         PeerPicker
	Lifecycle     context.Context
	Observer      Observer
	// EpochBuckets defaults to 256. One reproduces the former group-wide
	// invalidation barrier for controlled diagnostic comparisons.
	EpochBuckets    int
	HotCacheBytes   int64
	HotTTL          time.Duration
	MaxMissWaiters  int
	CleanupInterval time.Duration
	CleanupBatch    int
}

func normalizeOptions(opts Options, capacity int64) Options {
	if opts.TTL < 0 || opts.RouteTimeout < 0 || opts.SourceTimeout < 0 {
		panic("negative cache duration")
	}
	if opts.TTL == 0 {
		opts.TTL = time.Minute
	}
	if opts.RouteTimeout == 0 {
		opts.RouteTimeout = 1200 * time.Millisecond
	}
	if opts.SourceTimeout == 0 {
		opts.SourceTimeout = 800 * time.Millisecond
	}
	if opts.Lifecycle == nil {
		opts.Lifecycle = context.Background()
	}
	if opts.Limiter == nil {
		opts.Limiter = NewSourceLimiter(32)
	}
	if opts.EpochBuckets == 0 {
		opts.EpochBuckets = 256
	}
	if opts.EpochBuckets < 1 || opts.EpochBuckets > 256 || opts.EpochBuckets&(opts.EpochBuckets-1) != 0 {
		panic("epoch buckets must be a power of two between 1 and 256")
	}
	if opts.HotCacheBytes < 0 || opts.HotCacheBytes >= capacity || opts.HotTTL < 0 {
		panic("invalid hot-cache budget or TTL")
	}
	if opts.HotTTL == 0 {
		opts.HotTTL = time.Second
	}
	opts.HotTTL = min(opts.HotTTL, opts.TTL, time.Second)
	if opts.MaxMissWaiters == 0 {
		opts.MaxMissWaiters = DefaultMissWaiters
	}
	if opts.CleanupInterval == 0 {
		opts.CleanupInterval = time.Second
	}
	if opts.CleanupBatch == 0 {
		opts.CleanupBatch = 128
	}
	if opts.MaxMissWaiters < 1 || opts.CleanupInterval < 0 || opts.CleanupBatch < 1 {
		panic("invalid admission or cleanup options")
	}
	return opts
}
