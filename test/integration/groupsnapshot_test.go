package integration

import (
	"context"
	"strings"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/csi"
	"github.com/piwi3910/truenas-csi/internal/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestE2EVolumeGroupSnapshot exercises the GroupController against the real
// appliance.
//
// It is the only controller capability with no hardware coverage at all: the
// cluster cannot reach it, because the VolumeGroupSnapshot CRDs are an opt-in
// part of external-snapshotter that most clusters (including the one this
// driver is developed against) do not install. That leaves the whole path —
// recursive snapshot, member discovery, per-member sizes, deletion — resting on
// a fake that has repeatedly been more permissive than the middleware.
//
// The group's crash-consistency guarantee is the reason it must be a RECURSIVE
// snapshot of a common parent and not a loop: one transaction group, one
// instant. That only works when every member shares a parent dataset, which is
// what namespaced volumes give, so this provisions two volumes into one
// namespace and snapshots the namespace.
func TestE2EVolumeGroupSnapshot(t *testing.T) {
	e := requireAppliance(t)
	ctx := context.Background()

	reg, err := backend.NewRegistry(ctx, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	ctrl := csi.NewControllerWithNodes(reg, e.cfg, e.resolver(t))
	gc := csi.NewGroupController(reg, e.cfg)

	ns := uniqueName("grp")
	params := e.params("nfs")
	params[volume.ParamPVCNamespace] = ns

	var ids []string
	for _, name := range []string{uniqueName("pvc-a"), uniqueName("pvc-b")} {
		resp, cErr := ctrl.CreateVolume(ctx, &csipb.CreateVolumeRequest{
			Name: name, Parameters: params,
			CapacityRange:      &csipb.CapacityRange{RequiredBytes: 1 << 30},
			VolumeCapabilities: caps(),
		})
		if cErr != nil {
			t.Fatalf("CreateVolume %s: %v", name, cErr)
		}
		id := resp.GetVolume().GetVolumeId()
		ids = append(ids, id)
		t.Cleanup(func() {
			_, _ = ctrl.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{VolumeId: id})
		})
	}

	gname := uniqueName("groupsnap")
	created, err := gc.CreateVolumeGroupSnapshot(ctx, &csipb.CreateVolumeGroupSnapshotRequest{
		Name: gname, SourceVolumeIds: ids,
	})
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot: %v", err)
	}
	gs := created.GetGroupSnapshot()
	t.Cleanup(func() {
		_, _ = gc.DeleteVolumeGroupSnapshot(ctx, &csipb.DeleteVolumeGroupSnapshotRequest{
			GroupSnapshotId: gs.GetGroupSnapshotId(), SnapshotIds: memberIDs(gs),
		})
	})

	if !gs.GetReadyToUse() {
		t.Error("the group snapshot is not ready to use; ZFS snapshots are instant")
	}
	if got := len(gs.GetSnapshots()); got != len(ids) {
		t.Fatalf("the group has %d members, want %d: %v", got, len(ids), memberIDs(gs))
	}
	if gs.GetCreationTime() == nil || gs.GetCreationTime().AsTime().IsZero() {
		t.Error("the group snapshot has no creation time")
	}
	for _, m := range gs.GetSnapshots() {
		if m.GetSizeBytes() != 1<<30 {
			t.Errorf("member %s reports size %d, want the source's provisioned 1 GiB",
				m.GetSnapshotId(), m.GetSizeBytes())
		}
		if m.GetGroupSnapshotId() != gs.GetGroupSnapshotId() {
			t.Errorf("member %s does not carry its group id", m.GetSnapshotId())
		}
		if m.GetCreationTime() == nil || m.GetCreationTime().AsTime().IsZero() {
			t.Errorf("member %s has no creation time", m.GetSnapshotId())
		}
	}

	// A second create under the same name must converge, not fail: the CO
	// retries forever, and a recursive snapshot that already exists is an
	// error on the appliance.
	again, err := gc.CreateVolumeGroupSnapshot(ctx, &csipb.CreateVolumeGroupSnapshotRequest{
		Name: gname, SourceVolumeIds: ids,
	})
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot is not idempotent: %v", err)
	}
	if a, b := again.GetGroupSnapshot().GetCreationTime(), gs.GetCreationTime(); !a.AsTime().Equal(b.AsTime()) {
		t.Errorf("the retry reports creation time %s, want the first call's %s — "+
			"a sidecar reads a changed timestamp as a different snapshot",
			a.AsTime(), b.AsTime())
	}

	got, err := gc.GetVolumeGroupSnapshot(ctx, &csipb.GetVolumeGroupSnapshotRequest{
		GroupSnapshotId: gs.GetGroupSnapshotId(), SnapshotIds: memberIDs(gs),
	})
	if err != nil {
		t.Fatalf("GetVolumeGroupSnapshot: %v", err)
	}
	if n := len(got.GetGroupSnapshot().GetSnapshots()); n != len(ids) {
		t.Errorf("GetVolumeGroupSnapshot returned %d members, want %d", n, len(ids))
	}
	if got.GetGroupSnapshot().GetCreationTime().AsTime().IsZero() {
		t.Error("GetVolumeGroupSnapshot returned no creation time for the group")
	}

	if _, err := gc.DeleteVolumeGroupSnapshot(ctx, &csipb.DeleteVolumeGroupSnapshotRequest{
		GroupSnapshotId: gs.GetGroupSnapshotId(), SnapshotIds: memberIDs(gs),
	}); err != nil {
		t.Fatalf("DeleteVolumeGroupSnapshot: %v", err)
	}
	// Deleting again is success, per CSI.
	if _, err := gc.DeleteVolumeGroupSnapshot(ctx, &csipb.DeleteVolumeGroupSnapshotRequest{
		GroupSnapshotId: gs.GetGroupSnapshotId(), SnapshotIds: memberIDs(gs),
	}); err != nil && status.Code(err) != codes.NotFound {
		t.Errorf("deleting an absent group snapshot must succeed, got: %v", err)
	}
	if _, err := gc.GetVolumeGroupSnapshot(ctx, &csipb.GetVolumeGroupSnapshotRequest{
		GroupSnapshotId: gs.GetGroupSnapshotId(), SnapshotIds: memberIDs(gs),
	}); status.Code(err) != codes.NotFound {
		t.Errorf("a deleted group snapshot must be NotFound, got: %v", err)
	}
}

// TestE2EGroupSnapshotLeavesBystandersAlone pins the blast radius of the
// recursive snapshot the group is built from.
//
// ZFS has no way to snapshot a SUBSET of a parent's children atomically:
// recursive:true takes the parent and everything under it. Measured on
// hardware before the fix, a group snapshot of two volumes created four
// snapshots — one of them on a volume that was never a member — while the CO
// was shown two. With a flat layout every volume on the backend shares the
// parent, so "group snapshot of two PVCs" meant "snapshot the whole appliance",
// and the later DeleteVolumeGroupSnapshot destroyed all of them.
func TestE2EGroupSnapshotLeavesBystandersAlone(t *testing.T) {
	e := requireAppliance(t)
	ctx := context.Background()

	reg, err := backend.NewRegistry(ctx, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	ctrl := csi.NewControllerWithNodes(reg, e.cfg, e.resolver(t))
	gc := csi.NewGroupController(reg, e.cfg)

	var ids []string
	var bystander string
	for i, n := range []string{uniqueName("member-a"), uniqueName("member-b"), uniqueName("bystander")} {
		resp, cErr := ctrl.CreateVolume(ctx, &csipb.CreateVolumeRequest{
			Name: n, Parameters: e.params("nfs"),
			CapacityRange:      &csipb.CapacityRange{RequiredBytes: 1 << 30},
			VolumeCapabilities: caps(),
		})
		if cErr != nil {
			t.Fatalf("CreateVolume %s: %v", n, cErr)
		}
		id := resp.GetVolume().GetVolumeId()
		t.Cleanup(func() {
			_, _ = ctrl.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{VolumeId: id})
		})
		if i < 2 {
			ids = append(ids, id)
			continue
		}
		parsed, pErr := volume.ParseID(id)
		if pErr != nil {
			t.Fatal(pErr)
		}
		bystander = parsed.DatasetPath()
	}

	name := uniqueName("groupsnap")
	created, err := gc.CreateVolumeGroupSnapshot(ctx, &csipb.CreateVolumeGroupSnapshotRequest{
		Name: name, SourceVolumeIds: ids,
	})
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot: %v", err)
	}
	gs := created.GetGroupSnapshot()
	t.Cleanup(func() {
		_, _ = gc.DeleteVolumeGroupSnapshot(ctx, &csipb.DeleteVolumeGroupSnapshotRequest{
			GroupSnapshotId: gs.GetGroupSnapshotId(), SnapshotIds: memberIDs(gs),
		})
	})

	// What the APPLIANCE holds, not what the CO was told.
	snaps, err := e.client.SnapshotList(ctx, e.prefix)
	if err != nil {
		t.Fatal(err)
	}
	var onBystander []string
	var total int
	for _, s := range snaps {
		if !strings.Contains(s.ID, name) {
			continue
		}
		total++
		if strings.HasPrefix(s.ID, bystander+"@") {
			onBystander = append(onBystander, s.ID)
		}
	}
	if len(onBystander) > 0 {
		t.Errorf("the group snapshot also snapshotted %s, which is not a member: %v",
			bystander, onBystander)
	}
	// The two members plus the parent anchor that carries the group id.
	if want := len(ids) + 1; total != want {
		t.Errorf("the appliance holds %d snapshots named %s, want %d "+
			"(one per member, plus the group's own anchor)", total, name, want)
	}
}

// TestE2ENamespacedGroupSnapshot covers the layout a group snapshot is
// actually FOR, and which was refused outright before.
//
// Namespaced volumes share <pool>/<parent>/<namespace>, which is a dedicated
// parent: a recursive snapshot of it captures that namespace's volumes and
// nothing else. groupParent required the group's parent to be the configured
// parentDataset exactly, so it rejected every namespaced group with
// "outside backend's parent dataset" — leaving group snapshots supported only
// in the flat layout, where the shared parent is every volume on the appliance.
func TestE2ENamespacedGroupSnapshot(t *testing.T) {
	e := requireAppliance(t)
	ctx := context.Background()

	// A copy of the environment with the namespace layout switched on: it
	// changes where volumes are created, so it must not leak into other tests.
	cfg := *e.cfg
	cfg.Backends = map[string]config.Backend{}
	for k, v := range e.cfg.Backends {
		v.NamespaceQuotas = config.NamespaceQuotas{Enabled: true}
		cfg.Backends[k] = v
	}
	reg, err := backend.NewRegistry(ctx, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	ctrl := csi.NewControllerWithNodes(reg, &cfg, e.resolver(t))
	gc := csi.NewGroupController(reg, &cfg)

	ns := uniqueName("team")
	params := e.params("nfs")
	params[volume.ParamPVCNamespace] = ns

	var ids []string
	for _, n := range []string{uniqueName("pvc-a"), uniqueName("pvc-b")} {
		resp, cErr := ctrl.CreateVolume(ctx, &csipb.CreateVolumeRequest{
			Name: n, Parameters: params,
			CapacityRange:      &csipb.CapacityRange{RequiredBytes: 1 << 30},
			VolumeCapabilities: caps(),
		})
		if cErr != nil {
			t.Fatalf("CreateVolume %s: %v", n, cErr)
		}
		id := resp.GetVolume().GetVolumeId()
		if !strings.Contains(id, "/"+ns+"/") {
			t.Fatalf("volume %s is not under the namespace dataset; the layout is off "+
				"and this test would not exercise what it claims", id)
		}
		ids = append(ids, id)
		t.Cleanup(func() {
			_, _ = ctrl.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{VolumeId: id})
		})
	}
	t.Cleanup(func() {
		_ = e.client.DatasetDelete(ctx, e.prefix+"/"+ns, true, true)
	})

	created, err := gc.CreateVolumeGroupSnapshot(ctx, &csipb.CreateVolumeGroupSnapshotRequest{
		Name: uniqueName("nsgroup"), SourceVolumeIds: ids,
	})
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot for namespaced volumes: %v", err)
	}
	gs := created.GetGroupSnapshot()
	t.Cleanup(func() {
		_, _ = gc.DeleteVolumeGroupSnapshot(ctx, &csipb.DeleteVolumeGroupSnapshotRequest{
			GroupSnapshotId: gs.GetGroupSnapshotId(), SnapshotIds: memberIDs(gs),
		})
	})
	if n := len(gs.GetSnapshots()); n != len(ids) {
		t.Errorf("the group has %d members, want %d", n, len(ids))
	}
	// The anchor must be the NAMESPACE dataset, not the backend's parent.
	if !strings.HasPrefix(gs.GetGroupSnapshotId(), "nas1/"+e.prefix+"/"+ns+"@") {
		t.Errorf("the group is anchored at %q, want the namespace dataset %q",
			gs.GetGroupSnapshotId(), e.prefix+"/"+ns)
	}
}

func memberIDs(gs *csipb.VolumeGroupSnapshot) []string {
	out := make([]string, 0, len(gs.GetSnapshots()))
	for _, m := range gs.GetSnapshots() {
		out = append(out, m.GetSnapshotId())
	}
	return out
}
