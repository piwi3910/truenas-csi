package csi

import (
	"context"
	"encoding/json"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// ownedProps builds a driver-owned dataset's user properties, plus whatever
// else the case needs.
func ownedProps(extra map[string]any) map[string]any {
	props := map[string]any{
		volume.OwnerProperty: map[string]any{"value": volume.OwnerValue, "source": "LOCAL"},
	}
	for k, v := range extra {
		props[k] = v
	}
	return props
}

// TestListVolumesOmitsTheGraveyardAndItsContents.
//
// A retired volume is one DeleteVolume already reported as gone, so listing it
// would hand the CO back a handle the driver has said is deleted. It is also at
// a depth this driver never provisions into, so the handle would read ".trash"
// as a Kubernetes namespace and name the wrong dataset.
//
// The graveyard container is skipped for a related reason: it is driver-owned
// and holds no data of its own, and a DeleteVolume on a handle naming it would
// be asked to destroy every retired volume at once.
func TestListVolumesOmitsTheGraveyardAndItsContents(t *testing.T) {
	const oneGiB = int64(1) << 30
	s := fake.Start(t, fake.Options{})
	s.Handle("pool.dataset.query", func([]json.RawMessage) (any, error) {
		return []any{
			map[string]any{
				"id": "Pool0/k8s/pvc-live", "type": "FILESYSTEM",
				"refquota":        map[string]any{"parsed": oneGiB},
				"user_properties": ownedProps(nil),
			},
			map[string]any{
				"id": "Pool0/k8s/.trash", "type": "FILESYSTEM",
				"user_properties": ownedProps(map[string]any{
					volume.GraveyardProperty: map[string]any{
						"value": volume.GraveyardValue, "source": "LOCAL"}}),
			},
			map[string]any{
				"id": "Pool0/k8s/.trash/20260801T000000Z-pvc-dead", "type": "FILESYSTEM",
				"refquota": map[string]any{"parsed": oneGiB},
				"user_properties": ownedProps(map[string]any{
					// Inherited from the graveyard, exactly as ZFS reports it:
					// a presence-only check here would skip nothing at all.
					volume.GraveyardProperty: map[string]any{
						"value": volume.GraveyardValue, "source": "INHERITED"},
					volume.DeletedAtProperty: map[string]any{
						"value": "2026-08-01T00:00:00Z", "source": "LOCAL"},
					volume.RetiredFromProperty: map[string]any{
						"value": "nas1/nfs/Pool0/k8s/pvc-dead", "source": "LOCAL"},
				}),
			},
		}, nil
	})
	c := ctlWithServer(t, s)

	resp, err := c.ListVolumes(context.Background(), &csipb.ListVolumesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(resp.GetEntries()))
	for _, e := range resp.GetEntries() {
		got = append(got, e.GetVolume().GetVolumeId())
	}
	if len(got) != 1 || got[0] != "nas1/nfs/Pool0/k8s/pvc-live" {
		t.Fatalf("listed %v, want only the live volume", got)
	}
}
