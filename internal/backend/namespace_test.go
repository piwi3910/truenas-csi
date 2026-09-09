package backend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// nsDataset builds a pool.dataset.query answer for a namespace dataset.
// owner and marker are the property SOURCES, so a test can reproduce the
// inherited-marker case the ownership guard exists to catch.
func nsDataset(id, namespace, ownerSource, recordedQuota string, used int64) map[string]any {
	props := map[string]any{}
	if ownerSource != "" {
		props[volume.OwnerProperty] = map[string]any{
			"value": volume.OwnerValue, "source": ownerSource}
	}
	if namespace != "" {
		props[volume.NamespaceProperty] = map[string]any{
			"value": namespace, "source": "LOCAL"}
	}
	if recordedQuota != "" {
		props[volume.NamespaceQuotaProperty] = map[string]any{
			"value": recordedQuota, "source": "LOCAL"}
	}
	return map[string]any{
		"id": id, "type": "FILESYSTEM",
		"used":            map[string]any{"parsed": used},
		"user_properties": props,
	}
}

// datasetQuery scripts pool.dataset.query, which the driver uses two ways: an
// exact id lookup ("=") and a prefix listing ("^"). The fake gets one handler
// for the method, so the filter operator is what tells them apart.
func datasetQuery(t *testing.T, byID map[string]any, children []any) fake.Handler {
	t.Helper()
	return func(params []json.RawMessage) (any, error) {
		filter := string(params[0])
		if strings.Contains(filter, `"^"`) {
			return children, nil
		}
		if byID == nil {
			return []any{}, nil
		}
		return []any{byID}, nil
	}
}

func clientWith(t *testing.T, s *fake.Server) truenas.API {
	t.Helper()
	r := regWith(t, s)
	c, err := r.Client(context.Background(), "nas1")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func called(s *fake.Server, method string) bool {
	for _, c := range s.Calls() {
		if strings.Contains(c, method) {
			return true
		}
	}
	return false
}

func TestEnsureNamespace(t *testing.T) {
	const path = "Pool0/k8s/team-a"

	cases := []struct {
		name string
		// existing is the dataset already on the appliance, nil for none.
		existing      map[string]any
		quota         int64
		wantCreate    bool
		wantUpdate    bool
		wantDeferred  bool
		wantUsedBytes int64
	}{
		{
			name:       "missing dataset is created and the quota applied",
			quota:      10 << 30,
			wantCreate: true,
			wantUpdate: true,
		},
		{
			name:       "unlimited still creates the dataset, so usage is visible",
			quota:      0,
			wantCreate: true,
			wantUpdate: true, // records quota 0, so a later change is detectable
		},
		{
			name:          "an already-applied quota costs no write",
			existing:      nsDataset(path, "team-a", "LOCAL", "10737418240", 1<<30),
			quota:         10 << 30,
			wantUsedBytes: 1 << 30,
		},
		{
			name:          "raising a quota is applied immediately",
			existing:      nsDataset(path, "team-a", "LOCAL", "10737418240", 1<<30),
			quota:         20 << 30,
			wantUpdate:    true,
			wantUsedBytes: 1 << 30,
		},
		{
			name:          "lowering below usage is deferred, never applied",
			existing:      nsDataset(path, "team-a", "LOCAL", "10737418240", 8<<30),
			quota:         4 << 30,
			wantDeferred:  true,
			wantUsedBytes: 8 << 30,
		},
		{
			name:          "a dataset from before the quota was recorded gets it applied",
			existing:      nsDataset(path, "team-a", "LOCAL", "", 0),
			quota:         10 << 30,
			wantUpdate:    true,
			wantUsedBytes: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := fake.Start(t, fake.Options{})
			s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
			s.Handle("pool.dataset.query", datasetQuery(t, tc.existing, nil))
			s.HandleValue("pool.dataset.create", nsDataset(path, "team-a", "LOCAL", "", 0))
			s.HandleValue("pool.dataset.update", nsDataset(path, "team-a", "LOCAL", "", 0))
			c := clientWith(t, s)

			ns, err := EnsureNamespace(context.Background(), c, "Pool0", "k8s", "team-a", tc.quota)
			if err != nil {
				t.Fatalf("EnsureNamespace: %v", err)
			}
			if ns.Path != path {
				t.Errorf("Path = %q, want %q", ns.Path, path)
			}
			if ns.UsedBytes != tc.wantUsedBytes {
				t.Errorf("UsedBytes = %d, want %d", ns.UsedBytes, tc.wantUsedBytes)
			}
			if ns.QuotaDeferred != tc.wantDeferred {
				t.Errorf("QuotaDeferred = %v, want %v", ns.QuotaDeferred, tc.wantDeferred)
			}
			if got := called(s, "pool.dataset.create"); got != tc.wantCreate {
				t.Errorf("dataset created = %v, want %v (calls: %v)", got, tc.wantCreate, s.Calls())
			}
			if got := called(s, "pool.dataset.update"); got != tc.wantUpdate {
				t.Errorf("dataset updated = %v, want %v (calls: %v)", got, tc.wantUpdate, s.Calls())
			}
		})
	}
}

// TestEnsureNamespaceRefusesADatasetItDoesNotOwn is the guard that keeps the
// feature from adopting an operator's data. A dataset that merely INHERITED the
// ownership marker, or one that is a volume rather than a container, must be
// refused rather than filled with volumes and later put on the reclaim path.
func TestEnsureNamespaceRefusesADatasetItDoesNotOwn(t *testing.T) {
	const path = "Pool0/k8s/team-a"
	cases := []struct {
		name     string
		existing map[string]any
	}{
		{name: "no ownership marker", existing: nsDataset(path, "team-a", "", "", 0)},
		{name: "inherited marker", existing: nsDataset(path, "team-a", "INHERITED", "", 0)},
		{name: "owned but not a namespace container", existing: nsDataset(path, "", "LOCAL", "", 0)},
		{name: "accounts for another namespace", existing: nsDataset(path, "team-b", "LOCAL", "", 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := fake.Start(t, fake.Options{})
			s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
			s.Handle("pool.dataset.query", datasetQuery(t, tc.existing, nil))
			c := clientWith(t, s)

			_, err := EnsureNamespace(context.Background(), c, "Pool0", "k8s", "team-a", 1<<30)
			if !errors.Is(err, ErrNamespaceDatasetConflict) {
				t.Fatalf("err = %v, want ErrNamespaceDatasetConflict", err)
			}
			if called(s, "pool.dataset.update") || called(s, "pool.dataset.create") {
				t.Fatalf("a refused dataset must not be written to: %v", s.Calls())
			}
		})
	}
}

func TestEnsureNamespaceRejectsAHostileNamespace(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
	c := clientWith(t, s)

	_, err := EnsureNamespace(context.Background(), c, "Pool0", "k8s", "../../etc", 1<<30)
	if !errors.Is(err, volume.ErrInvalidNamespace) {
		t.Fatalf("err = %v, want ErrInvalidNamespace", err)
	}
	if called(s, "pool.dataset") {
		t.Fatalf("a hostile namespace must never reach the appliance: %v", s.Calls())
	}
}

// TestReclaimNamespace covers the destructive path. The case that matters most
// is the third: a dataset the driver did not create must survive the reclaim
// sweep, with no delete call issued at all.
func TestReclaimNamespace(t *testing.T) {
	const path = "Pool0/k8s/team-a"
	cases := []struct {
		name        string
		existing    map[string]any
		children    []any
		wantDeleted bool
		wantErr     error
	}{
		{
			name:     "already gone",
			existing: nil,
		},
		{
			name:        "empty and owned is reclaimed",
			existing:    nsDataset(path, "team-a", "LOCAL", "0", 0),
			wantDeleted: true,
		},
		{
			name:     "still holds a volume",
			existing: nsDataset(path, "team-a", "LOCAL", "0", 1<<30),
			children: []any{map[string]any{"id": path + "/pvc-1"}},
		},
		{
			name:     "not driver-owned",
			existing: nsDataset(path, "team-a", "", "", 0),
			wantErr:  ErrNamespaceDatasetConflict,
		},
		{
			name:     "marker only inherited",
			existing: nsDataset(path, "team-a", "INHERITED", "", 0),
			wantErr:  ErrNamespaceDatasetConflict,
		},
		{
			name:     "owned volume, not a namespace container",
			existing: nsDataset(path, "", "LOCAL", "", 0),
			wantErr:  ErrNamespaceDatasetConflict,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := fake.Start(t, fake.Options{})
			s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
			s.Handle("pool.dataset.query", datasetQuery(t, tc.existing, tc.children))
			s.HandleValue("pool.dataset.delete", true)
			c := clientWith(t, s)

			deleted, err := ReclaimNamespace(context.Background(), c, "Pool0", "k8s", "team-a")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("ReclaimNamespace: %v", err)
			}
			if deleted != tc.wantDeleted {
				t.Errorf("deleted = %v, want %v", deleted, tc.wantDeleted)
			}
			if got := called(s, "pool.dataset.delete"); got != tc.wantDeleted {
				t.Fatalf("delete issued = %v, want %v (calls: %v)", got, tc.wantDeleted, s.Calls())
			}
		})
	}
}

// TestReclaimNamespaceNeverForcesOrRecurses pins the two arguments that decide
// whether a refusal from ZFS is respected or overridden. A recursive, forced
// delete of a namespace dataset would destroy every volume inside it.
func TestReclaimNamespaceNeverForcesOrRecurses(t *testing.T) {
	const path = "Pool0/k8s/team-a"
	s := fake.Start(t, fake.Options{})
	s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
	s.Handle("pool.dataset.query", datasetQuery(t, nsDataset(path, "team-a", "LOCAL", "0", 0), nil))

	var opts string
	s.Handle("pool.dataset.delete", func(params []json.RawMessage) (any, error) {
		opts = string(params[1])
		return true, nil
	})
	c := clientWith(t, s)

	if _, err := ReclaimNamespace(context.Background(), c, "Pool0", "k8s", "team-a"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(opts, `"recursive":false`) || !strings.Contains(opts, `"force":false`) {
		t.Fatalf("delete options = %s, want recursive=false and force=false", opts)
	}
}

// volDataset is one of this driver's volumes under a namespace dataset: a thin
// filesystem whose refquota is a promise and whose `used` is nearly nothing.
func volDataset(id string, refquota int64) map[string]any {
	return map[string]any{
		"id": id, "type": "FILESYSTEM",
		"used":     map[string]any{"parsed": 0},
		"refquota": map[string]any{"parsed": refquota},
		"user_properties": map[string]any{
			volume.OwnerProperty: map[string]any{
				"value": volume.OwnerValue, "source": "LOCAL"},
			volume.OwnerIDProperty: map[string]any{
				"value": id, "source": "LOCAL"},
		},
	}
}

// TestNamespaceQuotaBoundsThinVolumes pins the ceiling the documentation
// promises and the implementation did not have.
//
// A namespace quota measured against ZFS `used` alone cannot bound thin
// volumes: ZFS charges nothing for a refquota until a byte is written into it,
// so a 2 GiB quota admitted an unlimited number of 1 GiB claims. Verified on
// hardware before the fix — three 1 GiB claims all bound under a 2 GiB quota,
// and the tenant would have met the limit as write failures spreading across
// workloads that were already running.
func TestNamespaceQuotaBoundsThinVolumes(t *testing.T) {
	const path = "Pool0/k8s/team-a"
	s := fake.Start(t, fake.Options{})
	s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
	s.Handle("pool.dataset.query", datasetQuery(t,
		nsDataset(path, "team-a", "LOCAL", "2147483648", 0),
		[]any{
			volDataset(path+"/pvc-1", 1<<30),
			volDataset(path+"/pvc-2", 1<<30),
		}))
	c := clientWith(t, s)

	ns, err := EnsureNamespace(context.Background(), c, "Pool0", "k8s", "team-a", 2<<30)
	if err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}
	if ns.UsedBytes != 0 {
		t.Fatalf("UsedBytes = %d, want 0 — the volumes are thin and empty", ns.UsedBytes)
	}
	if ns.ProvisionedBytes != 2<<30 {
		t.Errorf("ProvisionedBytes = %d, want %d (two 1 GiB refquotas)",
			ns.ProvisionedBytes, int64(2)<<30)
	}
	room, limited := ns.Room()
	if !limited {
		t.Fatal("Room reported no limit for a namespace with a 2 GiB quota")
	}
	if room != 0 {
		t.Errorf("Room = %d, want 0: the quota is fully promised even though "+
			"nothing has been written, so a third 1 GiB claim must be refused",
			room)
	}
}

// TestNamespaceQuotaCountsOnlyItsOwnDirectChildren guards the total against the
// three things that are not volumes of this namespace: a dataset the driver
// does not own, one nested deeper than a direct child, and the namespace
// dataset itself, which carries the quota rather than consuming it.
func TestNamespaceQuotaCountsOnlyItsOwnDirectChildren(t *testing.T) {
	const path = "Pool0/k8s/team-a"
	foreign := map[string]any{
		"id": path + "/not-ours", "type": "FILESYSTEM",
		"refquota":        map[string]any{"parsed": int64(500) << 30},
		"user_properties": map[string]any{},
	}
	s := fake.Start(t, fake.Options{})
	s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
	s.Handle("pool.dataset.query", datasetQuery(t,
		nsDataset(path, "team-a", "LOCAL", "10737418240", 0),
		[]any{
			volDataset(path+"/pvc-1", 1<<30),
			volDataset(path+"/pvc-1/nested", 8<<30), // deeper than a direct child
			foreign,
		}))
	c := clientWith(t, s)

	ns, err := EnsureNamespace(context.Background(), c, "Pool0", "k8s", "team-a", 10<<30)
	if err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}
	if ns.ProvisionedBytes != 1<<30 {
		t.Errorf("ProvisionedBytes = %d, want %d — only the one owned direct child counts",
			ns.ProvisionedBytes, int64(1)<<30)
	}
}
