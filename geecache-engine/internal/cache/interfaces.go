package cache

import (
	"context"
	"time"
)

type Getter interface {
	Get(context.Context, string) ([]byte, error)
}

// VersionedGetter reads value and version from the same source snapshot.
// It is optional for immutable/demo sources.
type VersionedGetter interface {
	GetVersioned(context.Context, string) (SourceValue, error)
}

type SourceValue struct {
	Value   []byte
	Version int64
}

type GetterFunc func(context.Context, string) ([]byte, error)

type PeerGetter interface {
	Get(context.Context, string, string) ([]byte, error)
}

// ExpiringPeerGetter is optional. Legacy responses remain readable but cannot
// be mirrored without an owner-provided remaining lifetime.
type ExpiringPeerGetter interface {
	GetWithExpiry(context.Context, string, string) (PeerValue, error)
}

type PeerValue struct {
	Value     []byte
	ExpiresAt time.Time
	Version   int64
}

type PeerPicker interface {
	PickPeer(string) (PeerGetter, bool)
}

// Observer is implemented by the process metrics package; the cache owns no registry.
type Observer interface {
	Observe(operation, result string, elapsed time.Duration)
	Inflight(delta float64)
}

func (f GetterFunc) Get(ctx context.Context, key string) ([]byte, error) { return f(ctx, key) }
