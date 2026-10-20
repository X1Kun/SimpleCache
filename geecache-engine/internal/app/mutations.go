package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/telemetry"
)

type MutableSource interface {
	cache.Getter
	Put(context.Context, string, []byte) error
	Delete(context.Context, string) error
}

type APIOptions struct {
	Store     MutableSource
	Router    *peer.Router
	Token     string
	Lifecycle context.Context
}

type mutationResponse struct {
	// nil means unknown after a source error, not a guaranteed rollback.
	SourceCommitted *bool                    `json:"source_committed"`
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
		source, stop := context.WithTimeout(work, 800*time.Millisecond)
		if r.Method == http.MethodPut {
			writeErr = opts.Store.Put(source, key, value)
		} else {
			writeErr = opts.Store.Delete(source, key)
		}
		stop()
		if writeErr == nil {
			committed := true
			response.SourceCommitted = &committed
		}
		if metrics != nil {
			metrics.Observe("write", cache.Result(writeErr), 0)
		}
	}
	// Even an error can mean an ambiguous commit. Attempt invalidation then too.
	report, err := opts.Router.Invalidate(work, group, key)
	if err != nil {
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
		metrics.Observe("broadcast", broadcastResult, 0)
	}
	if writeErr != nil {
		status = peer.Status(writeErr)
		response.Error = "source write failed; commit status is unknown; verify the source before retrying"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
	result = cache.Result(writeErr)
	if writeErr == nil && !report.Complete {
		result = "partial"
	}
	return
}
