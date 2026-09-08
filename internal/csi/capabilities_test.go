package csi

import (
	"context"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/node"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestEveryAdvertisedControllerCapabilityIsImplemented.
//
// Advertising a capability the driver answers with Unimplemented is worse than
// not advertising it: the CO stops treating the RPC as optional and reports the
// failure as a driver fault instead of as a missing feature. csi-sanity catches
// some of these; it does not exercise every one, so the probe table below does.
//
// The default case is the point of the test: a capability added to
// ControllerGetCapabilities without a probe here fails immediately, so nobody
// can advertise something without saying which call implements it.
func TestEveryAdvertisedControllerCapabilityIsImplemented(t *testing.T) {
	c := volumeServer(t, healthyPool, ownedFilesystem("Pool0/k8s/pvc-a", 1<<30, 1<<30, ""))
	ctx := context.Background()

	caps, err := c.ControllerGetCapabilities(ctx, &csipb.ControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("ControllerGetCapabilities: %v", err)
	}
	// probe calls the RPC that implements a capability. Any error but
	// Unimplemented is fine — these are deliberately minimal requests, and what
	// is under test is that the method exists at all.
	probe := func(name string, err error) {
		t.Helper()
		if status.Code(err) == codes.Unimplemented {
			t.Errorf("%s is advertised but %s", name, err)
		}
	}
	seen := map[csipb.ControllerServiceCapability_RPC_Type]bool{}
	for _, cap := range caps.GetCapabilities() {
		typ := cap.GetRpc().GetType()
		if seen[typ] {
			t.Errorf("capability %s is advertised twice", typ)
		}
		seen[typ] = true

		switch typ {
		case csipb.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
			// A clone is a CreateVolume with a content source, not an RPC of
			// its own, so the same probe covers it.
			csipb.ControllerServiceCapability_RPC_CLONE_VOLUME:
			_, err := c.CreateVolume(ctx, &csipb.CreateVolumeRequest{})
			probe(typ.String(), err)
			_, err = c.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{})
			probe(typ.String(), err)
		case csipb.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT:
			_, err := c.CreateSnapshot(ctx, &csipb.CreateSnapshotRequest{})
			probe(typ.String(), err)
			_, err = c.DeleteSnapshot(ctx, &csipb.DeleteSnapshotRequest{})
			probe(typ.String(), err)
		case csipb.ControllerServiceCapability_RPC_LIST_SNAPSHOTS:
			_, err := c.ListSnapshots(ctx, &csipb.ListSnapshotsRequest{})
			probe(typ.String(), err)
		case csipb.ControllerServiceCapability_RPC_LIST_VOLUMES:
			_, err := c.ListVolumes(ctx, &csipb.ListVolumesRequest{})
			probe(typ.String(), err)
		case csipb.ControllerServiceCapability_RPC_LIST_VOLUMES_PUBLISHED_NODES:
			// The capability promises a VolumeStatus on every entry, so an
			// entry without one is the failure mode, not a missing method.
			resp, err := c.ListVolumes(ctx, &csipb.ListVolumesRequest{})
			probe(typ.String(), err)
			for _, e := range resp.GetEntries() {
				if e.GetStatus() == nil {
					t.Errorf("%s is advertised but entry %q carries no status",
						typ, e.GetVolume().GetVolumeId())
				}
			}
		case csipb.ControllerServiceCapability_RPC_EXPAND_VOLUME:
			_, err := c.ControllerExpandVolume(ctx, &csipb.ControllerExpandVolumeRequest{})
			probe(typ.String(), err)
		case csipb.ControllerServiceCapability_RPC_GET_CAPACITY:
			_, err := c.GetCapacity(ctx, &csipb.GetCapacityRequest{})
			probe(typ.String(), err)
		case csipb.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME:
			_, err := c.ControllerPublishVolume(ctx, &csipb.ControllerPublishVolumeRequest{})
			probe(typ.String(), err)
			_, err = c.ControllerUnpublishVolume(ctx, &csipb.ControllerUnpublishVolumeRequest{})
			probe(typ.String(), err)
		case csipb.ControllerServiceCapability_RPC_GET_VOLUME:
			_, err := c.ControllerGetVolume(ctx, &csipb.ControllerGetVolumeRequest{VolumeId: getVol})
			probe(typ.String(), err)
		case csipb.ControllerServiceCapability_RPC_MODIFY_VOLUME:
			// Reached with no mutable parameters on purpose: the probe is
			// about the method existing, and an empty class is the one request
			// that must succeed against every volume.
			_, err := c.ControllerModifyVolume(ctx,
				&csipb.ControllerModifyVolumeRequest{VolumeId: getVol})
			probe(typ.String(), err)
		case csipb.ControllerServiceCapability_RPC_GET_VOLUME_HEALTH:
			_, err := c.ControllerGetVolumeHealth(ctx,
				&csipb.ControllerGetVolumeHealthRequest{VolumeId: getVol})
			probe(typ.String(), err)
		case csipb.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER:
			// Not an RPC: it declares that the two SINGLE_NODE_*_WRITER access
			// modes are understood, which is only true if every protocol
			// accepts them.
			for protocol := range protocolAccessClass {
				for _, m := range []csipb.VolumeCapability_AccessMode_Mode{
					csipb.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
					csipb.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
				} {
					if !supportsAccessMode(protocol, m) {
						t.Errorf("%s is advertised but protocol %q refuses %s", typ, protocol, m)
					}
				}
			}
		default:
			t.Errorf("capability %s is advertised with no probe in this test: add the call "+
				"that implements it, or stop advertising it", typ)
		}
	}

	// The gaps this task closed, named explicitly so removing one is a failure
	// rather than a silently shorter list.
	for _, want := range []csipb.ControllerServiceCapability_RPC_Type{
		csipb.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
		csipb.ControllerServiceCapability_RPC_GET_VOLUME,
		csipb.ControllerServiceCapability_RPC_GET_VOLUME_HEALTH,
		csipb.ControllerServiceCapability_RPC_LIST_VOLUMES_PUBLISHED_NODES,
		// MODIFY_VOLUME: dropping it makes every VolumeAttributesClass inert
		// without any error anywhere — the resizer simply stops calling.
		csipb.ControllerServiceCapability_RPC_MODIFY_VOLUME,
	} {
		if !seen[want] {
			t.Errorf("%s must be advertised", want)
		}
	}
}

// TestNodeAdvertisesSingleNodeMultiWriter: both halves of the declaration are
// required. A CO that sees the controller's half but not the node's keeps
// sending the legacy SINGLE_NODE_WRITER mode, and ReadWriteOncePod stays
// unusable.
func TestNodeAdvertisesSingleNodeMultiWriter(t *testing.T) {
	srv := NewNode(node.NewNode("worker-1", &node.Preflight{}, nil))
	caps, err := srv.NodeGetCapabilities(context.Background(),
		&csipb.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("NodeGetCapabilities: %v", err)
	}
	for _, cap := range caps.GetCapabilities() {
		if cap.GetRpc().GetType() == csipb.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER {
			return
		}
	}
	t.Fatal("the node plugin must advertise SINGLE_NODE_MULTI_WRITER as well as the controller")
}

// TestPluginAdvertisesOnlineAndOfflineExpansion.
//
// OFFLINE is claimed because it is true: every backend's Expand is an appliance
// -side property change that needs no attachment. This test pins the claim next
// to the reason, so removing the reason (an Expand that starts requiring a live
// attachment) shows up as a capability that has to be reconsidered.
func TestPluginAdvertisesOnlineAndOfflineExpansion(t *testing.T) {
	caps, err := NewIdentity(nil).GetPluginCapabilities(context.Background(),
		&csipb.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetPluginCapabilities: %v", err)
	}
	want := map[csipb.PluginCapability_VolumeExpansion_Type]bool{
		csipb.PluginCapability_VolumeExpansion_ONLINE:  false,
		csipb.PluginCapability_VolumeExpansion_OFFLINE: false,
	}
	for _, cap := range caps.GetCapabilities() {
		if e := cap.GetVolumeExpansion(); e != nil {
			want[e.GetType()] = true
		}
	}
	for typ, found := range want {
		if !found {
			t.Errorf("volume expansion %s must be advertised", typ)
		}
	}
}
