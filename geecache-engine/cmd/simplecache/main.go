package main

import (
	"context"
	"flag"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/app"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	port := flag.Int("port", 8001, "Cluster invalidation HTTP port")
	api := flag.Bool("api", true, "Serve API, health checks and metrics")
	flag.Parse()
	config, err := app.FromEnv(*port, *api)
	if err != nil {
		slog.Error("Invalid configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := app.Run(ctx, config); err != nil {
		slog.Error("Cache server failed", "error", err)
		os.Exit(1)
	}
}
