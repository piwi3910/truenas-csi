package csi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"github.com/piwi3910/truenas-csi/internal/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// nsCtl builds a controller whose single backend has per-namespace accounting
// configured as given, over a fake appliance the caller can script.
func nsCtl(t *testing.T, q config.NamespaceQuotas) (csipb.ControllerServer, *fake.Server) {
	t.Helper()
	s := fake.Start(t, fake.Options{})
	s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
	cfg := &config.Config{NodeID: "worker-21", Backends: map[string]config.Backend{
		"nas1": {Name: "nas1", Endpoint: s.URL(), Username: "truenas_admin", APIKey: "8-x",
			Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true, NamespaceQuotas: q},
	}}
	r, err := backend.NewRegistry(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return NewController(r, cfg), s
}

// nsParams are StorageClass parameters plus the PVC metadata the external
// provisioner injects when it runs with --extra-create-metadata.
func nsParams(namespace string) map[string]string {
	p := params()
	if namespace != "" {
		p[volume.ParamPVCNamespace] = namespace
		p[volume.ParamPVCName] = "claim-1"
	}
	return p
}

// serveNamespaceDataset scripts the appliance for one namespace dataset that
// already exists with the given usage.
func serveNamespaceDataset(s *fake.Server, path, namespace, recordedQuota string, used int64) {
	ds := map[string]any{
		"id": path, "type": "FILESYSTEM",
		"used": map[string]any{"parsed": used},
		"user_properties": map[string]any{
			volume.OwnerProperty:          map[string]any{"value": volume.OwnerValue, "source": "LOCAL"},
			volume.NamespaceProperty:      map[string]any{"value": namespace, "source": "LOCAL"},
			volume.NamespaceQuotaProperty: map[string]any{"value": recordedQuota, "source": "LOCAL"},
		},
	}
	s.Handle("pool.dataset.query", func(params []json.RawMessage) (any, error) {
		if strings.Contains(string(params[0]), `"^"`) {
			return []any{}, nil
		}
		return []any{ds}, nil
	})
	s.HandleValue("pool.dataset.update", ds)
	s.HandleValue("pool.dataset.create", ds)
	s.HandleValue("pool.dataset.delete", true)
}

func createVolume(t *testing.T, c csipb.ControllerServer, name string, ns string, size int64) (*csipb.CreateVolumeResponse, error) {
	t.Helper()
	return c.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name:               name,
		Parameters:         nsParams(ns),
		VolumeCapabilities: testCaps(),
		CapacityRange:      &csipb.CapacityRange{RequiredBytes: size},
	})
}

// TestCreateVolumeRefusesANamespaceOverQuota is the enforcement test. A
// reported capacity is advisory and a PVC can walk straight past it, so the
// refusal has to happen at CreateVolume with ResourceExhausted.
func TestCreateVolumeRefusesANamespaceOverQuota(t *testing.T) {
	cases := []struct {
		name          string
		quotas        config.NamespaceQuotas
		used          int64
		recordedQuota string
		request       int64
		wantCode      codes.Code
	}{
		{
			name:     "fits under the default quota",
			quotas:   config.NamespaceQuotas{Enabled: true, DefaultBytes: 10 << 30},
			used:     1 << 30,
			request:  2 << 30,
			wantCode: codes.OK,
		},
		{
			name:     "exceeds the default quota",
			quotas:   config.NamespaceQuotas{Enabled: true, DefaultBytes: 10 << 30},
			used:     9 << 30,
			request:  2 << 30,
			wantCode: codes.ResourceExhausted,
		},
		{
			name: "a per-namespace override raises the ceiling",
			quotas: config.NamespaceQuotas{Enabled: true, DefaultBytes: 1 << 30,
				PerNamespace: map[string]int64{"team-a": 100 << 30}},
			used:     9 << 30,
			request:  2 << 30,
			wantCode: codes.OK,
		},
		{
			name: "a per-namespace override lowers it",
			quotas: config.NamespaceQuotas{Enabled: true, DefaultBytes: 100 << 30,
				PerNamespace: map[string]int64{"team-a": 4 << 30}},
			used:     3 << 30,
			request:  2 << 30,
			wantCode: codes.ResourceExhausted,
		},
		{
			name:     "a zero quota is unlimited, not a ban",
			quotas:   config.NamespaceQuotas{Enabled: true},
			used:     900 << 30,
			request:  100 << 30,
			wantCode: codes.OK,
		},
		{
			name: "a quota lowered below usage refuses new volumes",
			quotas: config.NamespaceQuotas{Enabled: true,
				PerNamespace: map[string]int64{"team-a": 4 << 30}},
			used:          8 << 30,
			recordedQuota: "10737418240", // the higher quota still in force on ZFS
			request:       1 << 30,
			wantCode:      codes.ResourceExhausted,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shared = newCounting()
			c, s := nsCtl(t, tc.quotas)
			serveNamespaceDataset(s, "Pool0/k8s/team-a", "team-a", tc.recordedQuota, tc.used)

			resp, err := createVolume(t, c, "pvc-1", "team-a", tc.request)
			if got := status.Code(err); got != tc.wantCode {
				t.Fatalf("CreateVolume = %v (code %v), want %v", err, got, tc.wantCode)
			}
			if tc.wantCode != codes.OK {
				return
			}
			const want = "nas1/counting/Pool0/k8s/team-a/pvc-1"
			if got := resp.GetVolume().GetVolumeId(); got != want {
				t.Fatalf("volume id = %q, want %q", got, want)
			}
		})
	}
}

// TestNamespacedLayoutIsOptIn proves the feature is inert until asked for, and
// that even switched on it never invents a namespace it was not told.
func TestNamespacedLayoutIsOptIn(t *testing.T) {
	cases := []struct {
		name      string
		quotas    config.NamespaceQuotas
		namespace string // as delivered in CreateVolume parameters
		wantID    string
	}{
		{
			name:      "off: the namespace in the request is ignored",
			quotas:    config.NamespaceQuotas{},
			namespace: "team-a",
			wantID:    "nas1/counting/Pool0/k8s/pvc-1",
		},
		{
			name:   "on, but no metadata at all — the csi-sanity path",
			quotas: config.NamespaceQuotas{Enabled: true, DefaultBytes: 1 << 30},
			wantID: "nas1/counting/Pool0/k8s/pvc-1",
		},
		{
			name:      "on, with a namespace that is not a DNS-1123 label",
			quotas:    config.NamespaceQuotas{Enabled: true, DefaultBytes: 1 << 30},
			namespace: "../escape",
			wantID:    "nas1/counting/Pool0/k8s/pvc-1",
		},
		{
			name:      "on, with a real namespace",
			quotas:    config.NamespaceQuotas{Enabled: true, DefaultBytes: 100 << 30},
			namespace: "team-a",
			wantID:    "nas1/counting/Pool0/k8s/team-a/pvc-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shared = newCounting()
			c, s := nsCtl(t, tc.quotas)
			serveNamespaceDataset(s, "Pool0/k8s/team-a", "team-a", "", 0)

			resp, err := createVolume(t, c, "pvc-1", tc.namespace, 1<<30)
			if err != nil {
				t.Fatalf("CreateVolume: %v", err)
			}
			if got := resp.GetVolume().GetVolumeId(); got != tc.wantID {
				t.Fatalf("volume id = %q, want %q", got, tc.wantID)
			}
			if !strings.Contains(tc.wantID, "/team-a/") {
				// A flat volume must not touch the appliance's dataset surface
				// at all: no namespace dataset is created for it.
				for _, call := range s.Calls() {
					if strings.Contains(call, "pool.dataset.create") {
						t.Fatalf("a flat volume created a namespace dataset: %v", s.Calls())
					}
				}
			}
		})
	}
}

// TestFlatVolumeStillResolvesWithQuotasEnabled is the stranding test. Switching
// the feature on must not orphan the volumes that already exist: their handles
// were minted flat, they are immutable, and every subsequent RPC has to keep
// finding the dataset they always named.
func TestFlatVolumeStillResolvesWithQuotasEnabled(t *testing.T) {
	shared = newCounting()
	c, s := nsCtl(t, config.NamespaceQuotas{})
	serveNamespaceDataset(s, "Pool0/k8s/team-a", "team-a", "", 0)

	// Provision under the old, flat layout.
	resp, err := createVolume(t, c, "pvc-legacy", "team-a", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	handle := resp.GetVolume().GetVolumeId()
	if handle != "nas1/counting/Pool0/k8s/pvc-legacy" {
		t.Fatalf("legacy handle = %q", handle)
	}

	// The operator enables per-namespace accounting and the driver restarts.
	on, s2 := nsCtl(t, config.NamespaceQuotas{Enabled: true, DefaultBytes: 100 << 30})
	serveNamespaceDataset(s2, "Pool0/k8s/team-a", "team-a", "", 0)

	id, err := volume.ParseID(handle)
	if err != nil {
		t.Fatalf("the legacy handle stopped parsing: %v", err)
	}
	if id.Namespace != "" || id.DatasetPath() != "Pool0/k8s/pvc-legacy" {
		t.Fatalf("the legacy handle moved: namespace %q dataset %q", id.Namespace, id.DatasetPath())
	}
	if _, err := on.ControllerExpandVolume(context.Background(), &csipb.ControllerExpandVolumeRequest{
		VolumeId:      handle,
		CapacityRange: &csipb.CapacityRange{RequiredBytes: 2 << 30},
	}); err != nil {
		t.Fatalf("expanding a legacy volume: %v", err)
	}
	if _, err := on.DeleteVolume(context.Background(),
		&csipb.DeleteVolumeRequest{VolumeId: handle}); err != nil {
		t.Fatalf("deleting a legacy volume: %v", err)
	}
	// Nothing about a flat volume may trigger namespace reclamation.
	for _, call := range s2.Calls() {
		if strings.Contains(call, "pool.dataset.delete") {
			t.Fatalf("deleting a flat volume touched the namespace layout: %v", s2.Calls())
		}
	}
}

// TestDeleteVolumeReclaimsAnEmptyNamespace covers the other end: the last
// volume in a namespace takes the namespace dataset with it, and a namespace
// that still holds volumes keeps its dataset.
func TestDeleteVolumeReclaimsAnEmptyNamespace(t *testing.T) {
	cases := []struct {
		name        string
		children    []any
		wantDeleted bool
	}{
		{name: "last volume gone", wantDeleted: true},
		{name: "another volume remains",
			children: []any{map[string]any{"id": "Pool0/k8s/team-a/pvc-2"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shared = newCounting()
			c, s := nsCtl(t, config.NamespaceQuotas{Enabled: true, DefaultBytes: 100 << 30})

			ds := map[string]any{
				"id": "Pool0/k8s/team-a", "type": "FILESYSTEM",
				"used": map[string]any{"parsed": int64(0)},
				"user_properties": map[string]any{
					volume.OwnerProperty:     map[string]any{"value": volume.OwnerValue, "source": "LOCAL"},
					volume.NamespaceProperty: map[string]any{"value": "team-a", "source": "LOCAL"},
				},
			}
			s.Handle("pool.dataset.query", func(params []json.RawMessage) (any, error) {
				if strings.Contains(string(params[0]), `"^"`) {
					return tc.children, nil
				}
				return []any{ds}, nil
			})
			s.HandleValue("pool.dataset.update", ds)
			s.HandleValue("pool.dataset.create", ds)
			s.HandleValue("pool.dataset.delete", true)

			resp, err := createVolume(t, c, "pvc-1", "team-a", 1<<30)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.DeleteVolume(context.Background(),
				&csipb.DeleteVolumeRequest{VolumeId: resp.GetVolume().GetVolumeId()}); err != nil {
				t.Fatal(err)
			}
			deleted := false
			for _, call := range s.Calls() {
				if strings.Contains(call, "pool.dataset.delete") {
					deleted = true
				}
			}
			if deleted != tc.wantDeleted {
				t.Fatalf("namespace dataset deleted = %v, want %v (calls: %v)",
					deleted, tc.wantDeleted, s.Calls())
			}
		})
	}
}
