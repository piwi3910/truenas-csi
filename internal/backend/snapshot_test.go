package backend

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pwatteel/truenas-csi/internal/truenas/fake"
	"github.com/pwatteel/truenas-csi/internal/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func regWith(t *testing.T, s *fake.Server) *Registry {
	t.Helper()
	r, err := NewRegistry(context.Background(), cfgWith(t, map[string]string{"nas1": s.URL()}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
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
		_ = json.Unmarshal([]byte(`[{"id":"Pool0/k8s/pvc-restored",
		  "origin":{"parsed":"Pool0/k8s/pvc-1@snap1","value":"Pool0/k8s/pvc-1@snap1","source":"NONE"}}]`), &v)
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
