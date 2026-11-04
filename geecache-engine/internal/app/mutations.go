package app

import (
	"context"
	"errors"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/cluster"
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

type mutationResponse struct {
	// nil means unknown after a source error, not a guaranteed rollback.
	SourceCommitted *bool                       `json:"source_committed"`
	SourceVersion   *int64                      `json:"source_version,omitempty"`
	Invalidation    *cluster.InvalidationReport `json:"invalidation,omitempty"`
	Error           string                      `json:"error,omitempty"`
}

// Source-write and read-source budgets are independent policies even though
// their current durations match.
const (
	mutationTimeout       = 3 * time.Second
	mutationSourceTimeout = 800 * time.Millisecond
)

type mutationKind uint8

const (
	mutationNotify mutationKind = iota
	mutationPut
	mutationDelete
)

type mutationCommand struct {
	kind     mutationKind
	key      string
	value    []byte
	expected int64
	version  int64
}
type mutationOutcome struct {
	response     mutationResponse
	writeErr     error
	broadcastErr error
}

type mutationInvalidator interface {
	InvalidateVersion(context.Context, *cache.Group, string, int64) (cluster.InvalidationReport, error)
}
type mutationObserver interface {
	Observe(string, string, time.Duration)
}

// executeMutation owns the post-validation write and notification lifecycle.
// Caller disconnection cannot cancel this work; service shutdown still can.
func executeMutation(lifecycle context.Context, group *cache.Group, store MutableSource, router mutationInvalidator, command mutationCommand, metrics mutationObserver) mutationOutcome {
	if lifecycle == nil {
		lifecycle = context.Background()
	}
	work, cancel := context.WithTimeout(lifecycle, mutationTimeout)
	defer cancel()
	var outcome mutationOutcome
	response := &outcome.response
	version := command.version
	if command.kind != mutationNotify {
		writeStart := time.Now()
		source, stop := context.WithTimeout(work, mutationSourceTimeout)
		var committedVersion int64
		if command.kind == mutationPut {
			committedVersion, outcome.writeErr = store.PutIfVersion(source, command.key, command.value, command.expected)
		} else {
			committedVersion, outcome.writeErr = store.DeleteIfVersion(source, command.key, command.expected)
		}
		stop()
		if outcome.writeErr == nil && committedVersion <= 0 {
			outcome.writeErr = errors.New("source returned an invalid committed version")
		}
		if outcome.writeErr == nil {
			committed := true
			response.SourceCommitted = &committed
			response.SourceVersion = &committedVersion
		}
		if metrics != nil {
			metrics.Observe("write", cache.Result(outcome.writeErr), time.Since(writeStart))
		}
		if errors.Is(outcome.writeErr, cache.ErrPreconditionFailed) {
			committed := false
			response.SourceCommitted = &committed
			response.Error = "precondition failed; refresh the value before retrying"
			return outcome
		}
	}
	// Source errors can represent ambiguous commits. Force invalidation then too.
	broadcastStart := time.Now()
	if response.SourceVersion != nil {
		version = *response.SourceVersion
	}
	report, err := router.InvalidateVersion(work, group, command.key, version)
	if err != nil {
		if metrics != nil {
			metrics.Observe("broadcast", "error", time.Since(broadcastStart))
		}
		outcome.broadcastErr = err
		return outcome
	}
	response.Invalidation = &report
	broadcastResult := "complete"
	if !report.Complete {
		broadcastResult = "partial"
	}
	if metrics != nil {
		metrics.Observe("broadcast", broadcastResult, time.Since(broadcastStart))
	}
	if outcome.writeErr != nil {
		response.Error = "source write failed; commit status is unknown; verify the source before retrying"
	}
	return outcome
}
