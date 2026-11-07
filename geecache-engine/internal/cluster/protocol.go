package cluster

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
)

const BasePath = "/_simplecache/invalidate/"

// Echoing the committed version confirms support for version-aware semantics.
const InvalidationVersionHeader = "X-SimpleCache-Invalidation-Version"

var ErrUnversionedInvalidation = errors.New("member did not acknowledge version-aware invalidation")

// ParseInvalidationVersion accepts an absent header for legacy force invalidation.
func ParseInvalidationVersion(header http.Header) (int64, error) {
	values := header.Values(InvalidationVersionHeader)
	if len(values) == 0 {
		return 0, nil
	}
	if len(values) != 1 {
		return 0, cache.ErrInvalidVersion
	}
	version, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || version <= 0 || strconv.FormatInt(version, 10) != values[0] {
		return 0, cache.ErrInvalidVersion
	}
	return version, nil
}

// Authorized disables mutations entirely when no token has been configured.
func Authorized(r *http.Request, token string) bool {
	return token != "" && subtle.ConstantTimeCompare(
		[]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) == 1
}

// Status maps notification errors to HTTP response codes.
func Status(err error) int {
	switch {
	case errors.Is(err, cache.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, cache.ErrInvalidKey), errors.Is(err, cache.ErrInvalidVersion):
		return http.StatusBadRequest
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return http.StatusGatewayTimeout
	default:
		return http.StatusServiceUnavailable
	}
}
