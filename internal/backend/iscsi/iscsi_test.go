package iscsi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

func volID(name string) volume.ID {
	return volume.ID{Backend: "nas1", Protocol: "iscsi", Pool: "Pool0", Parent: "k8s", Name: name}
}

func createReq(name string, size int64, params map[string]string) backend.CreateRequest {
	if params == nil {
		params = map[string]string{}
	}
	return backend.CreateRequest{ID: volID(name), CapacityBytes: size, Params: params}
}

func (n *nas) backendWith(c *truenas.Client) *iscsiBackend {
	return New(c, "Pool0", "k8s").(*iscsiBackend)
}

// TestISCSIRegistersItself pins the wiring: the registry finds this backend by
// the StorageClass protocol value, not by an import of this package.
func TestISCSIRegistersItself(t *testing.T) {
	found := false
	for _, p := range backend.Protocols() {
		if p == "iscsi" {
			found = true
		}
	}
	if !found {
		t.Fatalf("iscsi must register itself, registry has %v", backend.Protocols())
	}
}

// TestExtentNameWithinLimit: iscsi.extent.create refuses a name longer than 64
// characters. A volume name long enough to breach it must still provision, and
// two different long names must not collapse onto one extent — a collision
// would map two volumes onto the same LUN.
func TestExtentNameWithinLimit(t *testing.T) {
	long := "pvc-" + strings.Repeat("a", 90)
	other := "pvc-" + strings.Repeat("a", 89) + "b"

	a := extentName(volID(long))
	b := extentName(volID(other))

	if len(a) > 64 {
		t.Fatalf("extent name must fit the 64-character limit, got %d: %q", len(a), a)
	}
	if a == b {
		t.Fatalf("two distinct volume names collapsed onto one extent name %q", a)
	}
	if got := extentName(volID("pvc-1234")); got != extentName(volID("pvc-1234")) {
		t.Fatal("extent names must be deterministic")
	}

	// And the appliance must accept it.
	n := newNAS(t)
	b2 := n.backend()
	ctx := context.Background()
	if _, err := b2.Create(ctx, createReq(long, 1<<30, nil)); err != nil {
		t.Fatalf("Create with a long name: %v", err)
	}
	if _, extents, _, _ := n.counts(); extents != 1 {
		t.Fatalf("want one extent, got %d", extents)
	}
}

// TestISCSICreateIsIdempotent: CSI retries CreateVolume freely. A retry must
// converge on the objects already there instead of stacking a second zvol,
// extent or LUN mapping onto the shared target.
func TestISCSICreateIsIdempotent(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	first, err := b.Create(ctx, createReq("pvc-1", 1<<30, nil))
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second, err := b.Create(ctx, createReq("pvc-1", 1<<30, nil))
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}

	ds, extents, targets, texts := n.counts()
	if ds != 1 || extents != 1 || targets != 1 || texts != 0 {
		t.Fatalf("want one of each object and NO mapping before publish, got datasets=%d extents=%d targets=%d targetextents=%d",
			ds, extents, targets, texts)
	}
	if first.Context["naa"] == "" || first.Context["naa"] != second.Context["naa"] {
		t.Fatalf("a retry must return the same NAA: %q then %q", first.Context["naa"], second.Context["naa"])
	}
	if _, ok := first.Context["lun"]; ok {
		t.Fatalf("CreateVolume must hand out no LUN: the mapping is the fence and belongs to publish, got %v", first.Context)
	}
	for _, k := range []string{"portal", "iqn", "naa"} {
		if first.Context[k] == "" {
			t.Fatalf("publish context is missing %q: %v", k, first.Context)
		}
	}
	if !strings.HasPrefix(first.Context["iqn"], "iqn.2005-10.org.freenas.ctl:") {
		t.Fatalf("iqn must be basename:target, got %q", first.Context["iqn"])
	}

	// A second volume lands on the next LUN of the SAME target, once published.
	if _, err := b.Create(ctx, createReq("pvc-2", 1<<30, nil)); err != nil {
		t.Fatalf("Create pvc-2: %v", err)
	}
	if _, _, targets, _ := n.counts(); targets != 1 {
		t.Fatalf("the target is shared across volumes, got %d targets", targets)
	}
	node := backend.NodeRef{ID: "worker-1", Addrs: []string{"10.0.0.1"}}
	firstPC, err := b.Publish(ctx, volID("pvc-1"), node)
	if err != nil {
		t.Fatalf("Publish pvc-1: %v", err)
	}
	otherPC, err := b.Publish(ctx, volID("pvc-2"), node)
	if err != nil {
		t.Fatalf("Publish pvc-2: %v", err)
	}
	if otherPC["lun"] == firstPC["lun"] {
		t.Fatalf("two volumes share LUN %q", otherPC["lun"])
	}
}

// TestISCSICreateRollsBackOnFailure: a half-built volume leaves a zvol nobody
// will ever reference and nobody will ever delete, because Kubernetes never
// learns its handle. Failure must leave the appliance as it was found.
func TestISCSICreateRollsBackOnFailure(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	n.failOn("iscsi.extent.create", &fake.RPCError{Code: -32001, ErrName: "EFAULT", Reason: "boom"})
	if _, err := b.Create(ctx, createReq("pvc-rb", 1<<30, nil)); err == nil {
		t.Fatal("Create must fail when the extent cannot be made")
	}
	if n.hasDataset("Pool0/k8s/pvc-rb") {
		t.Fatal("the zvol must be rolled back")
	}
	if _, extents, _, _ := n.counts(); extents != 0 {
		t.Fatalf("the extent must be rolled back, got %d", extents)
	}

	// With the failure cleared, the same request must now succeed cleanly.
	n.clearFail("iscsi.extent.create")
	if _, err := b.Create(ctx, createReq("pvc-rb", 1<<30, nil)); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	ds, extents, _, texts := n.counts()
	if ds != 1 || extents != 1 || texts != 0 {
		t.Fatalf("retry must build exactly one volume, got datasets=%d extents=%d targetextents=%d", ds, extents, texts)
	}
}

// TestISCSIDeleteVerifiesOwnership is the pool's last line of defence. The
// appliance holds ~20 TiB of irreplaceable data; a delete that trusts the
// volume handle alone would destroy any dataset whose name happens to match.
// Only a marker set LOCALLY on the zvol itself is proof — inheritance from the
// parent dataset is not.
func TestISCSIDeleteVerifiesOwnership(t *testing.T) {
	cases := []struct {
		name    string
		seed    map[string]any
		wantErr bool
	}{
		{"unmarked", zvol("Pool0/k8s/pvc-x", 1<<30, "", ""), true},
		{"inherited marker", zvol("Pool0/k8s/pvc-x", 1<<30, "truenas-csi", "INHERITED"), true},
		{"foreign value", zvol("Pool0/k8s/pvc-x", 1<<30, "someone-else", "LOCAL"), true},
		{"owned", zvol("Pool0/k8s/pvc-x", 1<<30, "truenas-csi", "LOCAL"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := newNAS(t)
			b := n.backend()
			n.putDataset(tc.seed)

			err := b.Delete(context.Background(), volID("pvc-x"))
			if tc.wantErr {
				if !errors.Is(err, volume.ErrNotManaged) {
					t.Fatalf("want ErrNotManaged, got %v", err)
				}
				if !n.hasDataset("Pool0/k8s/pvc-x") {
					t.Fatal("a dataset this driver does not own was destroyed")
				}
				if got := n.s.CallsTo("pool.dataset.delete"); got != 0 {
					t.Fatalf("no destructive call may be issued at all, got %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if n.hasDataset("Pool0/k8s/pvc-x") {
				t.Fatal("an owned dataset must be deleted")
			}
		})
	}
}

// TestISCSIDeleteRemovesEveryObject: the LUN mapping and extent must go too,
// or the shared target accumulates dangling entries that exhaust its LUN space.
func TestISCSIDeleteRemovesEveryObject(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	if _, err := b.Create(ctx, createReq("pvc-d", 1<<30, nil)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Delete(ctx, volID("pvc-d")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	ds, extents, _, texts := n.counts()
	if ds != 0 || extents != 0 || texts != 0 {
		t.Fatalf("want everything gone, got datasets=%d extents=%d targetextents=%d", ds, extents, texts)
	}

	// Deleting again is success: CSI retries DeleteVolume after the objects
	// are already gone.
	if err := b.Delete(ctx, volID("pvc-d")); err != nil {
		t.Fatalf("second Delete must be a no-op, got %v", err)
	}
}

// TestISCSIExpandRejectsShrink: the middleware refuses a zvol shrink, but the
// driver must refuse it first and say so, rather than surfacing a middleware
// error whose text tells the operator nothing.
func TestISCSIExpandRejectsShrink(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	if _, err := b.Create(ctx, createReq("pvc-e", 2<<30, nil)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := b.Expand(ctx, volID("pvc-e"), 1<<30); !errors.Is(err, ErrShrinkNotAllowed) {
		t.Fatalf("want ErrShrinkNotAllowed, got %v", err)
	}
	if got := n.s.CallsTo("pool.dataset.update"); got != 0 {
		t.Fatalf("a shrink must never reach the middleware, got %d updates", got)
	}

	got, err := b.Expand(ctx, volID("pvc-e"), 4<<30)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if got != 4<<30 {
		t.Fatalf("want 4 GiB, got %d", got)
	}
	// Growing to the size it already has is a no-op, not an error.
	if got, err := b.Expand(ctx, volID("pvc-e"), 4<<30); err != nil || got != 4<<30 {
		t.Fatalf("idempotent expand: got %d, %v", got, err)
	}
}

// TestISCSICreateFromSnapshotStampsClone: a ZFS clone inherits NEITHER the
// ownership marker NOR the size stamp. An unstamped clone can never be deleted
// by the ownership guard, so every restored volume would leak forever.
func TestISCSICreateFromSnapshotStampsClone(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	if _, err := b.Create(ctx, createReq("pvc-src", 1<<30, nil)); err != nil {
		t.Fatalf("Create source: %v", err)
	}

	req := createReq("pvc-restored", 2<<30, nil)
	req.SourceSnapshot = "Pool0/k8s/pvc-src@snap1"
	if _, err := b.Create(ctx, req); err != nil {
		t.Fatalf("restore: %v", err)
	}

	ds := n.dataset("Pool0/k8s/pvc-restored")
	if ds == nil {
		t.Fatal("the clone was not created")
	}
	props, _ := ds["user_properties"].(map[string]any)
	marker, _ := props["io.truenas.csi:managed"].(map[string]any)
	if marker == nil || marker["value"] != "truenas-csi" || marker["source"] != "LOCAL" {
		t.Fatalf("the clone must be stamped LOCALly, got %v", props)
	}
	size, _ := ds["volsize"].(map[string]any)
	if got, _ := size["parsed"].(float64); int64(got) != 2<<30 {
		t.Fatalf("the clone must carry the requested size, got %v", size)
	}

	// And it must now be deletable through the ownership guard.
	if err := b.Delete(ctx, volID("pvc-restored")); err != nil {
		t.Fatalf("restored volume must be deletable: %v", err)
	}
}

// TestDeleteRetriesWhileZvolIsBusy pins behaviour observed against a real
// appliance: removing an extent does not immediately release the zvol, and a
// delete issued in that window fails with "dataset is busy". Without the retry
// every iSCSI volume deletion would leak its zvol.
func TestDeleteRetriesWhileZvolIsBusy(t *testing.T) {
	old := zvolReleaseTimeout
	zvolReleaseTimeout = 5 * time.Second
	defer func() { zvolReleaseTimeout = old }()

	if !isBusy(&truenas.CallError{Code: -32001, ErrName: "EBUSY",
		Reason: "[EBUSY] Failed to delete dataset: cannot destroy 'Pool0/k8s/x': dataset is busy"}) {
		t.Fatal("the appliance's real EBUSY shape must be recognised as busy")
	}
	if isBusy(&truenas.CallError{Code: -32602, ErrName: "EINVAL",
		Reason: "[ENOENT] None: PoolDataset Pool0/k8s/x does not exist"}) {
		t.Fatal("a missing dataset must not be mistaken for a busy one — retrying " +
			"would turn an idempotent delete into a 30-second stall")
	}
}
