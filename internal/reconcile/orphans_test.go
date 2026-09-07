package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
)

type stubLister struct {
	handles map[string]struct{}
	err     error
}

func (s stubLister) VolumeHandles(context.Context) (map[string]struct{}, error) {
	return s.handles, s.err
}

func regFor(t *testing.T, s *fake.Server) *backend.Registry {
	t.Helper()
	cfg := &config.Config{NodeID: "worker-21", Backends: map[string]config.Backend{
		"nas1": {Name: "nas1", Endpoint: s.URL(), Username: "truenas_admin", APIKey: "8-x",
			Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true},
	}}
	r, err := backend.NewRegistry(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func datasetsJSON(t *testing.T, body string) func([]json.RawMessage) (any, error) {
	t.Helper()
	return func([]json.RawMessage) (any, error) {
		var v any
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatal(err)
		}
		return v, nil
	}
}

const threeOwned = `[
 {"id":"Pool0/k8s/pvc-a","type":"FILESYSTEM","refquota":{"parsed":1073741824},
  "user_properties":{"io.truenas.csi:managed":{"value":"truenas-csi","source":"LOCAL"}}},
 {"id":"Pool0/k8s/pvc-b","type":"FILESYSTEM","refquota":{"parsed":1073741824},
  "user_properties":{"io.truenas.csi:managed":{"value":"truenas-csi","source":"LOCAL"}}},
 {"id":"Pool0/k8s/pvc-c","type":"FILESYSTEM","refquota":{"parsed":1073741824},
  "user_properties":{"io.truenas.csi:managed":{"value":"truenas-csi","source":"LOCAL"}}}]`

func TestOrphanReconcilerReports(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.Handle("pool.dataset.query", datasetsJSON(t, threeOwned))
	r := regFor(t, s)

	lister := stubLister{handles: map[string]struct{}{
		"nas1/nfs/Pool0/k8s/pvc-a": {},
		"nas1/nfs/Pool0/k8s/pvc-b": {},
	}}
	o := NewOrphanReconciler(r, lister, 0)

	orphans, err := o.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0] != "nas1/nfs/Pool0/k8s/pvc-c" {
		t.Fatalf("want exactly the unreferenced volume, got %v", orphans)
	}
	// The whole point: reporting, never deleting.
	for _, c := range s.Calls() {
		if strings.Contains(c, "delete") {
			t.Fatalf("the reconciler must never delete anything, saw %q in %v", c, s.Calls())
		}
	}
}

// TestOrphanReconcilerIgnoresUnowned: a dataset that merely INHERITED the marker
// is pre-existing user data, not ours, and must never be reported.
func TestOrphanReconcilerIgnoresUnowned(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.Handle("pool.dataset.query", datasetsJSON(t, `[
	 {"id":"Pool0/k8s/inherited","type":"FILESYSTEM",
	  "user_properties":{"io.truenas.csi:managed":{"value":"truenas-csi","source":"INHERITED"}}},
	 {"id":"Pool0/k8s/unmarked","type":"FILESYSTEM","user_properties":{}}]`))
	r := regFor(t, s)

	orphans, err := NewOrphanReconciler(r, stubLister{handles: map[string]struct{}{}}, 0).
		RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Fatalf("datasets we do not own must never be reported as orphans, got %v", orphans)
	}
}

// TestOrphanReconcilerSkipsOnListerError: a partial view of Kubernetes must not
// be read as evidence that live volumes are orphaned.
func TestOrphanReconcilerSkipsOnListerError(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.Handle("pool.dataset.query", datasetsJSON(t, threeOwned))
	r := regFor(t, s)

	orphans, err := NewOrphanReconciler(r, stubLister{err: errors.New("apiserver unreachable")}, 0).
		RunOnce(context.Background())
	if err == nil {
		t.Fatal("a failed PV listing must surface as an error")
	}
	if len(orphans) != 0 {
		t.Fatalf("nothing may be reported when the PV listing failed, got %v", orphans)
	}
	if n := s.CallsTo("pool.dataset.query"); n != 0 {
		t.Fatalf("must not even scan the appliance without a usable PV list, %d calls", n)
	}
}
