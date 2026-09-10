package csi

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// healthyPool is the appliance's own Pool0, as it reports itself when nothing
// is wrong.
const healthyPool = `[{"name":"Pool0","status":"ONLINE","healthy":true,
	"free":{"parsed":44861949222912},"size":{"parsed":72000831750144}}]`

// degradedPool is the same pool with a failed disk: status and healthy move
// together, and a real appliance reports both.
const degradedPool = `[{"name":"Pool0","status":"DEGRADED","healthy":false,
	"free":{"parsed":44861949222912},"size":{"parsed":72000831750144}}]`

// volumeServer is a fake appliance holding exactly one volume, whose dataset
// row and pool row the test chooses.
func volumeServer(t *testing.T, pool string, dataset map[string]any) csipb.ControllerServer {
	t.Helper()
	s := fake.Start(t, fake.Options{})
	s.Handle("pool.query", func([]json.RawMessage) (any, error) {
		var v any
		if err := json.Unmarshal([]byte(pool), &v); err != nil {
			return nil, err
		}
		return v, nil
	})
	s.Handle("pool.dataset.query", func(p []json.RawMessage) (any, error) {
		var filters [][]any
		_ = json.Unmarshal(p[0], &filters)
		want, _ := filters[0][2].(string)
		if dataset == nil || dataset["id"] != want {
			return []any{}, nil
		}
		return []any{dataset}, nil
	})
	return ctlWithServer(t, s)
}

// ownedFilesystem is a dataset row as the middleware returns one for a volume
// this driver provisioned, with an optional publish ledger.
func ownedFilesystem(id string, refquota, available int64, ledger string) map[string]any {
	props := map[string]any{
		"io.truenas.csi:managed": map[string]any{"value": "truenas-csi", "source": "LOCAL"},
	}
	if ledger != "" {
		props["io.truenas.csi:published"] = map[string]any{"value": ledger, "source": "LOCAL"}
	}
	return map[string]any{
		"id": id, "type": "FILESYSTEM", "mountpoint": "/mnt/" + id,
		"refquota":        map[string]any{"parsed": refquota},
		"available":       map[string]any{"parsed": available},
		"user_properties": props,
	}
}

const getVol = "nas1/nfs/Pool0/k8s/pvc-a"

// TestControllerGetVolumeReportsTheAppliancesView: the size comes from the pool
// as it is NOW, not from what the PersistentVolume remembers, and the published
// nodes come from the volume's own ledger — the only record that survives a
// controller restart and the deletion of a Node object.
func TestControllerGetVolumeReportsTheAppliancesView(t *testing.T) {
	c := volumeServer(t, healthyPool,
		ownedFilesystem("Pool0/k8s/pvc-a", 4<<30, 1<<30, `{"worker-2":["10.0.0.2"],"worker-1":["10.0.0.1"]}`))

	resp, err := c.ControllerGetVolume(context.Background(),
		&csipb.ControllerGetVolumeRequest{VolumeId: getVol})
	if err != nil {
		t.Fatalf("ControllerGetVolume: %v", err)
	}
	if got := resp.GetVolume().GetCapacityBytes(); got != 4<<30 {
		t.Fatalf("capacity = %d, want the dataset's current refquota %d", got, int64(4)<<30)
	}
	if resp.GetVolume().GetVolumeId() != getVol {
		t.Fatalf("volume id = %q, want %q", resp.GetVolume().GetVolumeId(), getVol)
	}
	got := resp.GetStatus().GetPublishedNodeIds()
	if len(got) != 2 || got[0] != "worker-1" || got[1] != "worker-2" {
		t.Fatalf("published nodes = %v, want [worker-1 worker-2] in a stable order", got)
	}
}

// TestControllerGetVolumeArgumentValidation covers the codes the CO and
// csi-sanity distinguish: a malformed request is InvalidArgument, and anything
// that names a volume this driver does not have is NotFound.
func TestControllerGetVolumeArgumentValidation(t *testing.T) {
	c := volumeServer(t, healthyPool, ownedFilesystem("Pool0/k8s/pvc-a", 1<<30, 1<<30, ""))
	ctx := context.Background()

	cases := []struct {
		name string
		id   string
		want codes.Code
	}{
		{"no volume id", "", codes.InvalidArgument},
		{"unparseable id", "not-a-handle", codes.NotFound},
		{"unknown backend", "nope/nfs/Pool0/k8s/pvc-a", codes.NotFound},
		{"dataset is gone", "nas1/nfs/Pool0/k8s/pvc-ghost", codes.NotFound},
		{"outside the backend's parent dataset", "nas1/nfs/Pool0/other/pvc-a", codes.NotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.ControllerGetVolume(ctx, &csipb.ControllerGetVolumeRequest{VolumeId: tc.id})
			if got := status.Code(err); got != tc.want {
				t.Fatalf("code = %v, want %v (err %v)", got, tc.want, err)
			}
			_, err = c.ControllerGetVolumeHealth(ctx,
				&csipb.ControllerGetVolumeHealthRequest{VolumeId: tc.id})
			if got := status.Code(err); got != tc.want {
				t.Fatalf("health: code = %v, want %v (err %v)", got, tc.want, err)
			}
		})
	}
}

// TestControllerGetVolumeHealth exercises the RPC end to end for the two
// answers an operator acts on differently: nothing wrong, and a pool that is
// degraded underneath a volume nobody's node can see is degraded.
func TestControllerGetVolumeHealth(t *testing.T) {
	ctx := context.Background()

	healthy := volumeServer(t, healthyPool, ownedFilesystem("Pool0/k8s/pvc-a", 1<<30, 1<<30, ""))
	resp, err := healthy.ControllerGetVolumeHealth(ctx,
		&csipb.ControllerGetVolumeHealthRequest{VolumeId: getVol})
	if err != nil {
		t.Fatalf("ControllerGetVolumeHealth: %v", err)
	}
	if resp.GetVolumeHealth().GetVolumeId() != getVol {
		t.Fatalf("volume id = %q, want %q", resp.GetVolumeHealth().GetVolumeId(), getVol)
	}
	if n := len(resp.GetVolumeHealth().GetHealthStatuses()); n != 0 {
		t.Fatalf("a healthy pool and a volume with room must report no condition, got %d: %v",
			n, resp.GetVolumeHealth().GetHealthStatuses())
	}

	degraded := volumeServer(t, degradedPool, ownedFilesystem("Pool0/k8s/pvc-a", 1<<30, 1<<30, ""))
	resp, err = degraded.ControllerGetVolumeHealth(ctx,
		&csipb.ControllerGetVolumeHealthRequest{VolumeId: getVol})
	if err != nil {
		t.Fatalf("ControllerGetVolumeHealth: %v", err)
	}
	entries := resp.GetVolumeHealth().GetHealthStatuses()
	if len(entries) != 1 || entries[0].GetReason() != "PoolDegraded" {
		t.Fatalf("a degraded pool must be reported, got %v", entries)
	}
	if entries[0].GetStatus() != csipb.VolumeHealthErrorType_DEGRADED {
		t.Fatalf("status = %v, want DEGRADED", entries[0].GetStatus())
	}
}

// TestApplianceConditionsAreObservable pins the whole condition table. Each row
// is a fact the appliance itself reports; nothing here is inferred about a
// node, which is the property that makes this signal trustworthy.
func TestApplianceConditionsAreObservable(t *testing.T) {
	fs := func(refquota, available int64) *truenas.Dataset {
		var d truenas.Dataset
		if err := json.Unmarshal([]byte(fmt.Sprintf(
			`{"id":"Pool0/k8s/pvc-a","type":"FILESYSTEM",
			  "refquota":{"parsed":%d},"available":{"parsed":%d}}`,
			refquota, available)), &d); err != nil {
			t.Fatal(err)
		}
		return &d
	}
	zvol := func(volsize, available int64) *truenas.Dataset {
		var d truenas.Dataset
		if err := json.Unmarshal([]byte(fmt.Sprintf(
			`{"id":"Pool0/k8s/pvc-a","type":"VOLUME",
			  "volsize":{"parsed":%d},"available":{"parsed":%d}}`,
			volsize, available)), &d); err != nil {
			t.Fatal(err)
		}
		return &d
	}
	pool := func(status string, healthy bool) *truenas.Pool {
		return &truenas.Pool{Name: "Pool0", Status: status, Healthy: healthy}
	}

	cases := []struct {
		name    string
		pool    *truenas.Pool
		ds      *truenas.Dataset
		reasons []string
	}{
		{"nothing wrong", pool("ONLINE", true), fs(1<<30, 1<<29), nil},
		{"degraded pool", pool("DEGRADED", false), fs(1<<30, 1<<29), []string{"PoolDegraded"}},
		{"faulted pool", pool("FAULTED", false), fs(1<<30, 1<<29), []string{"PoolDegraded"}},
		// `healthy` is false for warnings that leave the status ONLINE, and a
		// warning is still something an operator should see.
		{"online but unhealthy", pool("ONLINE", false), fs(1<<30, 1<<29), []string{"PoolDegraded"}},
		{"quota exhausted", pool("ONLINE", true), fs(1<<30, 0), []string{"VolumeFull"}},
		// No quota set means `available` tracks the pool, not the volume, so it
		// says nothing about this volume and must not be reported as if it did.
		{"no quota, no space", pool("ONLINE", true), fs(0, 0), nil},
		// A zvol's space is preallocated by volsize; `available` there is the
		// pool's headroom for overcommit, not the volume's own space.
		{"zvol with a full pool", pool("ONLINE", true), zvol(1<<30, 0), nil},
		{"both at once", pool("DEGRADED", false), fs(1<<30, 0), []string{"PoolDegraded", "VolumeFull"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applianceConditions(tc.pool, tc.ds)
			if len(got) != len(tc.reasons) {
				t.Fatalf("conditions = %v, want reasons %v", got, tc.reasons)
			}
			for i, want := range tc.reasons {
				if got[i].GetReason() != want {
					t.Fatalf("condition %d reason = %q, want %q", i, got[i].GetReason(), want)
				}
				if got[i].GetMessage() == "" {
					t.Fatalf("condition %d has no message; an operator needs the detail", i)
				}
				// INACCESSIBLE means "a client cannot reach its data", which is
				// a node-side observation. A controller reporting it would be
				// inventing a signal it cannot see; that is the node plugin's
				// NodeGetVolumeHealth.
				if got[i].GetStatus() == csipb.VolumeHealthErrorType_INACCESSIBLE {
					t.Fatalf("condition %q reports INACCESSIBLE, which no controller can observe",
						got[i].GetReason())
				}
			}
		})
	}
}

// TestListVolumesReportsPublishedNodes: LIST_VOLUMES_PUBLISHED_NODES is
// advertised, so every entry has to carry the ledger's node ids — a listing
// that silently omitted them would tell the CO the volumes are attached
// nowhere.
func TestListVolumesReportsPublishedNodes(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.Handle("pool.dataset.query", func([]json.RawMessage) (any, error) {
		return []any{
			ownedFilesystem("Pool0/k8s/pvc-a", 1<<30, 1<<30, `{"worker-1":["10.0.0.1"]}`),
			ownedFilesystem("Pool0/k8s/pvc-b", 1<<30, 1<<30, ""),
			// A ledger that will not decode must not fail the listing.
			ownedFilesystem("Pool0/k8s/pvc-c", 1<<30, 1<<30, "{not json"),
		}, nil
	})
	c := ctlWithServer(t, s)

	resp, err := c.ListVolumes(context.Background(), &csipb.ListVolumesRequest{})
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	if len(resp.GetEntries()) != 3 {
		t.Fatalf("listed %d volumes, want 3", len(resp.GetEntries()))
	}
	want := map[string][]string{
		"nas1/nfs/Pool0/k8s/pvc-a": {"worker-1"},
		"nas1/nfs/Pool0/k8s/pvc-b": {},
		"nas1/nfs/Pool0/k8s/pvc-c": {},
	}
	for _, e := range resp.GetEntries() {
		w, ok := want[e.GetVolume().GetVolumeId()]
		if !ok {
			t.Fatalf("unexpected volume %q", e.GetVolume().GetVolumeId())
		}
		got := e.GetStatus().GetPublishedNodeIds()
		if len(got) != len(w) {
			t.Fatalf("%s published nodes = %v, want %v", e.GetVolume().GetVolumeId(), got, w)
		}
		for i := range w {
			if got[i] != w[i] {
				t.Fatalf("%s published nodes = %v, want %v", e.GetVolume().GetVolumeId(), got, w)
			}
		}
	}
}

// TestGetCapacityReportsAMaximumVolumeSize: the scheduler can only leave an
// oversized claim Pending if the driver says what oversized means, and the only
// honest answer is the figure CreateVolume itself enforces.
func TestGetCapacityReportsAMaximumVolumeSize(t *testing.T) {
	// Writable bytes: capacity is measured against what the pool root dataset
	// reports, not pool.query's raw figures.
	const free, used = int64(33352704796320), int64(21466962973168)
	s := fake.Start(t, fake.Options{})
	s.Handle("pool.dataset.query", func([]json.RawMessage) (any, error) {
		var v any
		_ = json.Unmarshal([]byte(fmt.Sprintf(
			`[{"id":"Pool0","type":"FILESYSTEM",
			   "available":{"parsed":%d},"used":{"parsed":%d}}]`, free, used)), &v)
		return v, nil
	})
	c := ctlWithServer(t, s)

	resp, err := c.GetCapacity(context.Background(), &csipb.GetCapacityRequest{
		Parameters: map[string]string{"backend": "nas1", "protocol": "nfs"}})
	if err != nil {
		t.Fatal(err)
	}
	maxSize := resp.GetMaximumVolumeSize()
	if maxSize == nil {
		t.Fatal("maximum_volume_size must be reported: four of Dell's five drivers do, " +
			"and without it the scheduler binds claims this driver will then refuse")
	}
	if maxSize.GetValue() != resp.GetAvailableCapacity() {
		t.Fatalf("maximum volume size %d, want the usable capacity %d — the reserve check in "+
			"CreateVolume refuses anything larger", maxSize.GetValue(), resp.GetAvailableCapacity())
	}
}
