package integration

import (
	"context"
	"testing"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/csi"
	"github.com/piwi3910/truenas-csi/internal/reconcile"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// handleSet is a PVLister backed by a fixed set, standing in for the API server.
type handleSet map[string]struct{}

func (h handleSet) VolumeHandles(context.Context) (map[string]struct{}, error) {
	return h, nil
}

// TestE2EOrphanReconciler runs the orphan report against the appliance.
//
// The reconciler is the only thing that would notice a dataset this driver
// created and Kubernetes has forgotten, and it had no hardware coverage: it
// runs on a 30-minute timer inside the controller, so nothing ever observed it
// answering from real appliance state. What it must get right is both
// directions — a volume Kubernetes still knows about must NOT be reported, and
// one it has forgotten must be.
func TestE2EOrphanReconciler(t *testing.T) {
	e := requireAppliance(t)
	ctx := context.Background()

	reg, err := backend.NewRegistry(ctx, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	ctrl := csi.NewControllerWithNodes(reg, e.cfg, e.resolver(t))

	resp, err := ctrl.CreateVolume(ctx, &csipb.CreateVolumeRequest{
		Name: uniqueName("orphan"), Parameters: e.params("nfs"),
		CapacityRange:      &csipb.CapacityRange{RequiredBytes: 1 << 30},
		VolumeCapabilities: caps(),
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	handle := resp.GetVolume().GetVolumeId()
	t.Cleanup(func() {
		_, _ = ctrl.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{VolumeId: handle})
	})

	// Kubernetes still knows about it: it must not be reported.
	known := reconcile.NewOrphanReconciler(reg, handleSet{handle: {}}, time.Minute)
	found, err := known.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if contains(found, handle) {
		t.Errorf("a volume with a live PersistentVolume was reported as an orphan: %v", found)
	}

	// Kubernetes has forgotten it: it must be reported.
	forgotten := reconcile.NewOrphanReconciler(reg, handleSet{}, time.Minute)
	found, err = forgotten.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !contains(found, handle) {
		t.Errorf("a volume with no PersistentVolume was NOT reported as an orphan.\n"+
			"  looking for: %s\n  reported:    %v", handle, found)
	}

	// A dataset this driver did not create is never anyone's orphan, however
	// forgotten it is. This is the check that stops the report from pointing an
	// operator at their own data.
	foreign := e.prefix + "/" + uniqueName("not-ours")
	if _, err := e.client.DatasetCreate(ctx, truenas.DatasetSpec{
		Name: foreign, Type: "FILESYSTEM", RefQuota: 1 << 30,
	}); err != nil {
		t.Fatalf("creating an unmanaged dataset: %v", err)
	}
	t.Cleanup(func() { _ = e.client.DatasetDelete(ctx, foreign, true, true) })

	found, err = forgotten.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	for _, f := range found {
		if id, perr := volume.ParseID(f); perr == nil && id.DatasetPath() == foreign {
			t.Errorf("a dataset this driver never created was reported as an orphan: %s", f)
		}
	}
}

func contains(all []string, want string) bool {
	for _, a := range all {
		if a == want {
			return true
		}
	}
	return false
}
