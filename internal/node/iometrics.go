package node

// Per-volume I/O metrics: the node side of the wiring.
//
// The node plugin is the only place in this driver that can answer "how busy is
// this volume": the appliance publishes no per-dataset or per-zvol series at
// all (verified on hardware, see internal/obs/volumeio.go). What the node
// contributes here is not the reading itself — that is podmon's IOStats — but
// the two things only it knows: WHICH paths belong to a volume this driver
// staged, and WHAT workload the volume was published for.
//
// Both halves matter. Discovering mounts from /proc would also report Longhorn's
// and local-path's volumes as this driver's, and a series that is not retired at
// unstage would keep every node reporting every volume it has ever hosted.

import (
	"context"
	"path"
	"strings"
	"sync"

	"github.com/piwi3910/truenas-csi/internal/obs"
)

// Publish-context keys the node reads for metric labels only.
//
// The pod keys are filled in by the kubelet at NodePublishVolume because the
// CSIDriver object sets podInfoOnMount: true. The claim keys are NOT: the
// external-provisioner passes them to CreateVolume, so they reach the node only
// if a PersistentVolume carries them in its volumeAttributes. They are read
// anyway, so a hand-written PV — or a later controller that echoes them into
// the volume context — labels its metrics without another change here.
const (
	// KeyPodName and KeyPodNamespace are the pod the volume is being published
	// for. A pod may only mount a claim from its own namespace, so the pod
	// namespace IS the claim's namespace.
	KeyPodName      = "csi.storage.k8s.io/pod.name"
	KeyPodNamespace = "csi.storage.k8s.io/pod.namespace"
	// KeyPVCName is the claim name, when the volume context happens to carry it.
	KeyPVCName = "csi.storage.k8s.io/pvc/name"
)

// VolumeIOSink receives the per-volume I/O series' lifecycle.
// *obs.VolumeIOCollector implements it; the interface keeps the node testable
// without a Prometheus registry.
type VolumeIOSink interface {
	// Track begins exporting a staged volume.
	Track(obs.VolumeIOLabels)
	// Attach records the workload a volume was published for.
	Attach(volumeID, namespace, pod, pvc string)
	// Forget stops exporting a volume.
	Forget(volumeID string)
}

// IOSampleFunc reads one volume's cumulative kernel counters. mountPath is the
// staging mount; devicePath is set only for a raw block volume, which has no
// filesystem mount to look up.
type IOSampleFunc func(mountPath, devicePath string) (obs.VolumeIOSample, bool)

// ioTarget is where a staged volume's counters live on this node.
type ioTarget struct {
	mountPath  string
	devicePath string
}

// EnableIOMetrics turns on per-volume I/O metrics. Both arguments come from the
// process that owns the metrics registry, so this package keeps depending on
// nothing but obs.
//
// The collector's own sample callback must be Node.SampleVolumeIO, which is
// what resolves a volume id to the paths below.
func (n *Node) EnableIOMetrics(sink VolumeIOSink, sample IOSampleFunc) {
	n.ioMu.Lock()
	defer n.ioMu.Unlock()
	n.ioSink, n.ioSample = sink, sample
	if n.ioTargets == nil {
		n.ioTargets = map[string]ioTarget{}
	}
}

// SampleVolumeIO reads the counters of one volume staged on this node. It is
// called once per volume per Prometheus scrape and only ever reads procfs
// files: it must never stat the mount, because the failure the health monitor
// exists for is exactly the one that makes statfs(2) block for ever, and a
// scrape that hangs takes the node's metrics down with the storage.
func (n *Node) SampleVolumeIO(volumeID string) (obs.VolumeIOSample, bool) {
	n.ioMu.Lock()
	t, ok := n.ioTargets[volumeID]
	sample := n.ioSample
	n.ioMu.Unlock()
	if !ok || sample == nil {
		return obs.VolumeIOSample{}, false
	}
	return sample(t.mountPath, t.devicePath)
}

// trackIO starts exporting a volume's counters. Best-effort throughout: a
// metric that cannot be labelled must never fail a mount.
func (n *Node) trackIO(ctx context.Context, req StageRequest, protocol string) {
	n.ioMu.Lock()
	sink := n.ioSink
	if sink != nil {
		if n.ioTargets == nil {
			n.ioTargets = map[string]ioTarget{}
		}
		n.ioTargets[req.VolumeID] = ioTarget{
			mountPath:  healthPathOf(req),
			devicePath: n.blockDeviceFor(ctx, req, protocol),
		}
	}
	n.ioMu.Unlock()
	if sink == nil {
		return
	}
	sink.Track(obs.VolumeIOLabels{
		VolumeID:         req.VolumeID,
		PersistentVolume: pvNameOf(req.VolumeID),
		PVC:              req.PublishContext[KeyPVCName],
		Namespace:        req.PublishContext[KeyPodNamespace],
		Pod:              req.PublishContext[KeyPodName],
		Protocol:         protocol,
	})
}

// attachIO labels a volume's series with the workload it was published for.
func (n *Node) attachIO(req PublishRequest) {
	n.ioMu.Lock()
	sink := n.ioSink
	n.ioMu.Unlock()
	if sink == nil {
		return
	}
	sink.Attach(req.VolumeID,
		req.PublishContext[KeyPodNamespace],
		req.PublishContext[KeyPodName],
		req.PublishContext[KeyPVCName])
}

// forgetIO retires a volume's series at unstage.
//
// Without it the series would go stale for ever: the volume is gone from this
// node, but its last counter values would be scraped indefinitely and every
// node that had ever hosted it would report it. The honest consequence is that
// when a pod reschedules, the volume's series moves from one node's exporter to
// another's — a new series starting from the new node's kernel counters, which
// is a reset. rate() copes; a dashboard must sum rates rather than rate a sum.
func (n *Node) forgetIO(volumeID string) {
	n.ioMu.Lock()
	sink := n.ioSink
	delete(n.ioTargets, volumeID)
	n.ioMu.Unlock()
	if sink != nil {
		sink.Forget(volumeID)
	}
}

// blockDeviceFor names the block device of a raw block volume, which has no
// filesystem mount for the counters to be found through.
//
// For iSCSI this is derived from the NAA without running anything: the by-id
// link the stage path already waited for is a symlink onto the sdX or dm-N name
// /proc/diskstats uses, and IOStats follows it. For NVMe the mapping from
// serial to device needs `nvme list`, which is run once here rather than on
// every scrape. A failure yields no device and therefore no metrics for that
// volume — never a failed stage.
func (n *Node) blockDeviceFor(ctx context.Context, req StageRequest, protocol string) string {
	if !req.VolumeCapability.Block {
		return "" // a mounted volume is found through the mount table
	}
	switch protocol {
	case ProtocolISCSI:
		if id := normalizeNAA(req.PublishContext[KeyNAA]); id != "" {
			return byIDDir + "/scsi-3" + id
		}
	case ProtocolNVMe:
		if serial := req.PublishContext[KeySerial]; serial != "" {
			if dev, err := resolveNVMeDevice(ctx, n.exec, n.hostRoot(), serial); err == nil {
				return dev
			}
		}
	}
	return ""
}

// pvNameOf is the PersistentVolume name inside a CSI volume handle
// (<backend>/<protocol>/<pool>/<dataset path>/<pv name>). It is the label that
// joins these metrics to kube-state-metrics, which is how a claim name is
// recovered without the node plugin being given API-server credentials.
func pvNameOf(volumeID string) string {
	if volumeID == "" || !strings.Contains(volumeID, "/") {
		return volumeID
	}
	return path.Base(volumeID)
}

// ioMetricsState is the per-node bookkeeping the functions above share. It is a
// separate struct so Node's own definition stays about the data path.
type ioMetricsState struct {
	ioMu      sync.Mutex
	ioSink    VolumeIOSink
	ioSample  IOSampleFunc
	ioTargets map[string]ioTarget
}
