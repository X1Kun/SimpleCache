package cache

import (
	"container/list"
	"strings"
)

// DefaultVersionRecords bounds independent version metadata per Group. Keys
// already obey the 256-byte key bound. This logical bound is not an RSS quota.
const DefaultVersionRecords = 1024

type versionRecord struct {
	key     string
	version int64
}
type versionFloors struct {
	capacity  int
	entries   map[string]*list.Element
	advanced  list.List
	evictions uint64
}

// All version/epoch operations share epochMu; lock order is epochMu -> cache.mu.
func (g *Group) knownVersionLocked(key string) int64 {
	version := int64(0)
	if e := g.floors.entries[key]; e != nil {
		version = e.Value.(versionRecord).version
	}
	version = max(version, g.mainCache.version(key))
	if g.opts.HotCacheBytes > 0 {
		version = max(version, g.hotCache.version(key))
	}
	return version
}
func (g *Group) knownVersion(key string) int64 {
	g.epochMu.Lock()
	defer g.epochMu.Unlock()
	return g.knownVersionLocked(key)
}

func (g *Group) rememberVersionLocked(key string, version int64) {
	if version <= 0 {
		return
	}
	f := &g.floors
	if e := f.entries[key]; e != nil {
		if version > e.Value.(versionRecord).version {
			record := e.Value.(versionRecord)
			record.version = version
			e.Value = record
			f.advanced.MoveToBack(e)
		}
		return // Replayed notifications and reads do not prolong retention.
	}
	if f.entries == nil {
		f.entries = make(map[string]*list.Element)
	}
	if len(f.entries) == f.capacity {
		old := f.advanced.Front()
		forgotten := old.Value.(versionRecord).key
		delete(f.entries, forgotten)
		f.advanced.Remove(old)
		f.evictions++
		// Retiring a floor must also retire every pre-retirement flight in that
		// stripe; forgetting the floor alone could allow its old work to refill.
		g.epochs[g.epochBucket(forgotten)]++
	}
	// Request paths/query values can be slices of a much larger URI. Retain
	// only the bounded key bytes, including when later advancing this record.
	owned := strings.Clone(key)
	f.entries[owned] = f.advanced.PushBack(versionRecord{owned, version})
}

// InvalidateVersion accepts the committed source generation; zero forces a
// legacy/manual invalidation without claiming a newly committed generation.
// Deletion is a normal increasing generation, not a version reset.
func (g *Group) InvalidateVersion(key string, version int64) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if version < 0 {
		return ErrInvalidVersion
	}
	g.epochMu.Lock()
	known := g.knownVersionLocked(key)
	g.rememberVersionLocked(key, max(known, version))
	result := "ignored"
	if version == 0 || version > known {
		result = "applied"
		g.epochs[g.epochBucket(key)]++
	}
	minimum := max(known, version)
	if version == 0 || g.mainCache.version(key) < minimum {
		g.mainCache.remove(key)
	}
	if g.opts.HotCacheBytes > 0 && (version == 0 || g.hotCache.version(key) < minimum) {
		g.hotCache.remove(key)
	}

	g.epochMu.Unlock()
	g.observe("invalidate", "ok", 0)
	if version > 0 {
		g.observe("version_event", result, 0)
	}
	return nil
}
