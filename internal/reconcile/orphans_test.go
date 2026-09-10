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
	t.Cleanup(func() { _ = r.Close() })
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

// TestProtocolIsReadFromTheDatasetNotGuessed pins a bug the NVMe backend
// exposed: an iSCSI volume and an NVMe-oF volume are both zvols, so inferring
// the protocol from the dataset type labelled every NVMe volume "iscsi". The
// resulting volume id matches no PersistentVolume, so a live volume is reported
// as an orphan.
func TestProtocolIsReadFromTheDatasetNotGuessed(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.Handle("pool.dataset.query", datasetsJSON(t, `[
	 {"id":"Pool0/k8s/pvc-nvme","type":"VOLUME","volsize":{"parsed":1073741824},
	  "user_properties":{
	    "io.truenas.csi:managed":{"value":"truenas-csi","source":"LOCAL"},
	    "io.truenas.csi:protocol":{"value":"nvme","source":"LOCAL"}}},
	 {"id":"Pool0/k8s/pvc-legacy","type":"VOLUME","volsize":{"parsed":1073741824},
	  "user_properties":{
	    "io.truenas.csi:managed":{"value":"truenas-csi","source":"LOCAL"}}}]`))
	r := regFor(t, s)

	// The NVMe volume has a live PV under its true protocol; the legacy one does not.
	lister := stubLister{handles: map[string]struct{}{
		"nas1/nvme/Pool0/k8s/pvc-nvme": {},
	}}
	orphans, err := NewOrphanReconciler(r, lister, 0).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range orphans {
		if strings.Contains(o, "pvc-nvme") {
			t.Fatalf("a live NVMe volume was reported as an orphan (%s): its protocol was "+
				"guessed from the dataset type instead of read from the dataset", o)
		}
	}
	// The pre-property volume still falls back to the historical guess.
	if len(orphans) != 1 || !strings.Contains(orphans[0], "iscsi") {
		t.Fatalf("a volume created before the protocol property should fall back to "+
			"iscsi, got %v", orphans)
	}
}

// graveyardListing is what the appliance returns for a driver whose delete
// protection has retired one volume: the graveyard container, the retired
// volume inside it (INHERITING the graveyard's marker, as ZFS does), and one
// live volume with no PersistentVolume.
const graveyardListing = `[
 {"id":"Pool0/k8s/.trash","type":"FILESYSTEM",
  "user_properties":{"io.truenas.csi:managed":{"value":"truenas-csi","source":"LOCAL"},
                     "io.truenas.csi:graveyard":{"value":"truenas-csi","source":"LOCAL"}}},
 {"id":"Pool0/k8s/.trash/20260801T000000Z-pvc-dead","type":"FILESYSTEM",
  "user_properties":{"io.truenas.csi:managed":{"value":"truenas-csi","source":"LOCAL"},
                     "io.truenas.csi:graveyard":{"value":"truenas-csi","source":"INHERITED"},
                     "io.truenas.csi:deleted-at":{"value":"2026-08-01T00:00:00Z","source":"LOCAL"},
                     "io.truenas.csi:retired-from":{"value":"nas1/nfs/Pool0/k8s/pvc-dead","source":"LOCAL"}}},
 {"id":"Pool0/k8s/pvc-a","type":"FILESYSTEM","refquota":{"parsed":1073741824},
  "user_properties":{"io.truenas.csi:managed":{"value":"truenas-csi","source":"LOCAL"}}}]`

// TestOrphanReconcilerIgnoresTheGraveyard. Both shapes are driver-owned and
// neither will ever have a PersistentVolume — that is what delete protection
// means — so reporting them would put a permanent false positive on every
// scan, which is how an operator learns to ignore this report entirely.
//
// The retired volume is the interesting one: it sits at a depth this driver
// never provisions into, so a handle derived from it would name ".trash" as a
// Kubernetes namespace.
func TestOrphanReconcilerIgnoresTheGraveyard(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.Handle("pool.dataset.query", datasetsJSON(t, graveyardListing))
	r := regFor(t, s)

	got, err := NewOrphanReconciler(r, stubLister{handles: map[string]struct{}{}}, 0).
		RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	want := "nas1/nfs/Pool0/k8s/pvc-a"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("reported %v, want only the live volume %q", got, want)
	}
	for _, unwanted := range got {
		if strings.Contains(unwanted, ".trash") {
			t.Errorf("the orphan report names a graveyard dataset: %q", unwanted)
		}
	}
}

// namespaceListing is what the appliance returns for a driver with namespace
// quotas on: the namespace's parent dataset, which the driver creates and owns,
// and one volume inside it that the CO knows about.
//
// The parent dataset INHERITS nothing — its namespace marker is LOCAL, which is
// exactly how it is told apart from the volumes beneath it, whose own marker is
// inherited from it.
const namespaceListing = `[
 {"id":"Pool0/k8s/shop","type":"FILESYSTEM",
  "user_properties":{"io.truenas.csi:managed":{"value":"truenas-csi","source":"LOCAL"},
                     "io.truenas.csi:namespace":{"value":"shop","source":"LOCAL"}}},
 {"id":"Pool0/k8s/shop/pvc-a","type":"FILESYSTEM","refquota":{"parsed":1073741824},
  "user_properties":{"io.truenas.csi:managed":{"value":"truenas-csi","source":"LOCAL"},
                     "io.truenas.csi:namespace":{"value":"shop","source":"INHERITED"}}}]`

// TestOrphanReconcilerIgnoresANamespaceParent.
//
// A namespace's parent dataset is driver-owned and has no PersistentVolume BY
// DESIGN — it accounts for a namespace's quota, it holds no data of its own,
// and nothing will ever create a PV for it. This scanner's own comment says so
// and says why it must be skipped; the code skipped only the graveyard shapes,
// so every scan reported it, for ever, on any cluster with namespace quotas on.
//
// A permanent false positive is worse than no report: truenas_csi_orphaned_volumes
// never reaches zero, and an operator learns to ignore the one signal that says
// a real dataset has been leaked. ListVolumes already skips it by the same test.
func TestOrphanReconcilerIgnoresANamespaceParent(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.Handle("pool.dataset.query", datasetsJSON(t, namespaceListing))
	r := regFor(t, s)

	// The volume inside the namespace has a PersistentVolume; the parent never
	// can. So a correct scan reports nothing at all.
	live := map[string]struct{}{"nas1/nfs/Pool0/k8s/shop/pvc-a": {}}
	got, err := NewOrphanReconciler(r, stubLister{handles: live}, 0).
		RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("reported %v as orphaned; a namespace's parent dataset has no "+
			"PersistentVolume by design, so this fires on every scan for ever and "+
			"trains the operator to ignore the report", got)
	}
}
