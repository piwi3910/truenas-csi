package obs

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Label cardinality is deliberately bounded: only method names and backend names ever
// become label values. A volume id or a credential must never appear in a label — the
// first is unbounded, the second is a leak.
var (
	csiCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "truenas_csi_calls_total",
		Help: "Total CSI RPCs served, by method and whether they returned an error.",
	}, []string{"method", "error"})

	csiDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "truenas_csi_call_duration_seconds",
		Help:    "Duration of CSI RPCs, by method.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method"})

	middlewareCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "truenas_csi_middleware_calls_total",
		Help: "Total TrueNAS middleware calls, by method and whether they returned an error.",
	}, []string{"method", "error"})

	middlewareDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "truenas_csi_middleware_duration_seconds",
		Help:    "Duration of TrueNAS middleware calls, by method.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method"})

	backendUp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "truenas_csi_backend_up",
		Help: "1 when the backend's websocket connection is established, 0 otherwise.",
	}, []string{"backend"})

	orphanedVolumes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "truenas_csi_orphaned_volumes",
		Help: "Datasets owned by this driver with no matching PersistentVolume, by backend.",
	}, []string{"backend"})
)

// ObserveCSI records one served CSI RPC.
func ObserveCSI(method string, err error, d time.Duration) {
	csiCalls.WithLabelValues(method, strconv.FormatBool(err != nil)).Inc()
	csiDuration.WithLabelValues(method).Observe(d.Seconds())
}

// ObserveMiddleware records one TrueNAS middleware call.
func ObserveMiddleware(method string, err error, d time.Duration) {
	middlewareCalls.WithLabelValues(method, strconv.FormatBool(err != nil)).Inc()
	middlewareDuration.WithLabelValues(method).Observe(d.Seconds())
}

// SetBackendUp records whether the named backend is currently connected.
func SetBackendUp(backend string, up bool) {
	v := 0.0
	if up {
		v = 1
	}
	backendUp.WithLabelValues(backend).Set(v)
}

// SetOrphanCount records the number of orphaned datasets last observed on a backend.
func SetOrphanCount(backend string, n int) {
	orphanedVolumes.WithLabelValues(backend).Set(float64(n))
}
