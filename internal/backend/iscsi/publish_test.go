package iscsi

import (
	"context"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

func testNode(id string) backend.NodeRef {
	return backend.NodeRef{ID: id, Addrs: []string{"10.0.0.1"}}
}

// TestPublishMapsAndUnpublishFences: the LUN mapping is the whole fence for
// iSCSI. Initiator ACLs belong to the shared target, not to a LUN, so the only
// thing that takes a volume away from the node holding it is removing its
// address on that target.
func TestPublishMapsAndUnpublishFences(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	if _, err := b.Create(ctx, createReq("pvc-f", 1<<30, nil)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, _, texts := n.counts(); texts != 0 {
		t.Fatalf("Create must leave the volume unmapped, got %d mappings", texts)
	}

	pc, err := b.Publish(ctx, volID("pvc-f"), testNode("worker-1"))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if pc["lun"] == "" || pc["naa"] == "" || pc["portal"] == "" || pc["iqn"] == "" {
		t.Fatalf("the publish context must carry everything the node attaches with, got %v", pc)
	}
	if _, _, _, texts := n.counts(); texts != 1 {
		t.Fatalf("Publish must map exactly one LUN, got %d", texts)
	}

	// A repeat converges rather than mapping a second address.
	again, err := b.Publish(ctx, volID("pvc-f"), testNode("worker-1"))
	if err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if again["lun"] != pc["lun"] {
		t.Fatalf("a repeated publish moved the volume from LUN %q to %q", pc["lun"], again["lun"])
	}
	if _, _, _, texts := n.counts(); texts != 1 {
		t.Fatalf("a repeated publish stacked mappings, got %d", texts)
	}

	if err := b.Unpublish(ctx, volID("pvc-f"), testNode("worker-1")); err != nil {
		t.Fatalf("Unpublish: %v", err)
	}
	if _, _, _, texts := n.counts(); texts != 0 {
		t.Fatalf("Unpublish must leave no mapping behind, got %d", texts)
	}
	// The extent and the zvol survive: a fence removes access, not data.
	if _, extents, _, _ := n.counts(); extents != 1 {
		t.Fatalf("the extent must survive a fence, got %d", extents)
	}
	if !n.hasDataset("Pool0/k8s/pvc-f") {
		t.Fatal("the zvol must survive a fence")
	}

	// And a second unpublish is success, because the CO retries.
	if err := b.Unpublish(ctx, volID("pvc-f"), testNode("worker-1")); err != nil {
		t.Fatalf("a repeated Unpublish must succeed, got %v", err)
	}
}

// TestPublishAdoptsAnExistingMapping covers every volume provisioned before the
// mapping moved out of CreateVolume: it already has one, and the first publish
// after the upgrade must take it over rather than fail on the duplicate or hand
// the volume a second address.
func TestPublishAdoptsAnExistingMapping(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	if _, err := b.Create(ctx, createReq("pvc-old", 1<<30, nil)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Reproduce the pre-upgrade state: a mapping made at provisioning time, on
	// a LUN the allocator would not have chosen next.
	target, err := queryTarget(ctx, b.c, targetName("Pool0", "k8s"))
	if err != nil || target == nil {
		t.Fatalf("target: %v %v", target, err)
	}
	extent, err := b.c.ExtentByName(ctx, extentName(volID("pvc-old")))
	if err != nil || extent == nil {
		t.Fatalf("extent: %v %v", extent, err)
	}
	if _, err := b.c.TargetExtentCreate(ctx, target.ID, extent.ID, 7); err != nil {
		t.Fatalf("seeding the old mapping: %v", err)
	}

	pc, err := b.Publish(ctx, volID("pvc-old"), testNode("worker-1"))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if pc["lun"] != "7" {
		t.Fatalf("an existing mapping must be adopted, got LUN %q want 7", pc["lun"])
	}
	if _, _, _, texts := n.counts(); texts != 1 {
		t.Fatalf("adoption must not add a second mapping, got %d", texts)
	}
}

// TestLUNIsStableAcrossRepublish: the allocator hands out the lowest free id,
// so after an unrelated volume is deleted a republish could land this volume on
// an id another volume used to occupy — and an initiator that cached the old
// mapping would address the wrong device. The id a volume held is recorded and
// preferred.
func TestLUNIsStableAcrossRepublish(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	for _, name := range []string{"pvc-a", "pvc-b"} {
		if _, err := b.Create(ctx, createReq(name, 1<<30, nil)); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		if _, err := b.Publish(ctx, volID(name), testNode("worker-1")); err != nil {
			t.Fatalf("Publish %s: %v", name, err)
		}
	}
	second, err := b.Publish(ctx, volID("pvc-b"), testNode("worker-1"))
	if err != nil {
		t.Fatalf("Publish pvc-b: %v", err)
	}
	if err := b.c.SetUserProperty(ctx, "Pool0/k8s/pvc-b", volume.LUNProperty, second["lun"]); err != nil {
		t.Fatalf("seeding the recorded LUN: %v", err)
	}

	// pvc-a goes away entirely, freeing the LOWEST id. A naive allocator would
	// now move pvc-b onto it.
	if err := b.Unpublish(ctx, volID("pvc-a"), testNode("worker-1")); err != nil {
		t.Fatalf("Unpublish pvc-a: %v", err)
	}
	if err := b.Delete(ctx, volID("pvc-a")); err != nil {
		t.Fatalf("Delete pvc-a: %v", err)
	}
	if err := b.Unpublish(ctx, volID("pvc-b"), testNode("worker-1")); err != nil {
		t.Fatalf("Unpublish pvc-b: %v", err)
	}
	resetState() // a controller restart forgets every in-process reservation

	again, err := b.Publish(ctx, volID("pvc-b"), testNode("worker-1"))
	if err != nil {
		t.Fatalf("republish pvc-b: %v", err)
	}
	if again["lun"] != second["lun"] {
		t.Fatalf("a republished volume moved from LUN %q to %q", second["lun"], again["lun"])
	}
}

// TestPublishRejectsWhatIsNotThere: CSI needs NotFound for a volume that does
// not exist, and the caller distinguishes it from a real failure.
func TestPublishRejectsWhatIsNotThere(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	if _, err := b.Publish(ctx, volID("pvc-absent"), testNode("worker-1")); err == nil {
		t.Fatal("publishing a volume that does not exist must fail")
	}
	if err := b.Unpublish(ctx, volID("pvc-absent"), testNode("worker-1")); err != nil {
		t.Fatalf("unpublishing a volume that does not exist must succeed, got %v", err)
	}
}
