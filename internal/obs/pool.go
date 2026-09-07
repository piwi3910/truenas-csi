package obs

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Appliance diagnostics gauges. Label cardinality stays bounded: backend names,
// pool names and disk device names are all fixed by the hardware, and the scrub
// state and alert level label values come from the closed sets below.
var (
	poolHealthy = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "truenas_appliance_pool_healthy",
		Help: "1 when the appliance reports the pool healthy, 0 otherwise.",
	}, []string{"backend", "pool"})

	poolSizeBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "truenas_appliance_pool_size_bytes",
		Help: "Total size of the pool as the appliance reports it.",
	}, []string{"backend", "pool"})

	poolFreeBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "truenas_appliance_pool_free_bytes",
		Help: "Free space in the pool as the appliance reports it.",
	}, []string{"backend", "pool"})

	poolFragmentation = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "truenas_appliance_pool_fragmentation_percent",
		Help: "ZFS fragmentation of the pool, in percent.",
	}, []string{"backend", "pool"})

	scrubState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "truenas_appliance_scrub_state",
		Help: "1 for the pool's current scrub state, 0 for every other state.",
	}, []string{"backend", "pool", "state"})

	scrubErrors = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "truenas_appliance_scrub_errors",
		Help: "Errors reported by the pool's last completed scrub.",
	}, []string{"backend", "pool"})

	diskHealthy = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "truenas_appliance_disk_healthy",
		Help: "1 when the disk's SMART summary is healthy, 0 when it is not.",
	}, []string{"backend", "disk"})

	applianceAlerts = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "truenas_appliance_alerts",
		Help: "Active alerts on the appliance, by severity level.",
	}, []string{"backend", "level"})
)

// ScrubStates is the closed set of scrub states published as label values.
// Publishing the whole set, with zeroes for the states the pool is not in, is
// what lets an alert rule say "scrub has not been FINISHED for N days" without
// the series disappearing when the state changes.
var ScrubStates = []string{"NONE", "SCANNING", "FINISHED", "CANCELED", "UNKNOWN"}

// AlertLevels is the closed set of alert levels published as label values.
var AlertLevels = []string{"INFO", "NOTICE", "WARNING", "ERROR", "CRITICAL", "ALERT", "EMERGENCY"}

// SetPoolCapacity records a pool's health and capacity.
func SetPoolCapacity(backend, pool string, healthy bool, size, free int64, fragmentationPercent float64) {
	poolHealthy.WithLabelValues(backend, pool).Set(boolGauge(healthy))
	poolSizeBytes.WithLabelValues(backend, pool).Set(float64(size))
	poolFreeBytes.WithLabelValues(backend, pool).Set(float64(free))
	poolFragmentation.WithLabelValues(backend, pool).Set(fragmentationPercent)
}

// SetScrubState records which state a pool's scrub is in, zeroing the others so
// a stale state cannot keep reading as current.
func SetScrubState(backend, pool, state string, errs int64) {
	known := false
	for _, s := range ScrubStates {
		if s == state {
			known = true
			break
		}
	}
	for _, s := range ScrubStates {
		v := 0.0
		if s == state || (!known && s == "UNKNOWN") {
			v = 1
		}
		scrubState.WithLabelValues(backend, pool, s).Set(v)
	}
	scrubErrors.WithLabelValues(backend, pool).Set(float64(errs))
}

// SetDiskHealthy records one disk's health.
func SetDiskHealthy(backend, disk string, healthy bool) {
	diskHealthy.WithLabelValues(backend, disk).Set(boolGauge(healthy))
}

// SetApplianceAlerts records the active alert count per level, zeroing levels
// that currently have none so a cleared alert does not leave a stale series.
func SetApplianceAlerts(backend string, byLevel map[string]int) {
	for _, l := range AlertLevels {
		applianceAlerts.WithLabelValues(backend, l).Set(float64(byLevel[l]))
	}
	for l, n := range byLevel {
		applianceAlerts.WithLabelValues(backend, l).Set(float64(n))
	}
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
