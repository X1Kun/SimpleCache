package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/telemetry"
)

func mutate(w http.ResponseWriter, r *http.Request, group *cache.Group, opts APIOptions, metrics *telemetry.Metrics, invalidateOnly bool) (result string) {
	result = "error"
	if !peer.Authorized(r, opts.Token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	key, keyErr := requestKey(r, opts.DirectReads)
	if keyErr != nil {
		http.Error(w, keyErr.Error(), http.StatusBadRequest)
		return
	}
	if err := cache.ValidateKey(key); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	version, versionErr := peer.ParseInvalidationVersion(r.Header)
	if versionErr != nil {
		http.Error(w, versionErr.Error(), http.StatusBadRequest)
		return
	}
	if !invalidateOnly && version != 0 {
		http.Error(w, "invalidation version is only accepted on POST /invalidate", http.StatusBadRequest)
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
	command := mutationCommand{key: key, value: value, expected: expected, version: version, kind: mutationNotify}
	if !invalidateOnly {
		if r.Method == http.MethodPut {
			command.kind = mutationPut
		} else {
			command.kind = mutationDelete
		}
	}
	outcome := executeMutation(opts.Lifecycle, group, opts.Store, opts.Router, command, metrics)
	response, writeErr := outcome.response, outcome.writeErr
	if errors.Is(writeErr, cache.ErrPreconditionFailed) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_ = json.NewEncoder(w).Encode(response)
		return "precondition_failed"
	}
	if outcome.broadcastErr != nil {
		http.Error(w, outcome.broadcastErr.Error(), peer.Status(outcome.broadcastErr))
		return
	}
	report := response.Invalidation
	status := http.StatusOK
	if !report.Complete {
		status = http.StatusAccepted
	}
	if writeErr != nil {
		status = peer.Status(writeErr)
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
