package peer

import (
	"context"
	"errors"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"net/http"
)

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
