package cache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

type versionGetterFunc func(context.Context, string) (SourceValue, error)

func (f versionGetterFunc) Get(ctx context.Context, key string) ([]byte, error) {
	v, e := f(ctx, key)
	return v.Value, e
}
func (f versionGetterFunc) GetVersioned(ctx context.Context, key string) (SourceValue, error) {
	return f(ctx, key)
}
func sourceValue(version int64) SourceValue {
	return SourceValue{Value: []byte(fmt.Sprint(version)), Version: version}
}

func TestVersionEventsPreserveNewerValuesAndForceKeepsFloor(t *testing.T) {
	ctx := context.Background()
	var version atomic.Int64
	version.Store(4)
	var loads atomic.Int32
	g := NewGroup("versions", 1024, versionGetterFunc(func(context.Context, string) (SourceValue, error) {
		loads.Add(1)
		return sourceValue(version.Load()), nil
	}), Options{})
	if _, err := g.GetDirect(ctx, "key"); err != nil {
		t.Fatal(err)
	}
	epoch := g.currentEpoch("key")
	for _, v := range []int64{3, 4, 2, 4} {
		if err := g.InvalidateVersion("key", v); err != nil {
			t.Fatal(err)
		}
	}
	if g.currentEpoch("key") != epoch || g.knownVersion("key") != 4 {
		t.Fatal("old events changed the fence or decreased the floor")
	}
	if v, err := g.GetDirect(ctx, "key"); err != nil || v.Version() != 4 || loads.Load() != 1 {
		t.Fatal(v, err, loads.Load())
	}
	if err := g.Invalidate("key"); err != nil {
		t.Fatal(err)
	}
	if g.Stats().Entries != 0 || g.knownVersion("key") != 4 {
		t.Fatal("force invalidation forgot known source version")
	}
	version.Store(3)
	if _, err := g.GetDirect(ctx, "key"); !errors.Is(err, ErrStaleVersion) {
		t.Fatal("accepted an old source after force invalidation", err)
	}
	version.Store(5)
	if v, err := g.GetDirect(ctx, "key"); err != nil || v.Version() != 5 {
		t.Fatal(v, err)
	}
	for _, v := range []int64{1, 4, 3} {
		_ = g.InvalidateVersion("key", v)
	}
	if v, err := g.GetDirect(ctx, "key"); err != nil || v.Version() != 5 {
		t.Fatal("replayed delete/update evicted a recreated generation", v, err)
	}
}

func TestVersionedNotificationRejectsLateFillAndSeparatesFlights(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	g := NewGroup("race", 1024, versionGetterFunc(func(ctx context.Context, _ string) (SourceValue, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
				return sourceValue(1), nil
			case <-ctx.Done():
				return SourceValue{}, ctx.Err()
			}
		}
		return sourceValue(2), nil
	}), Options{SourceTimeout: 2 * time.Second})
	old := make(chan error, 1)
	go func() { _, e := g.GetDirect(ctx, "key"); old <- e }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_ = g.InvalidateVersion("key", 2)
	if v, err := g.GetDirect(ctx, "key"); err != nil || v.Version() != 2 {
		t.Fatal("new caller joined old work", v, err)
	}
	epoch := g.currentEpoch("key")
	_ = g.InvalidateVersion("key", 1)
	_ = g.InvalidateVersion("key", 2)
	if g.currentEpoch("key") != epoch {
		t.Fatal("duplicate event detached a fresh flight")
	}
	release <- struct{}{}
	if err := <-old; !errors.Is(err, ErrStaleVersion) {
		t.Fatal("late old source result accepted", err)
	}
	if v, err := g.GetDirect(ctx, "key"); err != nil || v.Version() != 2 || calls.Load() != 2 {
		t.Fatal(v, err, calls.Load())
	}
}

func TestDuplicateEventDoesNotDetachLoadingCurrentVersion(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	g := NewGroup("duplicate", 1024, versionGetterFunc(func(ctx context.Context, _ string) (SourceValue, error) {
		close(started)
		select {
		case <-release:
			return sourceValue(2), nil
		case <-ctx.Done():
			return SourceValue{}, ctx.Err()
		}
	}), Options{SourceTimeout: 2 * time.Second})
	_ = g.InvalidateVersion("key", 2)
	done := make(chan error, 1)
	go func() { _, e := g.GetDirect(context.Background(), "key"); done <- e }()
	<-started
	epoch := g.currentEpoch("key")
	_ = g.InvalidateVersion("key", 2)
	_ = g.InvalidateVersion("key", 1)
	if g.currentEpoch("key") != epoch {
		t.Fatal("old event changed flight generation")
	}
	release <- struct{}{}
	if e := <-done; e != nil || g.Stats().Entries != 1 {
		t.Fatal("valid in-flight fill discarded", e, g.Stats())
	}
}

func TestFloorRetirementFencesWorkAndRetainsItsCapturedMinimum(t *testing.T) {
	for _, loaded := range []int64{1, 2} {
		t.Run(fmt.Sprint(loaded), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			g := NewGroup("retirement", 1024, versionGetterFunc(func(ctx context.Context, _ string) (SourceValue, error) {
				close(started)
				select {
				case <-release:
					return sourceValue(loaded), nil
				case <-ctx.Done():
					return SourceValue{}, ctx.Err()
				}
			}), Options{SourceTimeout: 2 * time.Second})
			g.floors.capacity = 2
			_ = g.InvalidateVersion("key", 2)
			done := make(chan error, 1)
			go func() { _, e := g.GetDirect(context.Background(), "key"); done <- e }()
			<-started
			before := g.currentEpoch("key")
			// Avoid stripe collisions: only retirement itself may advance this key's epoch.
			others := []string{}
			for i := 0; len(others) < 2; i++ {
				k := fmt.Sprintf("other-%d", i)
				if g.epochBucket(k) != g.epochBucket("key") {
					others = append(others, k)
				}
			}
			for _, k := range others {
				_ = g.InvalidateVersion(k, 1)
			}
			if g.knownVersion("key") != 0 || g.currentEpoch("key") != before+1 || g.Stats().VersionRecords != 2 || g.Stats().VersionEvictions != 1 {
				t.Fatal("floor retirement did not fence old work", g.Stats())
			}
			release <- struct{}{}
			err := <-done
			if loaded == 1 && !errors.Is(err, ErrStaleVersion) {
				t.Fatal("forgot the floor captured by active work", err)
			}
			if loaded == 2 && err != nil {
				t.Fatal(err)
			}
			if g.Stats().Entries != 0 {
				t.Fatal("retired flight refilled cache")
			}
		})
	}
}

func TestFloorCapacityAndDuplicateRetentionOrder(t *testing.T) {
	g := NewGroup("bound", 1024, GetterFunc(func(context.Context, string) ([]byte, error) { return nil, ErrNotFound }), Options{})
	_ = g.InvalidateVersion("oldest", 7)
	for i := 0; i < DefaultVersionRecords-1; i++ {
		_ = g.InvalidateVersion(fmt.Sprint(i), 1)
	}
	_ = g.InvalidateVersion("oldest", 7)
	_ = g.InvalidateVersion("oldest", 6)
	_ = g.InvalidateVersion("last", 1)
	if g.knownVersion("oldest") != 0 || g.Stats().VersionRecords != DefaultVersionRecords || g.Stats().VersionEvictions != 1 {
		t.Fatal("unbounded records or replay pinned an old floor", g.Stats())
	}
	if err := g.InvalidateVersion("bad", -1); !errors.Is(err, ErrInvalidVersion) {
		t.Fatal(err)
	}
}

func TestVersionComparisonDoesNotRefreshCacheRecencyOrTTL(t *testing.T) {
	g := NewGroup("peek", 4, versionGetterFunc(func(context.Context, string) (SourceValue, error) {
		return SourceValue{Value: []byte("x"), Version: 1}, nil
	}), Options{})
	for _, k := range []string{"a", "b"} {
		_, _ = g.GetDirect(context.Background(), k)
	}
	before, _ := g.mainCache.lru.Peek("a")
	deadline := before.(ByteView).expiresAt
	_ = g.InvalidateVersion("a", 1)
	after, _ := g.mainCache.lru.Peek("a")
	if after.(ByteView).expiresAt != deadline {
		t.Fatal("notification renewed TTL")
	}
	_, _ = g.GetDirect(context.Background(), "c")
	if g.mainCache.version("a") != 0 || g.mainCache.version("b") != 1 {
		t.Fatal("version comparison changed recency")
	}
}

type oldVersionPeer struct {
	version int64
	err     error
}

func (p oldVersionPeer) PickPeer(string) (PeerGetter, bool) { return p, true }
func (p oldVersionPeer) Get(ctx context.Context, g, k string) ([]byte, error) {
	v, e := p.GetWithExpiry(ctx, g, k)
	return v.Value, e
}
func (p oldVersionPeer) GetWithExpiry(context.Context, string, string) (PeerValue, error) {
	return PeerValue{Value: []byte("old"), Version: p.version, ExpiresAt: time.Now().Add(time.Minute)}, p.err
}

func TestKnownFloorRejectsOldAndUnversionedPeersWithoutHotCopies(t *testing.T) {
	for _, hot := range []int64{0, 128} {
		for _, pv := range []int64{0, 1} {
			t.Run(fmt.Sprintf("hot-%d-peer-%d", hot, pv), func(t *testing.T) {
				g := NewGroup("peer-floor", 1024, versionGetterFunc(func(context.Context, string) (SourceValue, error) { return sourceValue(2), nil }), Options{Peers: oldVersionPeer{version: pv}, HotCacheBytes: hot})
				_ = g.InvalidateVersion("key", 2)
				v, e := g.Get(context.Background(), "key")
				if e != nil || v.Version() != 2 || g.Stats().HotEntries != 0 {
					t.Fatal("stale Peer did not fall back to primary", v, e, g.Stats())
				}
			})
		}
	}
	g := NewGroup("peer-miss", 1024, versionGetterFunc(func(context.Context, string) (SourceValue, error) { return sourceValue(3), nil }), Options{Peers: oldVersionPeer{err: ErrNotFound}})
	_ = g.InvalidateVersion("key", 3)
	if v, e := g.Get(context.Background(), "key"); e != nil || v.Version() != 3 {
		t.Fatal("unversioned Peer absence hid a known generation", v, e)
	}
}

type heldOldPeer struct{ started, release chan struct{} }

func (p heldOldPeer) PickPeer(string) (PeerGetter, bool) { return p, true }
func (p heldOldPeer) Get(ctx context.Context, _, _ string) ([]byte, error) {
	close(p.started)
	select {
	case <-p.release:
		return []byte("old"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestRetiredRouteFloorSurvivesFallbackAndDoesNotJoinWeakerWork(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint(pending), func(t *testing.T) {
			peer := heldOldPeer{make(chan struct{}), make(chan struct{}, 1)}
			defer close(peer.release)
			sourceStarted, releaseSource := make(chan struct{}), make(chan struct{}, 1)
			defer close(releaseSource)
			var calls atomic.Int32
			g := NewGroup("inherited-floor", 1024, versionGetterFunc(func(ctx context.Context, _ string) (SourceValue, error) {
				if calls.Add(1) == 1 {
					close(sourceStarted)
					if pending {
						select {
						case <-releaseSource:
						case <-ctx.Done():
							return SourceValue{}, ctx.Err()
						}
					}
					return sourceValue(1), nil
				}
				return sourceValue(2), nil
			}), Options{Peers: peer, SourceTimeout: 2 * time.Second, RouteTimeout: 3 * time.Second})
			g.floors.capacity = 2
			_ = g.InvalidateVersion("key", 2)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			strong := make(chan error, 1)
			go func() {
				v, e := g.Get(ctx, "key")
				if e == nil && v.Version() != 2 {
					e = fmt.Errorf("inherited floor lost: %d", v.Version())
				}
				strong <- e
			}()
			<-peer.started
			added := 0
			for i := 0; added < 2; i++ {
				key := fmt.Sprintf("other-%d", i)
				if g.epochBucket(key) != g.epochBucket("key") {
					_ = g.InvalidateVersion(key, 1)
					added++
				}
			}
			if g.knownVersion("key") != 0 {
				t.Fatal("fixture did not retire floor")
			}
			weak := make(chan error, 1)
			go func() { _, e := g.GetDirect(ctx, "key"); weak <- e }()
			<-sourceStarted
			if !pending {
				if e := <-weak; e != nil {
					t.Fatal(e)
				}
			}
			peer.release <- struct{}{}
			select {
			case e := <-strong:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(time.Second):
				t.Fatal("strong fallback joined weaker source work")
			}
			if pending {
				releaseSource <- struct{}{}
				if e := <-weak; !errors.Is(e, ErrStaleVersion) {
					t.Fatal("weak result overwrote newer cache", e)
				}
			}
			if g.mainCache.version("key") != 2 || calls.Load() != 2 {
				t.Fatal(g.Stats(), calls.Load())
			}
		})
	}
}

func TestVersionMetadataOwnsOnlyTheBoundedKeyBytes(t *testing.T) {
	backing := "key" + strings.Repeat("x", 1<<20)
	key := backing[:3]
	g := NewGroup("key-storage", 1024, GetterFunc(func(context.Context, string) ([]byte, error) { return nil, ErrNotFound }), Options{})
	_ = g.InvalidateVersion(key, 1)
	_ = g.InvalidateVersion(key, 2)
	record := g.floors.entries[key].Value.(versionRecord)
	if unsafe.StringData(record.key) == unsafe.StringData(key) {
		t.Fatal("version record retained the request's oversized backing string")
	}
	for stored := range g.floors.entries {
		if unsafe.StringData(stored) == unsafe.StringData(key) {
			t.Fatal("version index retained request storage")
		}
	}
}
