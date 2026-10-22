package cache

import (
	"io"
	"time"
)

// ByteView wraps an immutable cached byte slice.
type ByteView struct {
	b         []byte
	expiresAt time.Time
}

// RemainingTTL is zero for uncacheable/legacy values and expired results.
func (bv ByteView) RemainingTTL() time.Duration {
	if bv.expiresAt.IsZero() {
		return 0
	}
	return max(0, time.Until(bv.expiresAt))
}

// Len implements the LRU Value interface.
func (bv ByteView) Len() int {
	return len(bv.b)
}

// ByteSlice returns a copy.
func (bv ByteView) ByteSlice() []byte {
	return cloneBytes(bv.b)
}

// WriteTo implements io.WriterTo without making a defensive slice copy.
// As required by io.Writer, w must not modify or retain the supplied bytes.
func (bv ByteView) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(bv.b)
	if err == nil && n < len(bv.b) {
		err = io.ErrShortWrite
	}
	return int64(n), err
}

// String returns the cached bytes as a string.
func (bv ByteView) String() string {
	return string(bv.b)
}

// cloneBytes copies the input slice.
func cloneBytes(input []byte) []byte {
	output := make([]byte, len(input))
	copy(output, input)
	return output
}
