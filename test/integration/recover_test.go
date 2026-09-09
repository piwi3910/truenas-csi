package integration

import (
	"context"
	"strings"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/csi"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// TestE2ERecoverRetiredVolume walks the recovery procedure in
// docs/delete-protection.md against the appliance, exactly as an operator would.
//
// It exists because that procedure was WRONG and nothing was testing it. It
// told the operator to rename the dataset back and clear two properties, and
// stopped there — so the recovered volume still carried an
// io.truenas.csi:owner-id naming its graveyard path. The driver establishes
// ownership by comparing that value against the dataset's own id, so a volume
// recovered by the book was not recognised as the driver's at all: DeleteVolume,
// expansion and ControllerModifyVolume would all refuse it and ListVolumes would
// omit it, for a volume the operator had just rescued.
//
// The test asserts the end state the procedure has to reach: the driver treats
// the dataset as its own again.
func TestE2ERecoverRetiredVolume(t *testing.T) {
	e := requireAppliance(t)
	ctx := context.Background()

	// Delete protection on, with a grace long enough that the reaper cannot
	// destroy the volume out from under the test.
	cfg := *e.cfg
	cfg.Backends = map[string]config.Backend{}
	for k, v := range e.cfg.Backends {
		v.DeleteProtection = config.DeleteProtection{
			Enabled: true, GracePeriod: "24h", ReapInterval: "24h",
		}
		cfg.Backends[k] = v
	}
	reg, err := backend.NewRegistry(ctx, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	ctrl := csi.NewControllerWithNodes(reg, &cfg, e.resolver(t))

	resp, err := ctrl.CreateVolume(ctx, &csipb.CreateVolumeRequest{
		Name: uniqueName("recover"), Parameters: e.params("nfs"),
		CapacityRange:      &csipb.CapacityRange{RequiredBytes: 1 << 30},
		VolumeCapabilities: caps(),
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	handle := resp.GetVolume().GetVolumeId()
	id, err := volume.ParseID(handle)
	if err != nil {
		t.Fatal(err)
	}
	original := id.DatasetPath()
	// Destroy the dataset outright rather than going through DeleteVolume: this
	// test runs with delete protection ON, so DeleteVolume would RETIRE the
	// volume into the graveyard and leave it there for the 24h grace period
	// this test configures. The path is whichever one the recovery reached, so
	// both are tried.
	t.Cleanup(func() { _ = e.client.DatasetDelete(ctx, original, true, true) })

	// Retire it, the way a PVC deletion does.
	if _, err := ctrl.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{VolumeId: handle}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	retired := findRetired(ctx, t, e.client, e.prefix, handle)
	// The rename below moves it out of the graveyard, so this only fires when
	// the test fails before that.
	t.Cleanup(func() { _ = e.client.DatasetDelete(ctx, retired, true, true) })

	// ---- the documented recovery, step for step ----

	// Rename back to the path the handle names. force is required: the
	// appliance refuses every rename without it.
	if err := e.client.DatasetRename(ctx, retired, original, true); err != nil {
		t.Fatalf("renaming %s back to %s: %v", retired, original, err)
	}
	// Clear the retirement marks and re-point the ownership marker at the new
	// path. The last one is the step the documentation used to omit.
	if _, err := e.client.DatasetUpdate(ctx, original, map[string]any{
		"user_properties_update": []map[string]any{
			{"key": volume.DeletedAtProperty, "remove": true},
			{"key": volume.RetiredFromProperty, "remove": true},
			{"key": volume.OwnerIDProperty, "value": original},
		},
	}); err != nil {
		t.Fatalf("clearing the retirement marks on %s: %v", original, err)
	}

	// ---- the end state the procedure has to reach ----

	ds, err := e.client.DatasetQuery(ctx, original)
	if err != nil || ds == nil {
		t.Fatalf("the recovered dataset is not at %s: %v", original, err)
	}
	view := &volume.Dataset{ID: ds.ID, UserProperties: map[string]volume.Property{}}
	for k, v := range ds.UserProperties {
		view.UserProperties[k] = volume.Property{Value: v.Value, Source: v.Source}
	}
	if err := volume.VerifyOwned(view); err != nil {
		t.Fatalf("the driver does not recognise the recovered volume as its own, so "+
			"every destructive call on it will refuse and ListVolumes will omit it: %v", err)
	}
	if _, ok := ds.UserProperties[volume.DeletedAtProperty]; ok {
		t.Errorf("%s is still set: ListVolumes will not report the volume", volume.DeletedAtProperty)
	}

	// The proof that ownership really is restored: a call that refuses anything
	// the driver does not own now succeeds.
	if _, err := ctrl.ControllerExpandVolume(ctx, &csipb.ControllerExpandVolumeRequest{
		VolumeId:      handle,
		CapacityRange: &csipb.CapacityRange{RequiredBytes: 2 << 30},
	}); err != nil {
		t.Errorf("expanding the recovered volume: %v", err)
	}
}

// findRetired locates the graveyard entry a retire produced for this handle.
func findRetired(ctx context.Context, t *testing.T, c truenas.API, prefix, handle string) string {
	t.Helper()
	all, err := c.DatasetList(ctx, prefix+"/.trash/")
	if err != nil {
		t.Fatalf("listing the graveyard: %v", err)
	}
	for i := range all {
		if all[i].UserProperties[volume.RetiredFromProperty].Value == handle {
			return all[i].ID
		}
	}
	var ids []string
	for i := range all {
		ids = append(ids, all[i].ID)
	}
	t.Fatalf("no graveyard entry carries %s=%s; the graveyard holds: %s",
		volume.RetiredFromProperty, handle, strings.Join(ids, ", "))
	return ""
}
