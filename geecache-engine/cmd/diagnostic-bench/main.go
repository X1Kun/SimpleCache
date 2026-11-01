// diagnostic-bench launches fresh server subprocesses and never profiles itself.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/bench/diagnostics"
)

func main() {
	var o diagnostics.Options
	flag.StringVar(&o.Server, "server", "", "Diagnostic server binary")
	flag.StringVar(&o.Dir, "dir", "", "Artifact directory")
	flag.IntVar(&o.Seconds, "seconds", 10, "Seconds per trial, 10-30")
	flag.IntVar(&o.Repeats, "repeats", 3, "Unprofiled baseline repetitions, 3-5")
	flag.IntVar(&o.Workers, "workers", 8, "Closed-loop workers, 1-32")
	flag.BoolVar(&o.Quick, "quick", false, "Explicit short functional smoke, not a performance baseline")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if _, err := diagnostics.Run(ctx, o); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
