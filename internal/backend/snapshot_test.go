package backend

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"github.com/piwi3910/truenas-csi/internal/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func regWith(t *testing.T, s *fake.Server) *Registry {
	t.Helper()
	r, err := NewRegistry(context.Background(), cfgWith(t, map[string]string{"nas1": s.URL()}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func srcID() volume.ID {
	return volume.ID{Backend: "nas1", Protocol: "nfs", Pool: "Pool0", Parent: "k8s", Name: "pvc-1"}
}

// TestSnapshotNeverPromotes guards a correction that live probing forced:
// pool.dataset.promote does NOT free the source snapshot, it INVERTS the
// dependency and leaves the source volume undeletable.
func TestSnapshotNeverPromotes(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("pool.snapshot.query", []any{})
	s.HandleValue("pool.snapshot.create", map[string]any{"id": "Pool0/k8s/pvc-1@snap1"})
	r := regWith(t, s)

	if _, err := r.CreateSnapshot(context.Background(), srcID(), "snap1"); err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Calls() {
		if strings.Contains(c, "promote") {
			t.Fatalf("promote must never be called: %v", s.Calls())
		}
	}
}

func TestDeleteSnapshotWithDependentClone(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("pool.snapshot.query", []any{map[string]any{"id": "Pool0/k8s/pvc-1@snap1"}})
	s.Handle("pool.dataset.query", func([]json.RawMessage) (any, error) {
		var v any
		// The middleware UPPERCASES origin's display value and keeps the true
		// name only in parsed/rawvalue — verified on a real appliance. The
		// fixture used to echo it back verbatim, which let this guard pass here
		// while never once matching a clone against the appliance.
		_ = json.Unmarshal([]byte(`[{"id":"Pool0/k8s/pvc-restored",
		  "origin":{"parsed":"Pool0/k8s/pvc-1@snap1","rawvalue":"Pool0/k8s/pvc-1@snap1",
		            "value":"POOL0/K8S/PVC-1@SNAP1","source":"NONE"}}]`), &v)
		return v, nil
	})
	r := regWith(t, s)

	err := r.DeleteSnapshot(context.Background(), "nas1/Pool0/k8s/pvc-1@snap1")
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	if !strings.Contains(err.Error(), "pvc-restored") {
		t.Errorf("error should name the dependent volume: %v", err)
	}
	for _, c := range s.Calls() {
		if c == "pool.snapshot.delete" {
			t.Fatal("must not delete a snapshot that still has clones")
		}
	}
}

func TestDeleteSnapshotAbsentIsSuccess(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("pool.snapshot.query", []any{})
	r := regWith(t, s)
	if err := r.DeleteSnapshot(context.Background(), "nas1/Pool0/k8s/pvc-1@gone"); err != nil {
		t.Fatalf("deleting an absent snapshot must succeed, got %v", err)
	}
}

func TestDeleteSnapshotNoClones(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("pool.snapshot.query", []any{map[string]any{"id": "Pool0/k8s/pvc-1@snap1"}})
	s.HandleValue("pool.dataset.query", []any{})
	s.HandleValue("pool.snapshot.delete", nil)
	r := regWith(t, s)
	if err := r.DeleteSnapshot(context.Background(), "nas1/Pool0/k8s/pvc-1@snap1"); err != nil {
		t.Fatalf("want success, got %v", err)
	}
	if s.CallsTo("pool.snapshot.delete") != 1 {
		t.Fatalf("expected one delete, calls: %v", s.Calls())
	}
}

func TestSnapshotIDCarriesBackend(t *testing.T) {
	for _, bad := range []string{"", "nas1", "nas1/", "/Pool0/x@s", "nas1/Pool0/k8s/pvc-1"} {
		if _, _, err := SnapshotSource(bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("SnapshotSource(%q): want InvalidArgument, got %v", bad, err)
		}
	}
	b, z, err := SnapshotSource("nas1/Pool0/k8s/pvc-1@snap1")
	if err != nil || b != "nas1" || z != "Pool0/k8s/pvc-1@snap1" {
		t.Fatalf("got (%q,%q,%v)", b, z, err)
	}
}

// TestCreateSnapshotReportsProvisionedSize pins the fix for an empty
// status.restoreSize on every VolumeSnapshot. CreateSnapshot never set
// SizeBytes, so external-snapshotter had nothing to publish and
// external-provisioner could not refuse a restore claim smaller than the
// source volume.
//
// The property block below is the shape a real 25.10 appliance returns for a
// zvol snapshot: "creation" parses as a {"$date": ms} object, so the seconds
// have to come from "rawvalue", and "volsize" is the source's PROVISIONED
// size, not the snapshot's own (near-zero) referenced bytes.
func TestCreateSnapshotReportsProvisionedSize(t *testing.T) {
	const zvolSnapshot = `[{
	  "id": "Pool0/k8s/pvc-1@snap1",
	  "dataset": "Pool0/k8s/pvc-1",
	  "properties": {
	    "creation": {"parsed": {"$date": 1788937661000}, "rawvalue": "1788937661",
	                 "value": "Wed Sep  9 11:07 2026", "source": "NONE"},
	    "referenced": {"parsed": 252720, "rawvalue": "252720", "value": "246K", "source": "NONE"},
	    "volsize": {"parsed": 3221225472, "rawvalue": "3221225472", "value": "3G", "source": "NONE"}
	  }
	}]`
	s := fake.Start(t, fake.Options{})
	var queried bool
	s.Handle("pool.snapshot.query", func([]json.RawMessage) (any, error) {
		if !queried { // the create path looks first, and must find nothing
			queried = true
			return []any{}, nil
		}
		var v any
		_ = json.Unmarshal([]byte(zvolSnapshot), &v)
		return v, nil
	})
	s.HandleValue("pool.snapshot.create", map[string]any{"id": "Pool0/k8s/pvc-1@snap1"})
	r := regWith(t, s)

	snap, err := r.CreateSnapshot(context.Background(), srcID(), "snap1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.SizeBytes != 3221225472 {
		t.Errorf("SizeBytes = %d, want the source volsize 3221225472", snap.SizeBytes)
	}
	if got := snap.CreationTime.Unix(); got != 1788937661 {
		t.Errorf("CreationTime = %d, want ZFS's 1788937661", got)
	}
}

// TestCreateSnapshotFilesystemSizeComesFromDataset covers the other half: a
// FILESYSTEM snapshot carries no volsize and no refquota at all — verified
// against a live appliance — so the size has to be read off the live dataset.
func TestCreateSnapshotFilesystemSizeComesFromDataset(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("pool.snapshot.query", []any{map[string]any{
		"id": "Pool0/k8s/pvc-1@snap1", "dataset": "Pool0/k8s/pvc-1",
		"properties": map[string]any{
			"creation": map[string]any{"rawvalue": "1788937661"},
		},
	}})
	s.Handle("pool.dataset.query", func([]json.RawMessage) (any, error) {
		var v any
		_ = json.Unmarshal([]byte(`[{"id":"Pool0/k8s/pvc-1","type":"FILESYSTEM",
		  "refquota":{"parsed":1073741824}}]`), &v)
		return v, nil
	})
	r := regWith(t, s)

	snap, err := r.CreateSnapshot(context.Background(), srcID(), "snap1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.SizeBytes != 1073741824 {
		t.Errorf("SizeBytes = %d, want the source refquota 1073741824", snap.SizeBytes)
	}
}

// TestDeleteGroupSnapshotWithDependentClone: the group guard had the same hole
// as the single-snapshot one and no test at all.
//
// It keyed a map of clones by origin's display value, which the middleware
// returns UPPERCASED, then looked it up by the real snapshot id -- so the
// lookup never hit, the guard never fired, and a group snapshot with dependent
// volumes went to the delete path instead of being refused by name.
func TestDeleteGroupSnapshotWithDependentClone(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("pool.snapshot.query", []any{
		map[string]any{"id": "Pool0/k8s@grp1", "dataset": "Pool0/k8s"},
		map[string]any{"id": "Pool0/k8s/pvc-1@grp1", "dataset": "Pool0/k8s/pvc-1"},
	})
	s.Handle("pool.dataset.query", func([]json.RawMessage) (any, error) {
		var v any
		_ = json.Unmarshal([]byte(`[{"id":"Pool0/k8s/pvc-restored",
		  "origin":{"parsed":"Pool0/k8s/pvc-1@grp1","rawvalue":"Pool0/k8s/pvc-1@grp1",
		            "value":"POOL0/K8S/PVC-1@GRP1","source":"NONE"}}]`), &v)
		return v, nil
	})
	r := regWith(t, s)

	err := r.DeleteGroupSnapshot(context.Background(), "nas1/Pool0/k8s@grp1",
		[]string{"nas1/Pool0/k8s/pvc-1@grp1"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	if !strings.Contains(err.Error(), "pvc-restored") {
		t.Errorf("error should name the dependent volume: %v", err)
	}
	for _, c := range s.Calls() {
		if c == "pool.snapshot.delete" {
			t.Fatal("must not delete a group snapshot that still has clones")
		}
	}
}
