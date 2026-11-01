package cache

import (
	"context"
	"errors"
)

const MaxValueBytes = 1 << 20

var (
	ErrNotFound           = errors.New("key not found")
	ErrInvalidKey         = errors.New("key must contain 1 to 256 bytes")
	ErrValueTooLarge      = errors.New("value exceeds 1MiB")
	ErrPreconditionFailed = errors.New("source version precondition failed")
	ErrInvalidVersion     = errors.New("invalid source invalidation version")
	ErrStaleVersion       = errors.New("value is older than an observed source version")
	ErrOverloaded         = errors.New("cache admission capacity exhausted")
)

func Result(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrOverloaded):
		return "overloaded"
	case errors.Is(err, ErrStaleVersion):
		return "stale_version"
	case errors.Is(err, ErrPreconditionFailed):
		return "precondition_failed"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	default:
		return "error"
	}
}

func ValidateKey(key string) error {
	if len(key) == 0 || len(key) > 256 {
		return ErrInvalidKey
	}
	return nil
}
