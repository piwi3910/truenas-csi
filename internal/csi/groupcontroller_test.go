package csi

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// snapCreates records every pool.snapshot.create the driver issues, so a test
// can tell ONE atomic recursive call from N sequential ones.
type snapCreates struct {
	mu     sync.Mutex
	params []map[string]any
}

func (c *snapCreates) all() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.params...)
}

// handleSnapshotCreate answers pool.snapshot.create the way the appliance does,
// recording the arguments.
func handleSnapshotCreate(s *fake.Server) *snapCreates {
	rec := &snapCreates{}
	s.Handle("pool.snapshot.create", func(p []json.RawMessage) (any, error) {
		var m map[string]any
		if len(p) > 0 {
			_ = json.Unmarshal(p[0], &m)
		}
		rec.mu.Lock()
		rec.params = append(rec.params, m)
		rec.mu.Unlock()
		ds, _ := m["dataset"].(string)
		name, _ := m["name"].(string)
		return map[string]any{"id": ds + "@" + name, "name": name, "dataset": ds}, nil
	})
	return rec
}

// gctlWith builds a GroupController (and the matching Controller, so a member
// snapshot can be restored through the ordinary path) over one fake appliance.
func gctlWith(t *testing.T) (csipb.GroupControllerServer, csipb.ControllerServer, *fake.Server) {
	t.Helper()
	s := fake.Start(t, fake.Options{})
	s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
	cfg := &config.Config{NodeID: "worker-21", Backends: map[string]config.Backend{
		"nas1": {Name: "nas1", Endpoint: s.URL(), Username: "truenas_admin", APIKey: "8-x",
			Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true},
	}}
	r, err := backend.NewRegistry(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return NewGroupController(r, cfg), NewController(r, cfg), s
}

// gctlTwoBackends builds a GroupController over two appliances.
func gctlTwoBackends(t *testing.T) (csipb.GroupControllerServer, *fake.Server, *fake.Server) {
	t.Helper()
	s1, s2 := fake.Start(t, fake.Options{}), fake.Start(t, fake.Options{})
	for _, s := range []*fake.Server{s1, s2} {
		s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
	}
	cfg := &config.Config{NodeID: "worker-21", Backends: map[string]config.Backend{
		"nas1": {Name: "nas1", Endpoint: s1.URL(), Username: "truenas_admin", APIKey: "8-x",
			Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true},
		"nas2": {Name: "nas2", Endpoint: s2.URL(), Username: "truenas_admin", APIKey: "8-x",
			Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true},
	}}
	r, err := backend.NewRegistry(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return NewGroupController(r, cfg), s1, s2
}

func gvid(name string) string { return "nas1/counting/Pool0/k8s/" + name }

// existingDatasets makes every pool.dataset.query answer "yes, it exists" with
// no clone origin, which is what the members' existence check needs.
func existingDatasets(s *fake.Server) {
	s.HandleValue("pool.dataset.query", []any{
		map[string]any{"id": "Pool0/k8s/pvc-data"},
		map[string]any{"id": "Pool0/k8s/pvc-wal"},
		map[string]any{"id": "Pool0/k8s/pvc-log"},
	})
}

// TestGroupSnapshotIsAtomic is the whole point of the feature: the members must
// be captured at ONE instant. ZFS gives that only through a single recursive
// pool.snapshot.create on their common parent — N sequential per-dataset calls
// produce N different transaction groups and a database restored from them can
// be internally inconsistent.
func TestGroupSnapshotIsAtomic(t *testing.T) {
	gc, _, s := gctlWith(t)
	s.HandleValue("pool.snapshot.query", []any{})
	existingDatasets(s)
	rec := handleSnapshotCreate(s)

	resp, err := gc.CreateVolumeGroupSnapshot(context.Background(), &csipb.CreateVolumeGroupSnapshotRequest{
		Name:            "grp1",
		SourceVolumeIds: []string{gvid("pvc-data"), gvid("pvc-wal"), gvid("pvc-log")},
	})
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot: %v", err)
	}
	if n := s.CallsTo("pool.snapshot.create"); n != 1 {
		t.Fatalf("group snapshot must be ONE recursive call, got %d: %v", n, s.Calls())
	}
	got := rec.all()[0]
	if got["dataset"] != "Pool0/k8s" {
		t.Errorf("recursive snapshot must be taken on the common parent, got %v", got["dataset"])
	}
	if r, _ := got["recursive"].(bool); !r {
		t.Errorf("snapshot must be recursive, got %v", got)
	}
	if len(resp.GetGroupSnapshot().GetSnapshots()) != 3 {
		t.Fatalf("want 3 members, got %d", len(resp.GetGroupSnapshot().GetSnapshots()))
	}
	if !resp.GetGroupSnapshot().GetReadyToUse() {
		t.Error("ZFS snapshots are instant; the group must be ready to use")
	}
}

// TestGroupSnapshotRefusesNonSiblings: without a common parent ZFS has no
// atomic primitive, and taking N snapshots in a loop would hand a database an
// inconsistent restore point while calling it crash-consistent. Refusing is
// the only honest answer.
func TestGroupSnapshotRefusesNonSiblings(t *testing.T) {
	gc, _, s := gctlWith(t)
	s.HandleValue("pool.snapshot.query", []any{})
	existingDatasets(s)
	handleSnapshotCreate(s)

	_, err := gc.CreateVolumeGroupSnapshot(context.Background(), &csipb.CreateVolumeGroupSnapshotRequest{
		Name: "grp1",
		SourceVolumeIds: []string{
			"nas1/counting/Pool0/k8s/pvc-data",
			"nas1/counting/Pool0/other/pvc-wal",
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	if !strings.Contains(err.Error(), "parent") {
		t.Errorf("the error must explain the shared-parent requirement: %v", err)
	}
	if n := s.CallsTo("pool.snapshot.create"); n != 0 {
		t.Fatalf("no snapshot may be created when the group cannot be atomic, got %d: %v", n, s.Calls())
	}
}

// TestGroupSnapshotRefusesCrossBackend: two appliances cannot share a ZFS
// transaction group, so a group spanning them is not crash-consistent.
func TestGroupSnapshotRefusesCrossBackend(t *testing.T) {
	gc, s1, s2 := gctlTwoBackends(t)
	for _, s := range []*fake.Server{s1, s2} {
		s.HandleValue("pool.snapshot.query", []any{})
		existingDatasets(s)
		handleSnapshotCreate(s)
	}

	_, err := gc.CreateVolumeGroupSnapshot(context.Background(), &csipb.CreateVolumeGroupSnapshotRequest{
		Name: "grp1",
		SourceVolumeIds: []string{
			"nas1/counting/Pool0/k8s/pvc-data",
			"nas2/counting/Pool0/k8s/pvc-wal",
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
	if s1.CallsTo("pool.snapshot.create")+s2.CallsTo("pool.snapshot.create") != 0 {
		t.Fatal("no snapshot may be created for a cross-appliance group")
	}
}

// TestGroupSnapshotMembersAreIndividuallyRestorable: a group snapshot is only
// useful if each member can be restored through the ordinary
// CreateVolume-from-snapshot path, which means the member ids must carry the
// same format the single-volume path produces.
func TestGroupSnapshotMembersAreIndividuallyRestorable(t *testing.T) {
	shared = newCounting()
	gc, ctrl, s := gctlWith(t)
	s.HandleValue("pool.snapshot.query", []any{})
	existingDatasets(s)
	handleSnapshotCreate(s)

	resp, err := gc.CreateVolumeGroupSnapshot(context.Background(), &csipb.CreateVolumeGroupSnapshotRequest{
		Name:            "grp1",
		SourceVolumeIds: []string{gvid("pvc-data"), gvid("pvc-wal")},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"nas1/Pool0/k8s/pvc-data@grp1": gvid("pvc-data"),
		"nas1/Pool0/k8s/pvc-wal@grp1":  gvid("pvc-wal"),
	}
	for _, m := range resp.GetGroupSnapshot().GetSnapshots() {
		src, ok := want[m.GetSnapshotId()]
		if !ok {
			t.Fatalf("unexpected member snapshot id %q, want one of %v", m.GetSnapshotId(), want)
		}
		if m.GetSourceVolumeId() != src {
			t.Errorf("member %s: source volume %q, want %q", m.GetSnapshotId(), m.GetSourceVolumeId(), src)
		}
		if _, _, perr := backend.SnapshotSource(m.GetSnapshotId()); perr != nil {
			t.Errorf("member id %q is not a restorable snapshot id: %v", m.GetSnapshotId(), perr)
		}
		delete(want, m.GetSnapshotId())
	}
	if len(want) != 0 {
		t.Fatalf("missing members: %v", want)
	}

	// Now restore one member through the ordinary single-volume path.
	s.HandleValue("pool.snapshot.query", []any{map[string]any{"id": "Pool0/k8s/pvc-data@grp1"}})
	cv, err := ctrl.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name: "pvc-restored", Parameters: params(), VolumeCapabilities: testCaps(),
		CapacityRange: &csipb.CapacityRange{RequiredBytes: 1 << 30},
		VolumeContentSource: &csipb.VolumeContentSource{Type: &csipb.VolumeContentSource_Snapshot{
			Snapshot: &csipb.VolumeContentSource_SnapshotSource{SnapshotId: "nas1/Pool0/k8s/pvc-data@grp1"}}},
	})
	if err != nil {
		t.Fatalf("a group member must be restorable on its own: %v", err)
	}
	if cv.GetVolume().GetContentSource().GetSnapshot().GetSnapshotId() != "Pool0/k8s/pvc-data@grp1" {
		t.Errorf("restored volume does not name its source snapshot: %v", cv.GetVolume().GetContentSource())
	}
}

// TestDeleteGroupSnapshotRefusesWithDependentClone mirrors DeleteSnapshot: a
// snapshot with a dependent clone cannot be destroyed, and promotion is not an
// escape hatch — it would invert the dependency and strand the source volume.
func TestDeleteGroupSnapshotRefusesWithDependentClone(t *testing.T) {
	gc, _, s := gctlWith(t)
	s.HandleValue("pool.snapshot.query", []any{
		map[string]any{"id": "Pool0/k8s@grp1", "name": "grp1", "dataset": "Pool0/k8s"},
		map[string]any{"id": "Pool0/k8s/pvc-data@grp1", "name": "grp1", "dataset": "Pool0/k8s/pvc-data"},
		map[string]any{"id": "Pool0/k8s/pvc-wal@grp1", "name": "grp1", "dataset": "Pool0/k8s/pvc-wal"},
	})
	s.Handle("pool.dataset.query", func([]json.RawMessage) (any, error) {
		var v any
		_ = json.Unmarshal([]byte(`[{"id":"Pool0/k8s/pvc-restored",
		  "origin":{"parsed":"Pool0/k8s/pvc-data@grp1","value":"Pool0/k8s/pvc-data@grp1","source":"NONE"}}]`), &v)
		return v, nil
	})
	s.HandleValue("pool.snapshot.delete", nil)

	_, err := gc.DeleteVolumeGroupSnapshot(context.Background(), &csipb.DeleteVolumeGroupSnapshotRequest{
		GroupSnapshotId: "nas1/Pool0/k8s@grp1",
		SnapshotIds:     []string{"nas1/Pool0/k8s/pvc-data@grp1", "nas1/Pool0/k8s/pvc-wal@grp1"},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	if !strings.Contains(err.Error(), "pvc-restored") {
		t.Errorf("the error must name the dependent volume: %v", err)
	}
	if n := s.CallsTo("pool.snapshot.delete"); n != 0 {
		t.Fatalf("nothing may be deleted while a member still has a clone, got %d deletes", n)
	}
}

// TestGroupSnapshotNeverPromotes guards the same correction the single-volume
// path documents: pool.dataset.promote does not free the snapshot, it inverts
// the dependency and leaves the SOURCE volume undeletable.
func TestGroupSnapshotNeverPromotes(t *testing.T) {
	gc, _, s := gctlWith(t)
	s.HandleValue("pool.snapshot.query", []any{})
	existingDatasets(s)
	handleSnapshotCreate(s)

	if _, err := gc.CreateVolumeGroupSnapshot(context.Background(), &csipb.CreateVolumeGroupSnapshotRequest{
		Name:            "grp1",
		SourceVolumeIds: []string{gvid("pvc-data"), gvid("pvc-wal")},
	}); err != nil {
		t.Fatal(err)
	}
	s.HandleValue("pool.snapshot.query", []any{
		map[string]any{"id": "Pool0/k8s@grp1", "name": "grp1", "dataset": "Pool0/k8s"},
		map[string]any{"id": "Pool0/k8s/pvc-data@grp1", "name": "grp1", "dataset": "Pool0/k8s/pvc-data"},
	})
	s.Handle("pool.dataset.query", func([]json.RawMessage) (any, error) {
		var v any
		_ = json.Unmarshal([]byte(`[{"id":"Pool0/k8s/pvc-clone",
		  "origin":{"parsed":"Pool0/k8s/pvc-data@grp1","value":"Pool0/k8s/pvc-data@grp1","source":"NONE"}}]`), &v)
		return v, nil
	})
	_, _ = gc.DeleteVolumeGroupSnapshot(context.Background(), &csipb.DeleteVolumeGroupSnapshotRequest{
		GroupSnapshotId: "nas1/Pool0/k8s@grp1",
		SnapshotIds:     []string{"nas1/Pool0/k8s/pvc-data@grp1"},
	})
	for _, c := range s.Calls() {
		if strings.Contains(c, "promote") {
			t.Fatalf("promote must never be called: %v", s.Calls())
		}
	}
}

func TestGroupControllerCapabilities(t *testing.T) {
	gc, _, _ := gctlWith(t)
	resp, err := gc.GroupControllerGetCapabilities(context.Background(),
		&csipb.GroupControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range resp.GetCapabilities() {
		if c.GetRpc().GetType() == csipb.GroupControllerServiceCapability_RPC_CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT {
			found = true
		}
	}
	if !found {
		t.Fatalf("CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT must be advertised, got %v", resp.GetCapabilities())
	}
}

// TestGetVolumeGroupSnapshotReturnsMembers: the CO polls this to learn whether
// the group is usable, so it must report every member snapshot it holds.
func TestGetVolumeGroupSnapshotReturnsMembers(t *testing.T) {
	gc, _, s := gctlWith(t)
	s.HandleValue("pool.snapshot.query", []any{
		map[string]any{"id": "Pool0/k8s@grp1", "name": "grp1", "dataset": "Pool0/k8s"},
		map[string]any{"id": "Pool0/k8s/pvc-data@grp1", "name": "grp1", "dataset": "Pool0/k8s/pvc-data"},
		map[string]any{"id": "Pool0/k8s/pvc-wal@grp1", "name": "grp1", "dataset": "Pool0/k8s/pvc-wal"},
	})
	resp, err := gc.GetVolumeGroupSnapshot(context.Background(), &csipb.GetVolumeGroupSnapshotRequest{
		GroupSnapshotId: "nas1/Pool0/k8s@grp1",
		SnapshotIds:     []string{"nas1/Pool0/k8s/pvc-data@grp1", "nas1/Pool0/k8s/pvc-wal@grp1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(resp.GetGroupSnapshot().GetSnapshots()); got != 2 {
		t.Fatalf("want the 2 member snapshots, got %d: %v", got, resp.GetGroupSnapshot().GetSnapshots())
	}
	if resp.GetGroupSnapshot().GetGroupSnapshotId() != "nas1/Pool0/k8s@grp1" {
		t.Errorf("group id changed: %q", resp.GetGroupSnapshot().GetGroupSnapshotId())
	}
}

// TestGetVolumeGroupSnapshotAbsent: a group that is not on the appliance is
// NotFound, not an empty success the CO would treat as ready.
func TestGetVolumeGroupSnapshotAbsent(t *testing.T) {
	gc, _, s := gctlWith(t)
	s.HandleValue("pool.snapshot.query", []any{})
	_, err := gc.GetVolumeGroupSnapshot(context.Background(), &csipb.GetVolumeGroupSnapshotRequest{
		GroupSnapshotId: "nas1/Pool0/k8s@gone",
		SnapshotIds:     []string{"nas1/Pool0/k8s/pvc-data@gone"},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}

// TestDeleteGroupSnapshotAbsentIsSuccess: CSI requires deletion to be
// idempotent.
func TestDeleteGroupSnapshotAbsentIsSuccess(t *testing.T) {
	gc, _, s := gctlWith(t)
	s.HandleValue("pool.snapshot.query", []any{})
	if _, err := gc.DeleteVolumeGroupSnapshot(context.Background(), &csipb.DeleteVolumeGroupSnapshotRequest{
		GroupSnapshotId: "nas1/Pool0/k8s@gone",
		SnapshotIds:     []string{"nas1/Pool0/k8s/pvc-data@gone"},
	}); err != nil {
		t.Fatalf("deleting an absent group must succeed, got %v", err)
	}
}
