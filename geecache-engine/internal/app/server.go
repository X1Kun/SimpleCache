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
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const peerReadTimeout = time.Second

func APIHandler(group *cache.Group, ready func() bool, metrics *telemetry.Metrics, options ...APIOptions) http.Handler {
	var opts APIOptions
	if len(options) > 0 {
		opts = options[0]
	}
	if opts.MaxConcurrentWrites == 0 {
		opts.MaxConcurrentWrites = DefaultWriteConcurrency
	}
	if opts.MaxConcurrentWrites < 1 {
		panic("write concurrency must be positive")
	}
	opts.writeSlots = make(chan struct{}, opts.MaxConcurrentWrites)
	writable := opts.Store != nil && opts.Router != nil && opts.Token != ""
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready() {
			http.Error(w, "not ready", 503)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		result := "error"
		defer func() {
			if metrics != nil {
				metrics.Observe("api", result, time.Since(start))
			}
		}()
		isWrite := writable && (r.Method == http.MethodPut || r.Method == http.MethodDelete)
		if r.Method != http.MethodGet && !isWrite {
			allow := "GET"
			if writable {
				allow = "GET, PUT, DELETE"
			}
			w.Header().Set("Allow", allow)
			http.Error(w, "method not allowed", 405)
			return
		}
		if !ready() {
			http.Error(w, "not ready", 503)
			return
		}
		if opts.NodeID != "" {
			w.Header().Set("X-SimpleCache-Node", opts.NodeID)
		}
		if isWrite {
			result = mutate(w, r, group, opts, metrics, false)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
		defer cancel()
		key, keyErr := requestKey(r, opts.DirectReads)
		if keyErr != nil {
			http.Error(w, keyErr.Error(), http.StatusBadRequest)
			return
		}
		get := group.Get
		if opts.DirectReads {
			get = group.GetDirect
		}
		value, err := get(ctx, key)
		result = cache.Result(err)
		if err != nil {
			if errors.Is(err, cache.ErrOverloaded) {
				w.Header().Set("Retry-After", "1")
			}
			http.Error(w, err.Error(), peer.Status(err))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if value.Version() > 0 {
			w.Header().Set("ETag", versionETag(value.Version()))
		}
		_, _ = value.WriteTo(w)
	})
	if opts.Router != nil && opts.Token != "" {
		mux.HandleFunc("/invalidate", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if !ready() {
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
			mutate(w, r, group, opts, metrics, true)
		})
	}
	if metrics != nil {
		mux.Handle("/metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
	}
	return mux
}
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
	servers := []*http.Server{{Addr: c.PeerAddr, Handler: peer.NewHandlerWithToken(c.AdminToken, group), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}}
	if c.API {
		api := APIHandler(group, ready, metrics, APIOptions{Store: mutable, Router: router, Token: c.AdminToken, Lifecycle: work, MaxConcurrentWrites: c.WriteConcurrency, DirectReads: c.ReadRoutingMode == "gateway", NodeID: c.SelfID})
		servers = append(servers, &http.Server{Addr: c.APIAddr, Handler: api, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second})
	}
	if c.DebugAddr != "" {
		servers = append(servers, &http.Server{Addr: c.DebugAddr, Handler: DebugHandler(), ReadHeaderTimeout: 3 * time.Second, IdleTimeout: 30 * time.Second})
	}
	// Bind every port before declaring readiness or starting discovery.
	listeners := make([]net.Listener, 0, len(servers))
	for _, server := range servers {
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return err
		}
		listeners = append(listeners, listener)
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
