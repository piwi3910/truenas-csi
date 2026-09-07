package obs

import (
	"net/http"
	"sync/atomic"
)

// ready flips once and stays set: readiness gates startup, not steady-state health.
// Connection loss after that is reported by truenas_csi_backend_up, not by evicting the
// pod from its Service endpoints.
var ready atomic.Bool

// MarkReady records that a backend has connected at least once.
func MarkReady() { ready.Store(true) }

// Ready reports whether MarkReady has been called.
func Ready() bool { return ready.Load() }

// HealthHandler serves /healthz. It answers 200 for as long as the process serves
// requests at all.
func HealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
}

// ReadyHandler serves /readyz. It answers 503 until MarkReady has been called at least
// once, then 200.
func ReadyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("no backend has connected\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
}
