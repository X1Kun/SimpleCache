package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/telemetry"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type APIOptions struct {
	Store               MutableSource
	Router              *peer.Router
	Token               string
	Lifecycle           context.Context
	MaxConcurrentWrites int
	writeSlots          chan struct{}
	DirectReads         bool
	NodeID              string
}

const DefaultWriteConcurrency = 16

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

// Envoy hashes the wire query value. Accept exactly one canonical representation
// so percent-encoding aliases, duplicate parameters and '+' cannot split one
// decoded source key across cache shards. Legacy Peer mode keeps its old API.
func requestKey(r *http.Request, canonical bool) (string, error) {
	if !canonical {
		return r.URL.Query().Get("key"), nil
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	bad := errors.New("gateway mode requires one canonical key query: percent-encoded UTF-8, uppercase escapes, %20 for spaces")
	if err != nil || len(values) != 1 || len(values["key"]) != 1 {
		return "", bad
	}
	key := values["key"][0]
	if !utf8.ValidString(key) {
		return "", bad
	}
	if err := cache.ValidateKey(key); err != nil {
		return "", err
	}
	wire := strings.ReplaceAll(url.QueryEscape(key), "+", "%20")
	if r.URL.RawQuery != "key="+wire {
		return "", bad
	}
	return key, nil
}
