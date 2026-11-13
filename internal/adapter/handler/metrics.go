package handler

import (
	"net/http"
	"time"
)

// RequestObserver records served HTTP requests.
type RequestObserver interface {
	ObserveRequest(server, route string, status int, d time.Duration)
}

// PartnerObserver records partner API outcomes.
type PartnerObserver interface {
	AuthFailed(code string)
	PartnerLookup(outcome string)
}

type nopObserver struct{}

func (nopObserver) ObserveRequest(string, string, int, time.Duration) {}
func (nopObserver) AuthFailed(string)                                 {}
func (nopObserver) PartnerLookup(string)                              {}

// Instrument reports every request with the matched route pattern (never the
// raw path, which would make label cardinality unbounded).
func Instrument(server string, obs RequestObserver) Middleware {
	if obs == nil {
		obs = nopObserver{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r) // r unchanged: the mux records r.Pattern on it
			obs.ObserveRequest(server, r.Pattern, sw.Status(), time.Since(start))
		})
	}
}

func partnerObserver(o PartnerObserver) PartnerObserver {
	if o == nil {
		return nopObserver{}
	}
	return o
}
