package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/piwi3910/truenas-csi/internal/obs"
)

// mkfifo makes a named pipe, whose open-for-read blocks until a writer arrives.
func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }

// stageRecordHost writes a host whose mount table holds one staged volume and
// one published block volume, each with its record beside it.
func stageRecordHost(t *testing.T, records []stageRecord, paths []string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	table := ""
	for i, p := range paths {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(records[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(StageRecordPath(full), b, 0o600); err != nil {
			t.Fatal(err)
		}
		table += "/dev/sdb " + full + " ext4 rw 0 0\n"
	}
	// A mount that is not this driver's. Nothing beside it, so nothing to find.
	table += "/dev/sda / ext4 rw 0 0\n"
	if err := os.WriteFile(filepath.Join(root, "proc", "mounts"), []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestVolumesStagedBeforeTheProcessStartedAreMonitored is the whole point.
//
// The monitor's targets live in memory and are registered by NodeStageVolume
// alone. The kubelet's own state says an already-staged volume is staged, so it
// never calls NodeStageVolume for it again — which means that without recovery,
// every restart of the node plugin stops watching every volume already on the
// node, silently, for as long as those volumes live.
func TestVolumesStagedBeforeTheProcessStartedAreMonitored(t *testing.T) {
	const staged = "nas1/iscsi/Pool0/k8s/pvc-a"
	root := stageRecordHost(t,
		[]stageRecord{{VolumeID: staged, Kind: recordStage, PublishContext: map[string]string{
			KeyProtocol: ProtocolISCSI, KeyPortal: "192.168.10.253:3260",
			KeyIQN: "iqn.x:csi", KeyNAA: "0xabc", KeyLUN: "3"}}},
		[]string{"var/lib/kubelet/plugins/kubernetes.io/csi/hash/globalmount"})

	n := &Node{Root: root, pre: &Preflight{Found: map[Capability]bool{}}, health: NewHealthMonitor()}
	if got := n.RecoverStagedVolumes(context.Background()); got != 1 {
		t.Fatalf("recovered %d volumes, want 1", got)
	}
	tgt, ok := n.health.Target(staged)
	if !ok {
		t.Fatal("the volume is mounted on this node and the monitor is not watching it; " +
			"its health metric and its volume condition are both gone until it happens " +
			"to be staged again, which for a running pod is never")
	}
	if tgt.Protocol != ProtocolISCSI || tgt.LUN != "3" || tgt.NAA != "0xabc" {
		t.Errorf("recovered target lost what it needs to check identity: %+v", tgt)
	}
	if filepath.Base(tgt.Path) != "globalmount" {
		t.Errorf("recovered target has no path to stat: %q", tgt.Path)
	}
}

// TestRecoveryPrefersTheStagingMountOverThePodsMount is why the record says
// which call wrote it.
//
// A filesystem volume is mounted twice: at its staging path, which lives as
// long as the volume is on this node, and at the pod's path, which goes away
// with the pod. Monitoring the pod's path would report a perfectly healthy
// volume as unreachable the moment its pod restarted. Nothing in the two paths
// distinguishes them except names the CO chose, so the record says it outright.
func TestRecoveryPrefersTheStagingMountOverThePodsMount(t *testing.T) {
	const id = "nas1/nfs/Pool0/k8s/pvc-a"
	pc := map[string]string{KeyProtocol: ProtocolNFS, KeyServer: "192.168.10.253",
		KeyExport: "/mnt/Pool0/k8s/pvc-a"}
	// The staging mount is listed FIRST and the pod's mount second, so a scan
	// that simply keeps the last entry it sees would keep the pod's — which is
	// exactly the mistake this preference exists to prevent.
	root := stageRecordHost(t,
		[]stageRecord{
			{VolumeID: id, Kind: recordStage, PublishContext: pc},
			{VolumeID: id, Kind: recordPublish, PublishContext: pc},
		},
		[]string{
			"var/lib/kubelet/plugins/kubernetes.io/csi/hash/globalmount",
			"var/lib/kubelet/pods/uid/volumes/kubernetes.io~csi/pvc-a/mount",
		})

	n := &Node{Root: root, pre: &Preflight{Found: map[Capability]bool{}}, health: NewHealthMonitor()}
	if got := n.RecoverStagedVolumes(context.Background()); got != 1 {
		t.Fatalf("recovered %d volumes, want 1 — the two mounts are one volume", got)
	}
	tgt, ok := n.health.Target(id)
	if !ok {
		t.Fatal("the volume was not recovered at all")
	}
	if filepath.Base(tgt.Path) != "globalmount" {
		t.Errorf("the monitor is watching %q, the pod's own mount; when that pod restarts "+
			"the path disappears and a healthy volume is reported unreachable", tgt.Path)
	}
}

// TestRecoveryIgnoresMountsThatAreNotOurs keeps the scan from claiming another
// driver's mounts. The record file is the proof of ownership, not the path.
func TestRecoveryIgnoresMountsThatAreNotOurs(t *testing.T) {
	root := stageRecordHost(t, nil, nil)
	n := &Node{Root: root, pre: &Preflight{Found: map[Capability]bool{}}, health: NewHealthMonitor()}
	if got := n.RecoverStagedVolumes(context.Background()); got != 0 {
		t.Fatalf("claimed %d mounts that carry no record of ours", got)
	}
}

// TestAnOldStageRecordStillUnstages is the compatibility this cannot break.
//
// A node upgraded into this version has records on disk in the previous, flat
// shape. They must keep working for the thing they exist for — telling unstage
// which target to log out of — even though they carry no volume id and so
// cannot be recovered.
func TestAnOldStageRecordStillUnstages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "globalmount")
	old := map[string]string{KeyPortal: "192.168.10.253:3260", KeyIQN: "iqn.x:csi"}
	b, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StageRecordPath(path), b, 0o600); err != nil {
		t.Fatal(err)
	}
	pc := ReadStageRecord(path)
	if pc[KeyIQN] != "iqn.x:csi" || pc[KeyPortal] != "192.168.10.253:3260" {
		t.Fatalf("a record written by the previous version no longer reads back; unstaging "+
			"every volume it staged would leave the iSCSI session behind: %v", pc)
	}
	if r, ok := readStageRecord(path); !ok || r.VolumeID != "" {
		t.Errorf("an old record must read back with no volume id, not a wrong one: %+v", r)
	}
}

// TestARoundTripKeepsBothHalves pins the current shape.
func TestARoundTripKeepsBothHalves(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "globalmount")
	pc := map[string]string{KeyPortal: "p", KeyIQN: "i", KeyNAA: "0xabc"}
	if err := WriteStageRecord(path, "nas1/iscsi/Pool0/k8s/pvc-a", pc); err != nil {
		t.Fatal(err)
	}
	r, ok := readStageRecord(path)
	if !ok || r.VolumeID != "nas1/iscsi/Pool0/k8s/pvc-a" {
		t.Fatalf("volume id did not survive the round trip: %+v", r)
	}
	if r.PublishContext[KeyNAA] != "0xabc" {
		t.Fatalf("publish context did not survive the round trip: %+v", r)
	}
}

// TestRecoveryNeverStatsANetworkMount is about where this code runs.
//
// Recovery happens at startup, before the plugin serves anything, and it walks
// the host's whole mount table. A stat on a hung NFS mount does not return —
// that is the precise failure the health monitor exists to report — so a stat
// there would hang the entire node plugin rather than one probe. Only the
// device filesystems are ever looked at, which is all a raw block publish can
// be.
func TestRecoveryNeverStatsANetworkMount(t *testing.T) {
	var stattedPaths []string
	restore := blockDeviceNumber
	t.Cleanup(func() { blockDeviceNumber = restore })
	blockDeviceNumber = func(path string) (uint64, bool) {
		stattedPaths = append(stattedPaths, path)
		return 0, false
	}

	n := &Node{Root: "/nonexistent", pre: &Preflight{Found: map[Capability]bool{}}}
	for _, tc := range []struct {
		fsType   string
		mayStat  bool
		whatItIs string
	}{
		{"nfs4", false, "an NFS mount, which can hang forever"},
		{"nfs", false, "an NFS mount, which can hang forever"},
		{"cifs", false, "an SMB mount, which can hang forever"},
		{"ext4", false, "a filesystem volume, which is never a device node"},
		{"devtmpfs", true, "a raw block publish"},
	} {
		stattedPaths = nil
		n.blockDeviceOfMount(mountEntry{source: "srv:/export", target: "/mnt/x", fsType: tc.fsType})
		if got := len(stattedPaths) > 0; got != tc.mayStat {
			t.Errorf("fstype %q is %s: statted=%v, want %v",
				tc.fsType, tc.whatItIs, got, tc.mayStat)
		}
	}
}

// TestAStuckMountCannotStopTheNodePluginStarting is about the blast radius of
// somebody else's problem.
//
// Reading a record means reading a file in the DIRECTORY HOLDING a mount point,
// and recovery walks every mount on the host, including other software's. That
// directory can itself lie inside a third party's hung network mount, where
// open(2) never returns. Recovery runs before the plugin serves anything, so
// unbounded it would turn one stuck mount belonging to somebody else into a
// node plugin that never starts — and a node whose CSI driver never registers
// mounts nothing at all.
func TestAStuckMountCannotStopTheNodePluginStarting(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A FIFO where a record would be. Opening it for reading blocks until
	// somebody opens the write end, which nobody ever will — the same shape as
	// a read into a hung mount, and the closest a test can get to one.
	stuck := filepath.Join(root, "stuck")
	if err := os.MkdirAll(filepath.Dir(StageRecordPath(stuck)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := mkfifo(StageRecordPath(stuck)); err != nil {
		t.Skipf("cannot create a fifo here (%v)", err)
	}
	table := "/dev/sdb " + stuck + " ext4 rw 0 0\n"
	if err := os.WriteFile(filepath.Join(root, "proc", "mounts"), []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}

	restore := scanTimeout
	t.Cleanup(func() { scanTimeout = restore })
	scanTimeout = 250 * time.Millisecond

	n := &Node{Root: root, pre: &Preflight{Found: map[Capability]bool{}}, health: NewHealthMonitor()}
	done := make(chan int, 1)
	go func() { done <- n.RecoverStagedVolumes(context.Background()) }()

	select {
	case got := <-done:
		if got != 0 {
			t.Errorf("recovered %d volumes from a mount that never answered", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RecoverStagedVolumes did not return: one stuck mount anywhere on the host " +
			"stops the node plugin from ever starting, and a node whose CSI driver never " +
			"registers can mount nothing at all")
	}
}

// TestRecoveryKeepsTheWorkloadLabels is why both records are read.
//
// The kubelet passes a pod's name, namespace and uid in NodePublishVolume
// alone, so the staging record has never seen them. Recovering from the staging
// record by itself brings a volume's I/O series back with no pod on it — which
// is how the per-volume metrics join to the workload, and without it a
// dashboard shows traffic belonging to nobody.
func TestRecoveryKeepsTheWorkloadLabels(t *testing.T) {
	const id = "nas1/iscsi/Pool0/k8s/pvc-a"
	stagePC := map[string]string{
		KeyProtocol: ProtocolISCSI, KeyPortal: "192.168.10.253:3260",
		KeyIQN: "iqn.x:csi", KeyNAA: "0xabc", KeyLUN: "3",
		KeyPVCName: "data-web-0",
	}
	publishPC := map[string]string{}
	for k, v := range stagePC {
		publishPC[k] = v
	}
	publishPC[KeyPodName] = "web-0"
	publishPC[KeyPodNamespace] = "shop"

	root := stageRecordHost(t,
		[]stageRecord{
			{VolumeID: id, Kind: recordStage, PublishContext: stagePC},
			{VolumeID: id, Kind: recordPublish, PublishContext: publishPC},
		},
		[]string{
			"var/lib/kubelet/plugins/kubernetes.io/csi/hash/globalmount",
			"var/lib/kubelet/pods/uid/volumes/kubernetes.io~csi/pvc-a/mount",
		})

	n := &Node{Root: root, pre: &Preflight{Found: map[Capability]bool{}}, health: NewHealthMonitor()}
	sink := &recordingIOSink{}
	n.EnableIOMetrics(sink, nil)
	if got := n.RecoverStagedVolumes(context.Background()); got != 1 {
		t.Fatalf("recovered %d volumes, want 1", got)
	}
	tgt, ok := n.health.Target(id)
	if !ok {
		t.Fatal("the volume was not recovered")
	}
	// The path still comes from the staging mount, which outlives the pod.
	if filepath.Base(tgt.Path) != "globalmount" {
		t.Errorf("the monitor is watching %q rather than the staging mount", tgt.Path)
	}
	// And the identity check still has what it needs.
	if tgt.LUN != "3" || tgt.NAA != "0xabc" {
		t.Errorf("recovered target lost its device identity: %+v", tgt)
	}
	// The labels are the half that only the publish record carries.
	if len(sink.tracked) != 1 {
		t.Fatalf("the recovered volume was not exported at all: %+v", sink.tracked)
	}
	got := sink.tracked[0]
	if got.Pod != "web-0" || got.Namespace != "shop" {
		t.Errorf("the volume's I/O series came back with no pod on it (%+v); the kubelet "+
			"passes pod identity in NodePublishVolume alone, so it can only come from the "+
			"publish record", got)
	}
	if got.PVC != "data-web-0" {
		t.Errorf("lost the claim name: %+v", got)
	}
}

// recordingIOSink captures what the node exports, so a test can assert on the
// labels rather than on the fact that a call was made.
type recordingIOSink struct{ tracked []obs.VolumeIOLabels }

func (s *recordingIOSink) Track(l obs.VolumeIOLabels) { s.tracked = append(s.tracked, l) }
func (s *recordingIOSink) Attach(_, _, _, _ string)   {}
func (s *recordingIOSink) Forget(string)              {}

// TestTheHostIsScannedOnceAtStartup keeps a delay out of the worst moment.
//
// Both recoveries want the same answer — the health monitor's targets and the
// single-writer reservation — and both run before the plugin serves anything,
// while the liveness probe is already counting. The scan is bounded, so
// repeating it doubles the worst case for an answer that cannot have changed:
// nothing this process does has started yet.
func TestTheHostIsScannedOnceAtStartup(t *testing.T) {
	root := stageRecordHost(t,
		[]stageRecord{{VolumeID: "v", Kind: recordStage, PublishContext: map[string]string{
			KeyProtocol: ProtocolNFS, KeyServer: "192.168.10.253"}}},
		[]string{"var/lib/kubelet/plugins/kubernetes.io/csi/hash/globalmount"})

	n := &Node{Root: root, pre: &Preflight{Found: map[Capability]bool{}}, health: NewHealthMonitor()}
	n.RecoverStagedVolumes(context.Background())
	n.PublishedTargets(context.Background())

	// Removing the mount table would make a SECOND real scan return nothing.
	// The cached answer must be unaffected.
	if err := os.Remove(filepath.Join(root, "proc", "mounts")); err != nil {
		t.Fatal(err)
	}
	entries, err := n.startupMountScan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the host was scanned again (got %d entries, want the 1 from the first "+
			"scan); both startup recoveries pay the bounded scan's worst case instead of "+
			"one of them", len(entries))
	}
}
