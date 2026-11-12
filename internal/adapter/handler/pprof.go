package handler

import (
	"net/http"
	"net/http/pprof"
)

// registerPprof exposes the runtime profiler; the caller guards the
// /debug/pprof prefix with authentication. Handlers are registered explicitly
// on our own mux: importing net/http/pprof also registers them on
// http.DefaultServeMux, which this service never serves.
//
// Long profiles (/profile?seconds=N, /trace?seconds=N) extend their own write
// deadline beyond the server's write_timeout and stop early when the request
// context ends, which the app uses to keep them from blocking shutdown.
func registerPprof(rt *router) {
	rt.handle(http.MethodGet, "/debug/pprof/", http.HandlerFunc(pprof.Index))
	rt.handle(http.MethodGet, "/debug/pprof/cmdline", http.HandlerFunc(pprof.Cmdline))
	rt.handle(http.MethodGet, "/debug/pprof/profile", http.HandlerFunc(pprof.Profile))
	rt.handle(http.MethodGet, "/debug/pprof/symbol", http.HandlerFunc(pprof.Symbol))
	rt.handle(http.MethodPost, "/debug/pprof/symbol", http.HandlerFunc(pprof.Symbol))
	rt.handle(http.MethodGet, "/debug/pprof/trace", http.HandlerFunc(pprof.Trace))
}
