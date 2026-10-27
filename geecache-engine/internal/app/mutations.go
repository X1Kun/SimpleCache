package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/telemetry"
)

// MutableSource requires atomic source-side preconditions. Expected version zero
// means create-if-absent for PutIfVersion; successful mutations return a positive
// version that survives delete/recreate. Demo sources remain read-only.
type MutableSource interface {
	cache.Getter
	cache.VersionedGetter
	PutIfVersion(context.Context, string, []byte, int64) (int64, error)
	DeleteIfVersion(context.Context, string, int64) (int64, error)
}

type APIOptions struct {
	Store               MutableSource
	Router              *peer.Router
	Token               string
	Lifecycle           context.Context
	MaxConcurrentWrites int
	writeSlots          chan struct{}
}

const DefaultWriteConcurrency = 16

type mutationResponse struct {
	// nil means unknown after a source error, not a guaranteed rollback.
	SourceCommitted *bool                    `json:"source_committed"`
	SourceVersion   *int64                   `json:"source_version,omitempty"`
	Invalidation    *peer.InvalidationReport `json:"invalidation,omitempty"`
	Error           string                   `json:"error,omitempty"`
}

func mutate(w http.ResponseWriter, r *http.Request, group *cache.Group, opts APIOptions, metrics *telemetry.Metrics, invalidateOnly bool) (result string) {
	result = "error"
	if !peer.Authorized(r, opts.Token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	key := r.URL.Query().Get("key")
	if err := cache.ValidateKey(key); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Keep body buffering and fan-out bounded independently of read misses.
	select {
	case opts.writeSlots <- struct{}{}:
		if metrics != nil {
			metrics.WriteInflight(1)
		}
		defer func() {
			<-opts.writeSlots
			if metrics != nil {
				metrics.WriteInflight(-1)
			}
		}()
	default:
		if metrics != nil {
			metrics.Observe("rejected", "write", 0)
		}
		w.Header().Set("Retry-After", "1")
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, cache.ErrOverloaded.Error(), http.StatusServiceUnavailable)
		return "overloaded"
	}
	var value []byte
	if r.Method == http.MethodPut {
		body := http.MaxBytesReader(w, r.Body, cache.MaxValueBytes)
		defer body.Close()
		var err error
		value, err = io.ReadAll(body)
		if err != nil {
			http.Error(w, "invalid or oversized request body", http.StatusRequestEntityTooLarge)
			return
		}
		if !json.Valid(value) {
			http.Error(w, "product detail must be valid JSON", http.StatusBadRequest)
			return
		}
	}
	if r.Context().Err() != nil {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	var expected int64
	if !invalidateOnly {
		var status int
		var err error
		expected, status, err = mutationPrecondition(r)
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}
	}
	lifecycle := opts.Lifecycle
	if lifecycle == nil {
		lifecycle = context.Background()
	}
	// Once a mutation starts, a disconnected caller must not cancel the
	// post-commit invalidation. Shutdown still bounds this work.
	work, cancel := context.WithTimeout(lifecycle, 3*time.Second)
	defer cancel()
	var writeErr error
	var response mutationResponse
	if !invalidateOnly {
		writeStart := time.Now()
		source, stop := context.WithTimeout(work, 800*time.Millisecond)
		var version int64
		if r.Method == http.MethodPut {
			version, writeErr = opts.Store.PutIfVersion(source, key, value, expected)
		} else {
			version, writeErr = opts.Store.DeleteIfVersion(source, key, expected)
		}
		stop()
		if writeErr == nil {
			committed := true
			response.SourceCommitted = &committed
			response.SourceVersion = &version
		}
		if metrics != nil {
			metrics.Observe("write", cache.Result(writeErr), time.Since(writeStart))
		}
		if errors.Is(writeErr, cache.ErrPreconditionFailed) {
			// This is a known failed condition, not an ambiguous commit. No
			// source mutation occurred and no cluster invalidation is needed.
			committed := false
			response.SourceCommitted = &committed
			response.Error = "precondition failed; refresh the value before retrying"
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusPreconditionFailed)
			_ = json.NewEncoder(w).Encode(response)
			return "precondition_failed"
		}
	}
	// Even an error can mean an ambiguous commit. Attempt invalidation then too.
	broadcastStart := time.Now()
	report, err := opts.Router.Invalidate(work, group, key)
	if err != nil {
		if metrics != nil {
			metrics.Observe("broadcast", "error", time.Since(broadcastStart))
		}
		http.Error(w, err.Error(), peer.Status(err))
		return
	}
	response.Invalidation = &report
	status := http.StatusOK
	broadcastResult := "complete"
	if !report.Complete {
		status = http.StatusAccepted
		broadcastResult = "partial"
	}
	if metrics != nil {
		metrics.Observe("broadcast", broadcastResult, time.Since(broadcastStart))
	}
	if writeErr != nil {
		status = peer.Status(writeErr)
		response.Error = "source write failed; commit status is unknown; verify the source before retrying"
	}
	w.Header().Set("Content-Type", "application/json")
	if writeErr == nil && r.Method == http.MethodPut && response.SourceVersion != nil {
		w.Header().Set("ETag", versionETag(*response.SourceVersion))
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
	result = cache.Result(writeErr)
	if writeErr == nil && !report.Complete {
		result = "partial"
	}
	return
}
