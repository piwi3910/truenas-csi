package obs

// Per-volume performance metrics.
//
// Dell's CSM asks the array for per-volume IOPS, bandwidth and latency. TrueNAS
// cannot answer that question: verified against the appliance on 2026-09-08,
// `reporting.netdata_graphs` returns 40 graphs — cpu, memory, per-PHYSICAL-DISK
// I/O, interfaces, load, uptime, ARC counters, disk temperature and UPS — and
// not one of them is per-dataset, per-zvol or per-pool. See
// .procoder/notes/truenas-api-findings.md.
//
// The numbers do exist, on the node where the volume is actually used: the
// kernel counts every read and write it issues. So this driver measures there
// instead, and the trade is deliberate rather than a consolation:
//
//   - it costs the appliance NOTHING. Dell's design polls each array every
//     10–20 seconds and needed a concurrency cap after their own bug report
//     about session exhaustion; this reads three local files.
//   - only MOUNTED volumes are visible. A volume that is attached but not
//     mounted, or idle on a node that never staged it, reports nothing at all —
//     where an array-side view would still describe it.
//   - a volume's series MOVES between node exporters when its pod reschedules.
//     The counters are the new node's kernel counters, starting from zero, so
//     the move looks like a counter reset on a new series. `rate()` handles the
//     reset; a dashboard summing across nodes must use sum(rate(...)) rather
//     than rate(sum(...)).
//
// Everything here is a COUNTER carrying the kernel's own cumulative value. A
// gauge of "current IOPS" computed by this exporter would be strictly worse: it
// would pick an averaging window nobody asked for, lose every event between two
// scrapes, and hide resets. Latency is exported the same way — cumulative time
// spent, never a ratio — so PromQL divides two rates over the window the user
// chose, exactly as node_exporter models /proc/diskstats.

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Label cardinality is bounded by the number of volumes currently STAGED on
// this node — MaxVolumesPerNode, 128 — because a series exists only while an
// entry does, and Forget removes the entry at unstage. Every metric below is a
// const metric built during Collect from that live set, so an unstaged volume
// cannot leave a stale series behind the way a GaugeVec would.
var volumeIOLabelNames = []string{
	"volume_id_hash", "persistentvolume", "pvc", "namespace", "pod", "protocol",
}

var (
	volumeReadOpsDesc = prometheus.NewDesc("truenas_csi_volume_read_ops_total",
		"Read operations completed against a volume mounted on this node, as the node's kernel counted them.",
		volumeIOLabelNames, nil)
	volumeWriteOpsDesc = prometheus.NewDesc("truenas_csi_volume_write_ops_total",
		"Write operations completed against a volume mounted on this node, as the node's kernel counted them.",
		volumeIOLabelNames, nil)
	volumeReadBytesDesc = prometheus.NewDesc("truenas_csi_volume_read_bytes_total",
		"Bytes read from a volume mounted on this node.",
		volumeIOLabelNames, nil)
	volumeWriteBytesDesc = prometheus.NewDesc("truenas_csi_volume_write_bytes_total",
		"Bytes written to a volume mounted on this node.",
		volumeIOLabelNames, nil)
	volumeReadSecondsDesc = prometheus.NewDesc("truenas_csi_volume_read_seconds_total",
		"Cumulative time spent on reads: service time for a block volume "+
			"(/proc/diskstats), RPC round-trip time for an NFS volume. Divide its "+
			"rate by the rate of truenas_csi_volume_read_ops_total for average latency.",
		volumeIOLabelNames, nil)
	volumeWriteSecondsDesc = prometheus.NewDesc("truenas_csi_volume_write_seconds_total",
		"Cumulative time spent on writes, on the same terms as "+
			"truenas_csi_volume_read_seconds_total.",
		volumeIOLabelNames, nil)
	volumeIOBusySecondsDesc = prometheus.NewDesc("truenas_csi_volume_io_busy_seconds_total",
		"Cumulative time the volume's block device had I/O in flight (io_ticks). "+
			"Its rate is device utilisation, 0 to 1. Block volumes only.",
		volumeIOLabelNames, nil)
)

// volumeIOFailures is a plain counter rather than a const metric: it describes
// the exporter, not a volume, so it must survive the volume being unstaged.
var volumeIOFailures = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "truenas_csi_volume_io_sample_failures_total",
	Help: "Scrapes that could not read kernel I/O counters for a staged volume, by protocol.",
}, []string{"protocol"})

// VolumeIOSample is one reading of a volume's cumulative kernel I/O counters.
//
// Every field is cumulative since the mount (or the device) appeared, and is
// exported unmodified. The Has* flags exist because the four data paths answer
// different questions: a block device reports operations, bytes, service time
// and busy time; an NFS mount reports operations, bytes and round-trip time but
// no busy time; an SMB mount reports bytes and nothing else.
type VolumeIOSample struct {
	// ReadOps and WriteOps are completed operations.
	ReadOps, WriteOps float64
	// ReadBytes and WriteBytes are bytes moved.
	ReadBytes, WriteBytes float64
	// ReadSeconds and WriteSeconds are cumulative time spent, NOT an average.
	ReadSeconds, WriteSeconds float64
	// BusySeconds is cumulative time with at least one request in flight.
	BusySeconds float64
	// HasOps, HasLatency and HasBusy say which of the above the source could
	// answer. A field that was not measured is omitted rather than exported as
	// zero, because a flat zero counter reads as "nothing is happening" when the
	// truth is "this protocol does not count that".
	HasOps, HasLatency, HasBusy bool
}

// VolumeIOLabels identifies the volume a sample belongs to.
//
// Where these come from matters, because the node plugin deliberately talks to
// neither the appliance nor the API server:
//
//   - PersistentVolume is the last component of the CSI volume handle, which is
//     the PV name this driver provisions the dataset under.
//   - Namespace and Pod arrive in the volume context at NodePublishVolume
//     because the CSIDriver sets podInfoOnMount: true. A pod may only mount a
//     claim from its own namespace, so Namespace IS the claim's namespace.
//   - PVC is filled in only when the publish context actually carries the
//     claim name (a hand-written PersistentVolume, or a future controller that
//     echoes it into the volume context). The external-provisioner passes the
//     claim name to CreateVolume, not to the node, and inventing a lookup for it
//     would mean giving every node plugin API-server or appliance credentials.
//     When it is empty, join on `persistentvolume` against kube-state-metrics'
//     kube_persistentvolume_claim_ref — see docs/metrics.md.
type VolumeIOLabels struct {
	VolumeID         string
	PersistentVolume string
	PVC              string
	Namespace        string
	Pod              string
	Protocol         string
}

// values renders the label set in the order of volumeIOLabelNames.
func (l VolumeIOLabels) values() []string {
	return []string{
		HashVolumeID(l.VolumeID), l.PersistentVolume, l.PVC, l.Namespace, l.Pod, l.Protocol,
	}
}

// VolumeIOCollector exports per-volume kernel I/O counters for the volumes this
// node has staged.
//
// It is a prometheus.Collector rather than a set of promauto vectors on
// purpose. Const metrics are produced from the live volume set on every scrape,
// so a volume that has been unstaged stops being exported the moment it is
// forgotten — no stale series, and no node reporting every volume it has ever
// hosted.
type VolumeIOCollector struct {
	sample func(volumeID string) (VolumeIOSample, bool)

	mu      sync.RWMutex
	volumes map[string]VolumeIOLabels
}

// NewVolumeIOCollector builds a collector that reads counters through sample,
// which is called once per tracked volume per scrape and must not block: it is
// expected to read local files only, never to stat a mount that may be hung.
func NewVolumeIOCollector(sample func(volumeID string) (VolumeIOSample, bool)) *VolumeIOCollector {
	return &VolumeIOCollector{sample: sample, volumes: map[string]VolumeIOLabels{}}
}

// Track starts exporting a volume, or updates what is known about one. It is
// called from NodeStageVolume, where the protocol and the volume handle are
// known but the workload is not yet.
func (c *VolumeIOCollector) Track(l VolumeIOLabels) {
	if l.VolumeID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Stage may run again for an already-published volume (the kubelet retries
	// it after a restart), so pod identity learned at publish is preserved
	// rather than blanked by the later stage.
	if prev, ok := c.volumes[l.VolumeID]; ok {
		if l.Namespace == "" {
			l.Namespace = prev.Namespace
		}
		if l.Pod == "" {
			l.Pod = prev.Pod
		}
		if l.PVC == "" {
			l.PVC = prev.PVC
		}
	}
	c.volumes[l.VolumeID] = l
}

// Attach records the workload a staged volume was published for. Called from
// NodePublishVolume, which is the first — and only — point where the node is
// told anything about the pod.
func (c *VolumeIOCollector) Attach(volumeID, namespace, pod, pvc string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.volumes[volumeID]
	if !ok {
		return // never staged here; nothing to label
	}
	if namespace != "" {
		l.Namespace = namespace
	}
	if pod != "" {
		l.Pod = pod
	}
	if pvc != "" {
		l.PVC = pvc
	}
	c.volumes[volumeID] = l
}

// Forget stops exporting a volume. Called from NodeUnstageVolume: without it
// the series would go stale forever and every node that had ever hosted the
// volume would keep reporting it.
func (c *VolumeIOCollector) Forget(volumeID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.volumes, volumeID)
}

// Tracked reports how many volumes are currently exported.
func (c *VolumeIOCollector) Tracked() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.volumes)
}

// Describe implements prometheus.Collector.
func (c *VolumeIOCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		volumeReadOpsDesc, volumeWriteOpsDesc,
		volumeReadBytesDesc, volumeWriteBytesDesc,
		volumeReadSecondsDesc, volumeWriteSecondsDesc,
		volumeIOBusySecondsDesc,
	} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *VolumeIOCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	tracked := make([]VolumeIOLabels, 0, len(c.volumes))
	for _, l := range c.volumes {
		tracked = append(tracked, l)
	}
	c.mu.RUnlock()

	for _, l := range tracked {
		s, ok := c.sample(l.VolumeID)
		if !ok {
			// A volume can be legitimately unreadable for a moment — staged but
			// not yet mounted, or a device that has just gone away — so this is
			// counted, not logged per scrape.
			volumeIOFailures.WithLabelValues(l.Protocol).Inc()
			continue
		}
		vals := l.values()
		counter := func(d *prometheus.Desc, v float64) {
			ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, vals...)
		}
		counter(volumeReadBytesDesc, s.ReadBytes)
		counter(volumeWriteBytesDesc, s.WriteBytes)
		if s.HasOps {
			counter(volumeReadOpsDesc, s.ReadOps)
			counter(volumeWriteOpsDesc, s.WriteOps)
		}
		if s.HasLatency {
			counter(volumeReadSecondsDesc, s.ReadSeconds)
			counter(volumeWriteSecondsDesc, s.WriteSeconds)
		}
		if s.HasBusy {
			counter(volumeIOBusySecondsDesc, s.BusySeconds)
		}
	}
}

// RegisterVolumeIO adds the collector to the registry the driver's /metrics
// endpoint serves. It is separate from construction so a test can collect from
// a registry of its own.
func RegisterVolumeIO(c *VolumeIOCollector) error {
	return prometheus.Register(c)
}
