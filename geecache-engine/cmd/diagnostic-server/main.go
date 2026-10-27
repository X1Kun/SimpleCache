// diagnostic-server runs the real cache server with an explicitly synthetic dataset.
// It is never used by the production container or Operator.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/app"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/demo"
)

func main() {
	capacity := flag.Int64("capacity", 64<<20, "Logical cache bytes")
	valueBytes := flag.Int("value-bytes", 0, "Padded Auto value bytes; zero keeps demo values")
	contention := flag.Bool("contention", false, "Enable sampled mutex/block profiles")
	flag.Parse()
	if *capacity < 1<<20 || *capacity > 128<<20 || *valueBytes < 0 || *valueBytes > demo.MaxDiagnosticValueBytes {
		fmt.Fprintln(os.Stderr, "invalid fixture configuration")
		os.Exit(2)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	c := app.Config{PeerAddr: "127.0.0.1:0", APIAddr: "127.0.0.1:0", DebugAddr: "127.0.0.1:0", SelfID: "diagnostic-fixture", DiscoveryMode: "static", API: true, CacheBytes: *capacity, TTL: time.Hour, SourceConcurrency: 32, SourceMaxPending: cache.DefaultSourcePending, MaxMissWaiters: cache.DefaultMissWaiters, WriteConcurrency: app.DefaultWriteConcurrency, ProfileContention: *contention}
	if err := app.RunWithSource(ctx, c, demo.NewSized(100*time.Millisecond, *valueBytes)); err != nil {
		slog.Error("Diagnostic server failed", "error", err)
		os.Exit(1)
	}
}
