package node

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/obs"
)

// fakeIOSink records the lifecycle calls the node makes, which is what the
// exported series' correctness rests on.
type fakeIOSink struct {
	mu       sync.Mutex
	tracked  []obs.VolumeIOLabels
	attached [][4]string
	forgot   []string
}

func (f *fakeIOSink) Track(l obs.VolumeIOLabels) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tracked = append(f.tracked, l)
}

func (f *fakeIOSink) Attach(volumeID, namespace, pod, pvc string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attached = append(f.attached, [4]string{volumeID, namespace, pod, pvc})
}

func (f *fakeIOSink) Forget(volumeID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgot = append(f.forgot, volumeID)
}

// TestIOMetricsFollowTheVolumeLifecycle is the wiring guard. The reader and the
// collector can both be perfect while nothing is ever tracked, and the symptom
// — a metrics endpoint with no volume series on it — looks exactly like an idle
// cluster.
func TestIOMetricsFollowTheVolumeLifecycle(t *testing.T) {
	root := hostRoot(t)
	e := &fakeExec{}
	n := newTestNode(t, root, e, CapNFS)
	sink := &fakeIOSink{}
	var sampled [][2]string
	n.EnableIOMetrics(sink, func(mount, device string) (obs.VolumeIOSample, bool) {
		sampled = append(sampled, [2]string{mount, device})
		return obs.VolumeIOSample{ReadBytes: 42}, true
	})

	staging := filepath.Join(t.TempDir(), "globalmount")
	const volumeID = "nas1/nfs/tank/k8s/pvc-abc"
	stage := StageRequest{
		VolumeID:         volumeID,
		StagingPath:      staging,
		PublishContext:   nfsContext(),
		VolumeCapability: VolumeCapability{FsType: "nfs"},
	}
	if err := n.Stage(context.Background(), stage); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if len(sink.tracked) != 1 {
		t.Fatalf("Stage must track the volume, got %d calls", len(sink.tracked))
	}
	got := sink.tracked[0]
	// The PV name comes out of the volume handle: it is the only claim-side
	// identity the node holds before a pod turns up.
	if got.PersistentVolume != "pvc-abc" || got.Protocol != ProtocolNFS {
		t.Fatalf("stage labels = %+v", got)
	}
	if got.Namespace != "" || got.Pod != "" {
		t.Fatalf("stage cannot know the pod yet, got %+v", got)
	}

	// The counters are read from the STAGING mount. The pod's own path is a
	// bind mount of it and shares one set of kernel counters, so sampling the
	// staging path answers for every pod that has the volume published.
	if _, ok := n.SampleVolumeIO(volumeID); !ok {
		t.Fatal("a staged volume must be sampleable")
	}
	if len(sampled) != 1 || sampled[0][0] != staging || sampled[0][1] != "" {
		t.Fatalf("sampled = %v, want the staging path and no device", sampled)
	}

	// podInfoOnMount puts the pod in the volume context of NodePublishVolume,
	// and of no other call.
	publish := PublishRequest{
		VolumeID:    volumeID,
		StagingPath: staging,
		TargetPath:  filepath.Join(t.TempDir(), "mount"),
		PublishContext: map[string]string{
			KeyProtocol:     ProtocolNFS,
			KeyPodName:      "postgres-0",
			KeyPodNamespace: "prod",
		},
	}
	if err := n.Publish(context.Background(), publish); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(sink.attached) != 1 || sink.attached[0] != [4]string{volumeID, "prod", "postgres-0", ""} {
		t.Fatalf("publish must attach the pod identity, got %v", sink.attached)
	}

	if err := n.Unstage(context.Background(), UnstageRequest{
		VolumeID: volumeID, StagingPath: staging, PublishContext: nfsContext(),
	}); err != nil {
		t.Fatalf("Unstage: %v", err)
	}
	if len(sink.forgot) != 1 || sink.forgot[0] != volumeID {
		t.Fatalf("unstage must retire the series, got %v", sink.forgot)
	}
	if _, ok := n.SampleVolumeIO(volumeID); ok {
		t.Fatal("an unstaged volume must no longer be sampleable")
	}
}

// TestIOMetricsAreOffUntilEnabled keeps the node's data path independent of the
// metrics: a plugin built without them must stage and unstage unchanged.
func TestIOMetricsAreOffUntilEnabled(t *testing.T) {
	n := newTestNode(t, hostRoot(t), &fakeExec{}, CapNFS)
	staging := filepath.Join(t.TempDir(), "globalmount")
	req := StageRequest{
		VolumeID:         "nas1/nfs/tank/k8s/pvc-abc",
		StagingPath:      staging,
		PublishContext:   nfsContext(),
		VolumeCapability: VolumeCapability{FsType: "nfs"},
	}
	if err := n.Stage(context.Background(), req); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, ok := n.SampleVolumeIO(req.VolumeID); ok {
		t.Fatal("no sampler was installed, so nothing may be sampled")
	}
	n.forgetIO(req.VolumeID) // must not panic on an absent map
}

// TestBlockVolumesAreNamedByDevice covers the raw block path, which has no
// filesystem mount for the counters to be found through. The iSCSI answer is
// derived from the NAA without running anything, because the by-id link the
// stage already waited for resolves onto the diskstats name.
func TestBlockVolumesAreNamedByDevice(t *testing.T) {
	n := newTestNode(t, hostRoot(t), &fakeExec{}, CapISCSI)
	tests := []struct {
		name     string
		block    bool
		protocol string
		ctx      map[string]string
		want     string
	}{
		{
			name:     "iscsi raw block resolves through by-id",
			block:    true,
			protocol: ProtocolISCSI,
			ctx:      map[string]string{KeyNAA: "0x6589cfc000000abc"},
			want:     byIDDir + "/scsi-36589cfc000000abc",
		},
		{
			name:     "a mounted volume is found through the mount table",
			protocol: ProtocolISCSI,
			ctx:      map[string]string{KeyNAA: "0x6589cfc000000abc"},
		},
		{
			name:     "a block volume with no identifier yields no device",
			block:    true,
			protocol: ProtocolISCSI,
			ctx:      map[string]string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := n.blockDeviceFor(context.Background(), StageRequest{
				PublishContext:   tc.ctx,
				VolumeCapability: VolumeCapability{Block: tc.block},
			}, tc.protocol)
			if got != tc.want {
				t.Fatalf("blockDeviceFor = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPVNameOf(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{"nas1/nfs/tank/k8s/pvc-abc", "pvc-abc"},
		{"nas1/iscsi/tank/k8s/team/pvc-1", "pvc-1"},
		{"pvc-standalone", "pvc-standalone"},
		{"", ""},
	} {
		if got := pvNameOf(tc.id); got != tc.want {
			t.Fatalf("pvNameOf(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}
