// Package handler is the inbound HTTP adapter: routing, middleware, request
// parsing and response mapping. It depends on use cases only through the
// small interfaces declared next to the handlers that consume them.
package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
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
	// Profiler enables /debug/pprof/* behind Basic auth; nil disables it.
	Profiler *Credentials
}

// Credentials for HTTP Basic authentication.
type Credentials struct {
	Username string
	Password string
}

// NewInternalRouter serves operational endpoints: probes, metrics, profiling
// and the sync trigger. It must never be exposed through the public ingress.
func NewInternalRouter(d InternalDeps) http.Handler {
	rt := newRouter()
	rt.handle(http.MethodGet, "/health", http.HandlerFunc(liveness))
	rt.handle(http.MethodGet, "/ready", readiness(d.Readiness))
	if d.Profiler != nil {
		registerPprof(rt)
		rt.guard("/debug/pprof", BasicAuth("pprof", d.Profiler.Username, d.Profiler.Password))
	}
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
	mux    *http.ServeMux
	guards []Middleware
}

func newRouter() *router {
	return &router{mux: http.NewServeMux()}
}

func (rt *router) handle(method, pattern string, h http.Handler) {
	rt.mux.Handle(method+" "+pattern, h)
}

// guard applies mw to every request whose path starts with prefix *before*
// routing, so unauthenticated callers cannot discover routes under it through
// 404/405 answers or redirects.
func (rt *router) guard(prefix string, mw Middleware) {
	rt.guards = append(rt.guards, func(next http.Handler) http.Handler {
		guarded := mw(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, prefix) {
				guarded.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
}

// ServeHTTP dispatches matched requests to the mux. For unmatched ones the
// mux still decides between 404 and 405 (it knows which methods are
// registered for the path and computes the Allow header), but its plain-text
// answer is replaced with the JSON error envelope.
func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, pattern := rt.mux.Handler(r); pattern != "" {
		rt.mux.ServeHTTP(w, r)
		return
	}

	probe := &statusProbe{header: make(http.Header)}
	rt.mux.ServeHTTP(probe, r)
	if probe.status == http.StatusMethodNotAllowed {
		w.Header().Set("Allow", probe.header.Get("Allow"))
		apierror.Write(w, r, apierror.New(http.StatusMethodNotAllowed, apierror.CodeMethodNotAllowed,
			"method "+r.Method+" is not allowed on "+r.URL.Path))
		return
	}
	apierror.Write(w, r, apierror.New(http.StatusNotFound, apierror.CodeNotFound, "resource not found"))
}

// build wraps the router with the standard middleware chain. quietPaths are
// access-logged at debug level.
func (rt *router) build(logger *slog.Logger, quietPaths ...string) http.Handler {
	mws := []Middleware{
		RequestID(),
		AccessLog(logger, quietPaths...),
		Recover(logger),
	}
	return Chain(rt, append(mws, rt.guards...)...)
}

// statusProbe captures the status and headers the mux would have written.
type statusProbe struct {
	header http.Header
	status int
}

func (p *statusProbe) Header() http.Header         { return p.header }
func (p *statusProbe) Write(b []byte) (int, error) { return len(b), nil }
func (p *statusProbe) WriteHeader(code int)        { p.status = code }

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
