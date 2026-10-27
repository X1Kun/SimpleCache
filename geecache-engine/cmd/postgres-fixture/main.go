// postgres-fixture is a loopback-only diagnostic process. It shares the real
// cache, PostgreSQL source, peer/API handlers and metrics, with selectable read
// routing. Both modes retain identical membership for write invalidation.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/app"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/source/postgres"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/telemetry"
)

func main() {
	mode := flag.String("mode", "", "independent or coordinated read routing")
	node := flag.Int("node", -1, "Node index, 0-2")
	peers := flag.String("peers", "", "Three comma-separated loopback peer addresses")
	api := flag.String("api", "", "Loopback API listen address")
	capacity := flag.Int64("capacity", 1<<20, "Logical cache bytes per node")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, *mode, *node, strings.Split(*peers, ","), *api, *capacity); err != nil {
		slog.Error("Fixture failed", "error", err)
		os.Exit(1)
	}
}

func loopback(address string) bool {
	host, port, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback() && port != "0" && port != ""
}

func run(ctx context.Context, mode string, node int, addresses []string, api string, capacity int64) error {
	if (mode != "independent" && mode != "coordinated" && mode != "coordinated-global" && mode != "coordinated-hot") || node < 0 || node > 2 || len(addresses) != 3 || !loopback(api) || capacity < 1<<20 || capacity > 128<<20 {
		return fmt.Errorf("invalid diagnostic fixture configuration")
	}
	var members []peer.Member
	for i, address := range addresses {
		if !loopback(address) {
			return fmt.Errorf("peer address must be literal loopback")
		}
		members = append(members, peer.Member{ID: fmt.Sprintf("node-%d", i), Address: address})
	}
	dsn, token := os.Getenv("DATABASE_URL"), os.Getenv("CACHE_ADMIN_TOKEN")
	if dsn == "" || token == "" {
		return fmt.Errorf("DATABASE_URL and CACHE_ADMIN_TOKEN are required")
	}
	startup, cancelStartup := context.WithTimeout(ctx, 5*time.Second)
	source, err := postgres.Open(startup, dsn, 32)
	cancelStartup()
	if err != nil {
		return err
	}
	defer source.Close()
	work, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()
	router := peer.NewRouterWithToken(members[node].ID, time.Second, token)
	defer router.Close()
	if err := router.Update(members); err != nil {
		return err
	}
	var readPeers cache.PeerPicker
	if mode != "independent" {
		readPeers = router
	}
	metrics := telemetry.New()
	metrics.Membership(len(members), false)
	epochBuckets := 256
	if mode == "coordinated-global" {
		epochBuckets = 1
	}
	var hotBytes int64
	if mode == "coordinated-hot" {
		hotBytes = capacity / 8
	}
	group := cache.NewGroup("products", capacity, source, cache.Options{
		TTL: time.Hour, Peers: readPeers, Lifecycle: work,
		EpochBuckets:  epochBuckets,
		HotCacheBytes: hotBytes,
		Limiter:       cache.NewSourceLimiter(32), Observer: metrics,
	})
	metrics.ObserveCache(group)
	var ready atomic.Bool
	servers := []*http.Server{
		{Handler: peer.NewHandlerWithToken(token, group), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second},
		{Handler: app.APIHandler(group, ready.Load, metrics, app.APIOptions{Store: source, Router: router, Token: token, Lifecycle: work}), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second},
	}
	var listeners []net.Listener
	for _, address := range []string{addresses[node], api} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			for _, previous := range listeners {
				_ = previous.Close()
			}
			return err
		}
		listeners = append(listeners, listener)
	}
	errors := make(chan error, len(servers))
	for i, server := range servers {
		go func(server *http.Server, listener net.Listener) { errors <- server.Serve(listener) }(server, listeners[i])
	}
	ready.Store(true)
	go group.RunMaintenance(work)
	slog.Info("Fixture ready", "listeners", map[string]string{"peer": listeners[0].Addr().String(), "api": listeners[1].Addr().String()})
	var result error
	select {
	case <-ctx.Done():
	case err := <-errors:
		if err != http.ErrServerClosed {
			result = err
		}
	}
	ready.Store(false)
	drain, cancelDrain := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDrain()
	for _, server := range servers {
		if err := server.Shutdown(drain); err != nil {
			_ = server.Close()
			result = err
		}
	}
	return result
}
