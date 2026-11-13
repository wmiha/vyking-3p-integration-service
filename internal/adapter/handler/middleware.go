package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"time"

	"github.com/wmiha/vyking-3p-integration-service/internal/apierror"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/requestid"
)

// Middleware decorates an http.Handler.
type Middleware func(http.Handler) http.Handler

// Chain wraps h so that the first middleware is the outermost one.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for _, mw := range slices.Backward(mws) {
		h = mw(h)
	}
	return h
}

// RequestID assigns every request an identifier, stores it in the context
// and echoes it in the response header. A caller-supplied id is reused when
// it is safe to do so, which lets Acme Support correlate calls end to end.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(requestid.Header)
			if !requestid.Valid(id) {
				id = requestid.New()
			}
			w.Header().Set(requestid.Header, id)
			next.ServeHTTP(w, r.WithContext(requestid.NewContext(r.Context(), id)))
		})
	}
}

// Recover turns a panic into a 500 response so one faulty request cannot take
// down the process. http.ErrAbortHandler keeps its special meaning.
func Recover(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				logger.ErrorContext(r.Context(), "panic recovered",
					"panic", rec, "stack", string(debug.Stack()))
				apierror.Write(w, r, apierror.Internal(errors.New("panic recovered")))
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// AccessLog logs one line per request with the matched route pattern, never
// the query string: it carries customer emails (PII). Successful requests to
// quiet paths (probes) are logged at debug level to keep logs readable;
// failures on them are still logged at info.
func AccessLog(logger *slog.Logger, quiet ...string) Middleware {
	quietPaths := make(map[string]bool, len(quiet))
	for _, p := range quiet {
		quietPaths[p] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			// r is passed on unchanged so the ServeMux below can record the
			// matched pattern on it.
			next.ServeHTTP(sw, r)

			level := slog.LevelInfo
			if quietPaths[r.URL.Path] && sw.Status() < http.StatusBadRequest {
				level = slog.LevelDebug
			}
			logger.Log(r.Context(), level, "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"route", r.Pattern,
				"status", sw.Status(),
				"bytes", sw.bytes,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

// statusWriter records the status code and body size of a response.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Status returns the response status, defaulting to 200 like net/http does.
func (w *statusWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// Unwrap lets http.ResponseController reach the underlying writer (flush,
// write deadlines for long-running profiling endpoints, ...).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
