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
	"github.com/X1Kun/simpleCache/geecache-engine/internal/cluster"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/demo"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/discovery"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/outbox"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/source/postgres"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/telemetry"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	invalidationRequestTimeout = time.Second
	sourceStartupTimeout       = 5 * time.Second
	readHeaderTimeout          = 3 * time.Second
	requestReadTimeout         = 5 * time.Second
	connectionIdleTimeout      = 30 * time.Second
	shutdownGracePeriod        = 5 * time.Second
)

// Covers the 5s request-body read and bounded mutation work.
const responseWriteTimeout = 10 * time.Second

func Run(ctx context.Context, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := c.validateSourceConfig(); err != nil {
		return err
	}
	switch c.SourceMode {
	case "", "demo":
		return RunWithSource(ctx, c, demo.New(100*time.Millisecond))
	case "postgres":
		startup, cancel := context.WithTimeout(ctx, sourceStartupTimeout)
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
	if err := c.Validate(); err != nil {
		return err
	}
	if source == nil {
		return errors.New("source is required")
	}
	// The signal context controls shutdown; in-flight work survives the grace period.
	work, cancel := context.WithCancel(context.Background())
	defer cancel()
	router := cluster.NewDirectory(c.SelfID, invalidationRequestTimeout, c.AdminToken)
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
		TTL: c.TTL, KnownKeys: knownKeys, Lifecycle: work, Limiter: cache.NewSourceLimiterWithQueue(c.SourceConcurrency, c.SourceMaxPending), Observer: metrics,
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
		watcher, err = discovery.New(client, c.Namespace, c.ClusterService, router)
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
	servers := []*http.Server{newDataServer(c.ClusterAddr, cluster.NewHandlerWithToken(c.AdminToken, group))}
	if c.API {
		api := APIHandler(group, ready, metrics, APIOptions{Store: mutable, Directory: router, Token: c.AdminToken, Lifecycle: work, MaxConcurrentWrites: c.WriteConcurrency, NodeID: c.SelfID})
		servers = append(servers, newDataServer(c.APIAddr, api))
	}
	if c.DebugAddr != "" {
		servers = append(servers, &http.Server{Addr: c.DebugAddr, Handler: DebugHandler(), ReadHeaderTimeout: readHeaderTimeout, IdleTimeout: connectionIdleTimeout})
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
	if queue, ok := source.(outbox.Queue); ok && c.AdminToken != "" {
		relayCtx, stopRelay := context.WithCancel(work)
		relayDone := make(chan struct{})
		defer func() { stopRelay(); <-relayDone }() // Join before Directory/Store are closed.
		relay := outbox.Worker{Queue: queue, Observer: metrics, Send: outboxSender(ready, group, router)}
		go func() { defer close(relayDone); relay.Run(relayCtx) }()
	}

	if c.ProfileContention {
		previous := runtime.SetMutexProfileFraction(10)
		runtime.SetBlockProfileRate(1000000)
		defer runtime.SetMutexProfileFraction(previous)
		defer runtime.SetBlockProfileRate(0)
	}
	addresses := map[string]string{"cluster": listeners[0].Addr().String()}
	if c.API {
		addresses["api"] = listeners[1].Addr().String()
	}
	if c.DebugAddr != "" {
		addresses["debug"] = listeners[len(listeners)-1].Addr().String()
	}
	slog.Info("Cache server started", "identity", c.SelfID, "discovery", c.DiscoveryMode, "listeners", addresses)
	return serve(ctx, servers, listeners, &serving)
}

// A relay can finish only a successful nonempty captured membership snapshot.
// Readiness alone can legitimately have an empty discovery view during startup.
func outboxSender(ready func() bool, group *cache.Group, router *cluster.Directory) outbox.Sender {
	return func(ctx context.Context, event outbox.Event) error {
		if !ready() {
			return errors.New("outbox discovery not ready")
		}
		report, err := router.InvalidateVersion(ctx, group, event.Key, event.Version)
		if err != nil {
			return err
		}
		if !report.Complete || report.KnownMembers == 0 {
			return errors.New("outbox delivery incomplete")
		}
		return nil
	}
}

func newDataServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout: requestReadTimeout, WriteTimeout: responseWriteTimeout, IdleTimeout: connectionIdleTimeout}
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
	shutdown, stop := context.WithTimeout(context.Background(), shutdownGracePeriod)
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
