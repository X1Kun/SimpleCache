package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

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
	var observer mutationObserver
	if metrics != nil {
		observer = metrics
	}
	outcome := executeMutation(opts.Lifecycle, group, opts.Store, opts.Router, command, observer)
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

func versionETag(version int64) string { return `"v` + strconv.FormatInt(version, 10) + `"` }

// This API deliberately accepts one exact strong source-version ETag for
// replacement/deletion. Wildcard If-Match would permit stale overwrites.
// Zero is reserved for PUT with If-None-Match: * (create-if-absent).
func mutationPrecondition(r *http.Request) (int64, int, error) {
	matches, absent := r.Header.Values("If-Match"), r.Header.Values("If-None-Match")
	if len(matches) == 0 && len(absent) == 0 {
		return 0, http.StatusPreconditionRequired, errors.New("use If-Match with the GET ETag, or If-None-Match: * to create")
	}
	bad := errors.New("require one exact If-Match ETag, or PUT with If-None-Match: *; do not combine them")
	if len(matches) == 0 && len(absent) == 1 && r.Method == http.MethodPut && strings.TrimSpace(absent[0]) == "*" {
		return 0, 0, nil
	}
	if len(matches) != 1 || len(absent) != 0 {
		return 0, http.StatusBadRequest, bad
	}
	raw := strings.TrimSpace(matches[0])
	if len(raw) < 4 || !strings.HasPrefix(raw, `"v`) || !strings.HasSuffix(raw, `"`) {
		return 0, http.StatusBadRequest, bad
	}
	version, err := strconv.ParseInt(raw[2:len(raw)-1], 10, 64)
	if err != nil || version <= 0 || raw != versionETag(version) {
		return 0, http.StatusBadRequest, bad
	}
	return version, 0, nil
}
