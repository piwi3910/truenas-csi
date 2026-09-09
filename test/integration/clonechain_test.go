package integration

import (
	"context"
	"strings"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestE2EDeleteSourceOfARestoredClone covers the most ordinary thing an
// operator does with a snapshot: restore it, check the restored copy, then
// delete the original.
//
// ZFS makes that awkward. The restored volume is a CLONE of the source's
// snapshot, so destroying the source means destroying a snapshot something
// still depends on, and ZFS refuses. What must not happen is a confusing
// middleware error on a PersistentVolumeClaim that then sits in Terminating
// for ever with no indication of which volume is holding it.
func TestE2EDeleteSourceOfARestoredClone(t *testing.T) {
	e := requireAppliance(t)
	ctx := context.Background()

	reg, err := backend.NewRegistry(ctx, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	ctrl := csi.NewControllerWithNodes(reg, e.cfg, e.resolver(t))

	src, err := ctrl.CreateVolume(ctx, &csipb.CreateVolumeRequest{
		Name: uniqueName("chain-src"), Parameters: e.params("nfs"),
		CapacityRange:      &csipb.CapacityRange{RequiredBytes: 1 << 30},
		VolumeCapabilities: caps(),
	})
	if err != nil {
		t.Fatalf("CreateVolume(source): %v", err)
	}
	srcID := src.GetVolume().GetVolumeId()

	snap, err := ctrl.CreateSnapshot(ctx, &csipb.CreateSnapshotRequest{
		Name: uniqueName("chain-snap"), SourceVolumeId: srcID,
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snapID := snap.GetSnapshot().GetSnapshotId()

	clone, err := ctrl.CreateVolume(ctx, &csipb.CreateVolumeRequest{
		Name: uniqueName("chain-clone"), Parameters: e.params("nfs"),
		CapacityRange:      &csipb.CapacityRange{RequiredBytes: 1 << 30},
		VolumeCapabilities: caps(),
		VolumeContentSource: &csipb.VolumeContentSource{
			Type: &csipb.VolumeContentSource_Snapshot{
				Snapshot: &csipb.VolumeContentSource_SnapshotSource{SnapshotId: snapID}}},
	})
	if err != nil {
		t.Fatalf("CreateVolume(from snapshot): %v", err)
	}
	cloneID := clone.GetVolume().GetVolumeId()

	// Tear down in the order that always works, whatever the assertions below
	// find: the clone first, then the snapshot, then the source.
	t.Cleanup(func() {
		_, _ = ctrl.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{VolumeId: cloneID})
		_, _ = ctrl.DeleteSnapshot(ctx, &csipb.DeleteSnapshotRequest{SnapshotId: snapID})
		_, _ = ctrl.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{VolumeId: srcID})
	})

	// Deleting the source is what this test is about.
	_, err = ctrl.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{VolumeId: srcID})
	if err == nil {
		// Succeeding is acceptable only if the clone really still works: ZFS
		// would have had to promote or otherwise break the dependency.
		if _, gerr := ctrl.ValidateVolumeCapabilities(ctx, &csipb.ValidateVolumeCapabilitiesRequest{
			VolumeId: cloneID, VolumeCapabilities: caps(),
		}); gerr != nil {
			t.Fatalf("deleting the source took the restored volume with it: %v", gerr)
		}
		return
	}

	// It failed, which is the expected ZFS behaviour. The requirement is that
	// the refusal is legible: FailedPrecondition, and the message must name the
	// volume that is holding the source so an operator knows what to delete.
	if code := status.Code(err); code != codes.FailedPrecondition {
		t.Errorf("deleting a volume whose snapshot has a dependent clone returned %s; "+
			"want FailedPrecondition so the CO stops retrying and the operator sees why.\n"+
			"  got: %v", code, err)
	}
	if !strings.Contains(err.Error(), "clone") && !strings.Contains(err.Error(), cloneID) {
		t.Errorf("the refusal does not say what is holding the volume, so a claim "+
			"stuck in Terminating gives no way to find the cause.\n  got: %v", err)
	}

	// And the restored volume must be untouched either way.
	if _, gerr := ctrl.ValidateVolumeCapabilities(ctx, &csipb.ValidateVolumeCapabilitiesRequest{
		VolumeId: cloneID, VolumeCapabilities: caps(),
	}); gerr != nil {
		t.Errorf("the restored volume is gone after a failed delete of its source: %v", gerr)
	}
}
