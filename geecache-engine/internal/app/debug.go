package app

import (
	"net/http"
	"net/http/pprof"
)

// DebugHandler is served only by a separate, explicitly enabled loopback listener.
// Neither public API nor notification handlers use DefaultServeMux.
func DebugHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/{$}", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	for _, name := range []string{"heap", "allocs", "mutex", "block", "goroutine"} {
		mux.Handle("GET /debug/pprof/"+name, pprof.Handler(name))
	}
	return mux
}
