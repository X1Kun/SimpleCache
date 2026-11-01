package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/demo"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/discovery"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/source/postgres"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/telemetry"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const peerReadTimeout = time.Second

// Covers the 5s request-body read and bounded mutation work.
const responseWriteTimeout = 10 * time.Second

func Run(ctx context.Context, c Config) error {
	switch c.SourceMode {
	case "", "demo":
		return RunWithSource(ctx, c, demo.New(100*time.Millisecond))
	case "postgres":
		if c.DatabaseURL == "" || c.AdminToken == "" {
			return errors.New("postgres mode requires DATABASE_URL and CACHE_ADMIN_TOKEN")
		}
		startup, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		source, err := postgres.Open(startup, c.DatabaseURL, int32(c.SourceConcurrency))
		if err != nil {
			return err
		}
		defer source.Close()
		return RunWithSource(ctx, c, source)
	default:
		return errors.New("unsupported SOURCE_MODE")
	}
}

// RunWithSource shares the real server lifecycle with the isolated diagnostic fixture.
func RunWithSource(ctx context.Context, c Config, source cache.Getter) error {
	if err := validateDebugAddr(c.DebugAddr); err != nil {
		return err
	}
	if c.ProfileContention && c.DebugAddr == "" {
		return errors.New("contention profiling requires DEBUG_ADDR")
	}
	if source == nil {
		return errors.New("source is required")
	}
	// The signal context controls shutdown; in-flight work survives the grace period.
	work, cancel := context.WithCancel(context.Background())
	defer cancel()
	if c.ReadRoutingMode == "" {
		c.ReadRoutingMode = "peer"
	}
	if c.ReadRoutingMode != "peer" && c.ReadRoutingMode != "gateway" {
		return errors.New("unsupported read routing mode")
	}
	if c.ReadRoutingMode == "gateway" && c.HotCacheBytes != 0 {
		return errors.New("gateway routing cannot reserve a hot area")
	}
	var router *peer.Router
	if c.ReadRoutingMode == "gateway" {
		router = peer.NewDirectory(c.SelfID, peerReadTimeout, c.AdminToken)
	} else {
		router = peer.NewRouterWithToken(c.SelfID, peerReadTimeout, c.AdminToken)
	}
	defer router.Close()
	if err := router.Update(c.Members); err != nil {
		return err
	}
	metrics := telemetry.New()
	metrics.Membership(len(router.Members()), false)
	var knownKeys []string
	mutable, _ := source.(MutableSource)
	// A mutable source must not use a startup-only key set.
	if known, ok := source.(interface{ Keys() []string }); ok && mutable == nil {
		knownKeys = known.Keys()
	}
	groupName := "scores"
	if c.SourceMode == "postgres" {
		groupName = "products"
	}
	group := cache.NewGroup(groupName, c.CacheBytes, source, cache.Options{
		TTL: c.TTL, KnownKeys: knownKeys, Peers: router, Lifecycle: work, Limiter: cache.NewSourceLimiterWithQueue(c.SourceConcurrency, c.SourceMaxPending), Observer: metrics,
		HotCacheBytes:  c.HotCacheBytes,
		MaxMissWaiters: c.MaxMissWaiters,
	})
	metrics.ObserveCache(group)
	var watcher *discovery.Watcher
	if c.DiscoveryMode == "kubernetes" {
		config, err := rest.InClusterConfig()
		if err != nil {
			return err
		}
		client, err := kubernetes.NewForConfig(config)
		if err != nil {
			return err
		}
		watcher, err = discovery.New(client, c.Namespace, c.PeerService, router)
		if err != nil {
			return err
		}
		watcher.Report = metrics.Membership
		watcher.ReportError = metrics.DiscoveryError
	} else if c.DiscoveryMode != "static" {
		return errors.New("unsupported discovery mode")
	}
	var serving atomic.Bool
	ready := func() bool { return serving.Load() && (watcher == nil || watcher.Ready()) }
	servers := []*http.Server{newDataServer(c.PeerAddr, peer.NewHandlerWithToken(c.AdminToken, group))}
	if c.API {
		api := APIHandler(group, ready, metrics, APIOptions{Store: mutable, Router: router, Token: c.AdminToken, Lifecycle: work, MaxConcurrentWrites: c.WriteConcurrency, DirectReads: c.ReadRoutingMode == "gateway", NodeID: c.SelfID})
		servers = append(servers, newDataServer(c.APIAddr, api))
	}
	if c.DebugAddr != "" {
		servers = append(servers, &http.Server{Addr: c.DebugAddr, Handler: DebugHandler(), ReadHeaderTimeout: 3 * time.Second, IdleTimeout: 30 * time.Second})
	}
	// Bind every port before declaring readiness or starting discovery.
	listeners, err := bindServers(servers)
	if err != nil {
		return err
	}
	if watcher != nil {
		go watcher.Run(work)
	}
	go group.RunMaintenance(work)
	if c.ProfileContention {
		previous := runtime.SetMutexProfileFraction(10)
		runtime.SetBlockProfileRate(1000000)
		defer runtime.SetMutexProfileFraction(previous)
		defer runtime.SetBlockProfileRate(0)
	}
	addresses := map[string]string{"peer": listeners[0].Addr().String()}
	if c.API {
		addresses["api"] = listeners[1].Addr().String()
	}
	if c.DebugAddr != "" {
		addresses["debug"] = listeners[len(listeners)-1].Addr().String()
	}
	slog.Info("Cache server started", "identity", c.SelfID, "discovery", c.DiscoveryMode, "listeners", addresses)
	return serve(ctx, servers, listeners, &serving)
}

func newDataServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: responseWriteTimeout, IdleTimeout: 30 * time.Second}
}

// bindServers opens every listener before readiness and closes partial work on error.
func bindServers(servers []*http.Server) ([]net.Listener, error) {
	listeners := make([]net.Listener, 0, len(servers))
	for _, server := range servers {
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return nil, err
		}
		listeners = append(listeners, listener)
	}
	return listeners, nil
}

// serve drains active HTTP requests before the caller stops shared source work.
func serve(ctx context.Context, servers []*http.Server, listeners []net.Listener, serving *atomic.Bool) error {
	errorsC := make(chan error, len(servers))
	for i, s := range servers {
		go func(server *http.Server, listener net.Listener) { errorsC <- server.Serve(listener) }(s, listeners[i])
	}
	serving.Store(true)
	var result error
	select {
	case <-ctx.Done():
	case err := <-errorsC:
		if !errors.Is(err, http.ErrServerClosed) {
			result = err
		}
	}
	serving.Store(false)
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	for _, server := range servers {
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			if result == nil {
				result = err
			}
		}
	}
	return result
}
