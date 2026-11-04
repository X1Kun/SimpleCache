package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/cluster"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/outbox"
)

func TestOutboxSenderRequiresReadyNonemptyCapturedDirectory(t *testing.T) {
	group := cache.NewGroup("products", 1024, cache.GetterFunc(func(context.Context, string) ([]byte, error) { return []byte("value"), nil }), cache.Options{})
	router := cluster.NewDirectory("self", time.Second, "token")
	defer router.Close()
	event := outbox.Event{ID: 1, Key: "key", Version: 1}
	if err := outboxSender(func() bool { return false }, group, router)(context.Background(), event); err == nil {
		t.Fatal("unready discovery acknowledged")
	}
	if err := outboxSender(func() bool { return true }, group, router)(context.Background(), event); err == nil {
		t.Fatal("empty discovery acknowledged")
	}
	if err := router.Update([]cluster.Member{{ID: "self", Address: "127.0.0.1:8001"}}); err != nil {
		t.Fatal(err)
	}
	if err := outboxSender(func() bool { return true }, group, router)(context.Background(), event); err != nil {
		t.Fatal("single-node domain could not acknowledge", err)
	}
	report, err := router.InvalidateVersion(context.Background(), group, event.Key, event.Version)
	if err != nil || report.KnownMembers != 1 {
		t.Fatal(report, err)
	}
	data, err := json.Marshal(report)
	if err != nil || strings.Contains(string(data), "KnownMembers") || strings.Contains(string(data), "known_members\"") {
		t.Fatal("internal readiness metadata changed wire report", string(data), err)
	}
}
