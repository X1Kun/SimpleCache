// validation-probe checks functional continuity on the immutable demo dataset.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/validation/probe"
)

func main() {
	target := flag.String("url", "", "API origin URL")
	dir := flag.String("dir", "", "Artifact directory")
	flag.Parse()
	if *target == "" || *dir == "" {
		fmt.Fprintln(os.Stderr, "-url and -dir are required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	summary, err := probe.Run(ctx, probe.Options{Origin: *target, Dir: *dir})
	if summary.Status != "" {
		compact, _ := json.Marshal(summary)
		fmt.Println("EVIDENCE " + string(compact))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
