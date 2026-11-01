package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Covers the 5s request-body read and bounded mutation work.
const responseWriteTimeout = 10 * time.Second

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
