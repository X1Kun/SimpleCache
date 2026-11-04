package cache

import (
	"context"
	"errors"
	"time"
)

type sourceResult struct {
	value   ByteView
	minimum int64
}

// loadSource returns the highest floor it observed, including during rejection.
// Retry must retain it even if bounded metadata is retired before the next attempt.
// Its caller owns the shared deadline.
func (g *Group) loadSource(work context.Context, key string, fence readFence) (sourceResult, error) {
	waitStart := time.Now()
	err := g.opts.Limiter.acquire(work)
	slotResult := "acquired"
	if err != nil {
		slotResult = "timeout"
		if errors.Is(err, ErrOverloaded) {
			slotResult = "overloaded"
			g.observe("rejected", "source_queue", 0)
		}
		if errors.Is(err, context.Canceled) {
			slotResult = "canceled"
		}
	}
	g.observe("slot", slotResult, time.Since(waitStart))
	if err != nil {
		return sourceResult{minimum: fence.minimum}, err
	}
	defer g.opts.Limiter.release()
	if err := work.Err(); err != nil {
		return sourceResult{minimum: fence.minimum}, err
	}
	if g.opts.Observer != nil {
		g.opts.Observer.Inflight(1)
		defer g.opts.Observer.Inflight(-1)
	}
	start := time.Now()
	var value []byte
	var version int64
	if source, ok := g.getter.(VersionedGetter); ok {
		loaded, loadErr := source.GetVersioned(work, key)
		value, version, err = loaded.Value, loaded.Version, loadErr
		if err == nil && version <= 0 {
			err = errors.New("invalid source version")
		}
	} else {
		value, err = g.getter.Get(work, key)
	}
	if err == nil {
		err = work.Err()
	}
	if err == nil && len(value) > MaxValueBytes {
		err = ErrValueTooLarge
	}
	g.observe("source", Result(err), time.Since(start))
	if err != nil {
		return sourceResult{minimum: fence.minimum}, err
	}
	if err := work.Err(); err != nil {
		return sourceResult{minimum: fence.minimum}, err
	}
	view := ByteView{b: cloneBytes(value), version: version}
	// Check and insert under the same lock as invalidation. A pre-invalidation
	// reader may finish with its old value, but cannot repopulate the cache.
	g.epochMu.Lock()
	minimum := max(fence.minimum, g.knownVersionLocked(key))
	stale := version < minimum
	valid := fence.epoch == g.epochs[g.epochBucket(key)] && !stale
	if valid {
		view = g.mainCache.add(key, view, g.opts.TTL)
	}
	g.epochMu.Unlock()
	if stale {
		g.observe("version_rejected", "source", 0)
		g.observe("discard_source", "source_version", 0)
		return sourceResult{minimum: minimum}, ErrStaleVersion
	}
	if !valid {
		g.observe("discard_source", "epoch_changed", 0)
	}
	return sourceResult{value: view, minimum: minimum}, nil
}
