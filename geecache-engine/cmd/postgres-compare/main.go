package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/productcompare"
)

func main() {
	var options productcompare.Options
	flag.StringVar(&options.Server, "server", "", "Loopback PostgreSQL fixture binary")
	flag.StringVar(&options.Dir, "dir", "", "Fresh artifact directory")
	flag.IntVar(&options.Repeats, "repeats", 3, "Paired repeats, 3-5; quick permits 1")
	flag.IntVar(&options.Workers, "workers", 12, "Closed-loop workers, 3-32")
	flag.IntVar(&options.Keys, "keys", 512, "Working-set keys")
	flag.IntVar(&options.ValueBytes, "value-bytes", 4096, "Exact JSON value bytes")
	flag.Int64Var(&options.CacheBytes, "cache-bytes", 1<<20, "Logical capacity per cache")
	flag.IntVar(&options.Passes, "passes", 2, "Measured capacity passes after one warmup pass")
	flag.Int64Var(&options.Seed, "seed", 42, "Deterministic key/trace seed")
	flag.BoolVar(&options.Quick, "quick", false, "Functional smoke only, not performance evidence")
	flag.IntVar(&options.ZipfReads, "zipf-reads", 8192, "Measured Zipf reads")
	flag.IntVar(&options.MixedWindows, "mixed-windows", 30, "Concurrent 19-read/1-write windows")
	flag.Parse()
	options.DatabaseURL = os.Getenv("SIMPLECACHE_COMPARE_DATABASE_URL")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	if _, err := productcompare.Run(ctx, options); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
