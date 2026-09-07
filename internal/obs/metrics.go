package obs

import (
	"crypto/sha256"
	"encoding/hex"
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

// Volume and data-path health, reported by the node plugin's health monitor.
//
// The volume label is a hash, not the volume id: a cluster churns PVCs, and a
// raw id label would grow one series per volume ever staged on the node and
// never retire it. Twelve hex characters of SHA-256 are enough to correlate a
// series with the log line that carries the full id, and cheap to compute.
var (
	volumeHealth = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "truenas_csi_volume_health",
		Help: "1 when a staged volume's data path answered its last bounded check, 0 when it did not.",
	}, []string{"volume_id_hash", "protocol"})

	backendReachable = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "truenas_csi_node_backend_reachable",
		Help: "1 when this node's last bounded probe of the backend's data address succeeded, 0 when it did not.",
	}, []string{"backend"})
)

// HashVolumeID returns the short, stable label form of a volume id. It is a
// label value; the full id belongs in the log line next to it.
func HashVolumeID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:12]
}

// SetVolumeHealth records the result of one volume's data-path check.
func SetVolumeHealth(volumeID, protocol string, healthy bool) {
	volumeHealth.WithLabelValues(HashVolumeID(volumeID), protocol).Set(boolGauge(healthy))
}

// ForgetVolumeHealth retires a volume's series. Without it an unstaged volume
// would report its last known state forever and the series set would only ever
// grow.
func ForgetVolumeHealth(volumeID, protocol string) {
	volumeHealth.DeleteLabelValues(HashVolumeID(volumeID), protocol)
}

// SetBackendReachable records whether this node's data path to a backend
// answered. It is deliberately distinct from SetBackendUp, which describes the
// controller's middleware websocket: a node can lose NFS or iSCSI while the
// controller's API connection is perfectly healthy.
func SetBackendReachable(backend string, reachable bool) {
	backendReachable.WithLabelValues(backend).Set(boolGauge(reachable))
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
