// Package handler is the inbound HTTP adapter: routing, middleware, request
// parsing and response mapping. It depends on use cases only through the
// small interfaces declared next to the handlers that consume them.
package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/wmiha/vyking-3p-integration-service/internal/apierror"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/health"
)

// ReadinessChecker reports whether the instance can serve traffic.
type ReadinessChecker interface {
	Check(ctx context.Context) health.Report
}

// InternalDeps are the collaborators of the internal (cluster-only) router.
type InternalDeps struct {
	Logger    *slog.Logger
	Readiness ReadinessChecker
}

// NewInternalRouter serves operational endpoints: probes, metrics, profiling
// and the sync trigger. It must never be exposed through the public ingress.
func NewInternalRouter(d InternalDeps) http.Handler {
	rt := newRouter()
	rt.handle(http.MethodGet, "/health", http.HandlerFunc(liveness))
	rt.handle(http.MethodGet, "/ready", readiness(d.Readiness))
	return rt.build(d.Logger, "/health", "/ready")
}

// PublicDeps are the collaborators of the public (partner-facing) router.
type PublicDeps struct {
	Logger *slog.Logger
}

// NewPublicRouter serves the partner API.
func NewPublicRouter(d PublicDeps) http.Handler {
	rt := newRouter()
	return rt.build(d.Logger)
}

// router wraps http.ServeMux so that unknown paths and unsupported methods
// are answered with the standard JSON error envelope instead of plain text.
type router struct {
	mux     *http.ServeMux
	methods map[string][]string // path -> allowed methods
}

func newRouter() *router {
	return &router{mux: http.NewServeMux(), methods: make(map[string][]string)}
}

func (rt *router) handle(method, path string, h http.Handler) {
	rt.mux.Handle(method+" "+path, h)

	if _, seen := rt.methods[path]; !seen {
		rt.mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", strings.Join(rt.methods[path], ", "))
			apierror.Write(w, r, apierror.New(http.StatusMethodNotAllowed, apierror.CodeMethodNotAllowed,
				"method "+r.Method+" is not allowed on "+path))
		})
	}
	rt.methods[path] = append(rt.methods[path], method)
	if method == http.MethodGet {
		rt.methods[path] = append(rt.methods[path], http.MethodHead)
	}
	slices.Sort(rt.methods[path])
}

// build registers the JSON 404 fallback and wraps the mux with the standard
// middleware chain. quietPaths are access-logged at debug level.
func (rt *router) build(logger *slog.Logger, quietPaths ...string) http.Handler {
	rt.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apierror.Write(w, r, apierror.New(http.StatusNotFound, apierror.CodeNotFound, "resource not found"))
	})
	return Chain(rt.mux,
		RequestID(),
		AccessLog(logger, quietPaths...),
		Recover(logger),
	)
}

// writeJSON renders a successful JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// liveness only proves the process is serving HTTP; it deliberately ignores
// dependencies so a database outage does not trigger restart loops.
func liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": health.StatusOK})
}

// readiness reports 503 while a dependency is down or the instance is
// draining, so Kubernetes stops routing traffic to it.
func readiness(checker ReadinessChecker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		report := checker.Check(r.Context())
		status := http.StatusOK
		if !report.Ready() {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, report)
	})
}
