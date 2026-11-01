package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer"
)

func TestMutationCoordinatorPreservesCommitAndInvalidationOutcomes(t *testing.T) {
	for _, name := range []string{"committed", "conflict", "unknown", "notification-only"} {
		t.Run(name, func(t *testing.T) {
			store := &memoryProducts{values: map[string][]byte{"key": []byte(`{"v":1}`)}}
			group := cache.NewGroup("products", 1024, store, cache.Options{})
			if _, err := group.GetDirect(context.Background(), "key"); err != nil {
				t.Fatal(err)
			}
			router := peer.NewDirectory("local", time.Second, "token")
			defer router.Close()
			command := mutationCommand{kind: mutationPut, key: "key", value: []byte(`{"v":2}`), expected: 1}
			switch name {
			case "conflict":
				command.expected = 0
			case "unknown":
				store.writeErr = errors.New("ambiguous source failure")
			case "notification-only":
				if err := store.Put(context.Background(), "key", []byte(`{"v":2}`)); err != nil {
					t.Fatal(err)
				}
				command.kind = mutationNotify
				command.version = 2
			}
			outcome := executeMutation(nil, group, store, router, command, nil)
			if outcome.broadcastErr != nil {
				t.Fatal(outcome.broadcastErr)
			}
			response := outcome.response
			if name == "conflict" {
				if !errors.Is(outcome.writeErr, cache.ErrPreconditionFailed) || response.SourceCommitted == nil || *response.SourceCommitted || response.Invalidation != nil || group.Stats().Entries != 1 {
					t.Fatalf("conflict mutated or invalidated: %+v", outcome)
				}
				return
			}
			if group.Stats().Entries != 0 || response.Invalidation == nil || !response.Invalidation.Complete || store.reads != 1 {
				t.Fatalf("missing invalidation or unexpected source read: %+v", outcome)
			}
			switch name {
			case "committed":
				if outcome.writeErr != nil || response.SourceCommitted == nil || !*response.SourceCommitted || response.SourceVersion == nil || *response.SourceVersion != 2 || response.Invalidation.Version != 2 {
					t.Fatalf("lost committed version: %+v", outcome)
				}
			case "unknown":
				if outcome.writeErr == nil || response.SourceCommitted != nil || response.SourceVersion != nil || response.Invalidation.Version != 0 || store.versionLocked("key") != 1 {
					t.Fatalf("invented commit outcome: %+v", outcome)
				}
			case "notification-only":
				if outcome.writeErr != nil || response.SourceCommitted != nil || response.SourceVersion != nil || response.Invalidation.Version != 2 || store.versionLocked("key") != 2 {
					t.Fatalf("notification changed source: %+v", outcome)
				}
			}
		})
	}
}
