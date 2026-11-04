// arrival-reference is a bounded in-memory HTTP fixture for client calibration.
// Its results do not establish cache service capacity.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	bench "github.com/X1Kun/simpleCache/geecache-engine/internal/bench/gateway"
)

func main() {
	address := flag.String("listen", "127.0.0.1:0", "listen address")
	prefix := flag.String("prefix", "calibration", "fixture key prefix")
	keys := flag.Int("keys", 2048, "fixture keys")
	size := flag.Int("value-bytes", 4096, "fixture value bytes")
	flag.Parse()
	if *keys < 1 || *keys > 4096 || *size < 128 || *size > 16384 || len(*prefix) > 80 {
		panic("invalid bounded reference fixture")
	}
	values := make(map[string][]byte, *keys)
	for i := 0; i < *keys; i++ {
		key := bench.Key(*prefix, i)
		values[key] = bench.Value(key, 1, *size)
	}
	var received atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int64{"received": received.Load()})
	})
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.Header().Set("X-SimpleCache-Node", "calibration-reference")
		value, ok := values[r.URL.Query().Get("key")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(value)
	})
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		panic(err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"endpoint": "http://" + listener.Addr().String()})
	select {
	case <-ctx.Done():
	case <-done:
	}
	drain, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(drain)
}
