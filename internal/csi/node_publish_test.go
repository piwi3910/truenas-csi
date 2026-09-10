package csi

import (
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/node"
)

// TestSingleWriterPublicationsAreExclusive.
//
// SINGLE_NODE_SINGLE_WRITER promises one writer in one workload; the node
// plugin is the only participant that sees both target paths, so it is the only
// one that can keep the promise. This is the enforcement that makes the
// SINGLE_NODE_MULTI_WRITER capability honest, and csi-sanity fails the whole
// suite without it.
func TestSingleWriterPublicationsAreExclusive(t *testing.T) {
	const (
		vol   = "nas1/iscsi/Pool0/k8s/pvc-a"
		other = "nas1/iscsi/Pool0/k8s/pvc-b"
		one   = "/var/lib/kubelet/pods/a/volumes/kubernetes.io~csi/pv/mount"
		two   = "/var/lib/kubelet/pods/b/volumes/kubernetes.io~csi/pv/mount"
	)
	const (
		single = csipb.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER
		multi  = csipb.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER
	)

	type step struct {
		volume, target string
		mode           csipb.VolumeCapability_AccessMode_Mode
		release        bool
		wantAllowed    bool
	}
	cases := []struct {
		name  string
		steps []step
	}{
		{"first publication is always allowed", []step{
			{volume: vol, target: one, mode: single, wantAllowed: true},
		}},
		{"republishing the same target is idempotent", []step{
			{volume: vol, target: one, mode: single, wantAllowed: true},
			{volume: vol, target: one, mode: single, wantAllowed: true},
		}},
		{"a second target is refused", []step{
			{volume: vol, target: one, mode: single, wantAllowed: true},
			{volume: vol, target: two, mode: single, wantAllowed: false},
		}},
		// Either side of the pair forbids it: the existing mount's promise is
		// broken by the new one, and vice versa.
		{"single writer arriving beside a multi writer mount", []step{
			{volume: vol, target: one, mode: multi, wantAllowed: true},
			{volume: vol, target: two, mode: single, wantAllowed: false},
		}},
		{"multi writer arriving beside a single writer mount", []step{
			{volume: vol, target: one, mode: single, wantAllowed: true},
			{volume: vol, target: two, mode: multi, wantAllowed: false},
		}},
		// SINGLE_NODE_MULTI_WRITER exists precisely so several pods on one node
		// can share the mount.
		{"multi writer admits several targets", []step{
			{volume: vol, target: one, mode: multi, wantAllowed: true},
			{volume: vol, target: two, mode: multi, wantAllowed: true},
		}},
		{"unpublishing frees the volume again", []step{
			{volume: vol, target: one, mode: single, wantAllowed: true},
			{volume: vol, target: one, release: true},
			{volume: vol, target: two, mode: single, wantAllowed: true},
		}},
		// The ledger is per volume: one volume's single writer says nothing
		// about another's.
		{"other volumes are unaffected", []step{
			{volume: vol, target: one, mode: single, wantAllowed: true},
			{volume: other, target: two, mode: single, wantAllowed: true},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPublishedTargets()
			for i, s := range tc.steps {
				if s.release {
					p.release(s.volume, s.target)
					continue
				}
				blocker, allowed := p.reserve(s.volume, s.target, s.mode)
				if allowed != s.wantAllowed {
					t.Fatalf("step %d: reserve(%s, %s) allowed = %t, want %t (blocker %q)",
						i, s.volume, s.target, allowed, s.wantAllowed, blocker)
				}
				if !allowed && blocker == "" {
					t.Fatalf("step %d: a refusal must name the target path that caused it, "+
						"or an operator cannot find the other mount", i)
				}
			}
		})
	}
}

// TestSingleWriterSurvivesARestartOfTheNodePlugin covers the enforcement that
// used to disappear without a trace.
//
// The reservation map is what makes SINGLE_NODE_SINGLE_WRITER enforceable, and
// it lives in memory. It used to be justified by the claim that the kubelet
// re-issues NodePublishVolume for every mounted volume after a plugin restart.
// It does not: measured on a real cluster, a plugin restarted while a volume
// was mounted and its pod running received zero NodeStageVolume and zero
// NodePublishVolume calls, only NodeGetVolumeStats. So after every restart the
// map was empty and the driver granted the second writer it advertises that it
// refuses.
func TestSingleWriterSurvivesARestartOfTheNodePlugin(t *testing.T) {
	const (
		volumeID = "nas1/iscsi/Pool0/k8s/pvc-a"
		first    = "/var/lib/kubelet/pods/uid-1/volumes/kubernetes.io~csi/pvc-a/mount"
		second   = "/var/lib/kubelet/pods/uid-2/volumes/kubernetes.io~csi/pvc-a/mount"
	)
	p := newPublishedTargets()
	// What the previous process had published, as the records beside the
	// still-mounted target paths describe it.
	p.recover([]node.PublishedTarget{{
		VolumeID:   volumeID,
		TargetPath: first,
		AccessMode: int32(csipb.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER),
	}})

	if other, ok := p.reserve(volumeID, second,
		csipb.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER); ok {
		t.Fatal("a second single-writer publication was granted; the volume is already " +
			"published elsewhere on this node and the driver advertises that it refuses this")
	} else if other != first {
		t.Errorf("the refusal names %q, want the publication that forbids it, %q", other, first)
	}

	// Republishing the SAME target must still succeed: the CO retries
	// NodePublishVolume freely and the call is required to be idempotent.
	if _, ok := p.reserve(volumeID, first,
		csipb.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER); !ok {
		t.Error("refused a republish of the target this node had already published, " +
			"which the CO does routinely and which must be a no-op")
	}
}
