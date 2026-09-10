package csi

import (
	"context"
	"testing"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/node"
)

// createdTopology runs a CreateVolume and returns the single accessible
// topology segment map it published.
func createdTopology(t *testing.T, name string, params map[string]string) map[string]string {
	t.Helper()
	shared = newCounting()
	c, _ := ctlWith(t)
	resp, err := c.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name:               name,
		VolumeCapabilities: testCaps(),
		CapacityRange:      &csipb.CapacityRange{RequiredBytes: 1 << 20},
		Parameters:         params,
	})
	if err != nil {
		t.Fatal(err)
	}
	topo := resp.GetVolume().GetAccessibleTopology()
	if len(topo) != 1 {
		t.Fatalf("want exactly one topology term, got %d", len(topo))
	}
	return topo[0].GetSegments()
}

// TestTopologyRequiresBackendReachability is the scheduling constraint issue #6
// exists for: a volume that lives on nas1 must only be placed on a node that
// says it can reach nas1. Requiring only the tooling labels lets the scheduler
// pick a node with the right binaries and no route to the appliance.
func TestTopologyRequiresBackendReachability(t *testing.T) {
	segments := createdTopology(t, "pvc-topology", params())

	key := node.BackendTopologyKey("nas1")
	got, ok := segments[key]
	if !ok {
		t.Fatalf("CreateVolume topology %v does not require %s", segments, key)
	}
	if got != "true" {
		t.Fatalf("%s = %q, want %q", key, got, "true")
	}
}

// TestTopologyKeysMatchWhatNodesPublish catches the failure mode that cannot be
// seen from either side alone: a requirement no node can ever satisfy, which
// leaves every pod Pending with no error anywhere in the driver's logs. Every
// key CreateVolume requires must be a key some node actually publishes.
func TestTopologyKeysMatchWhatNodesPublish(t *testing.T) {
	// What a node with nas1 configured publishes at NodeGetInfo. The values do
	// not matter here, only the key spelling: a key nothing publishes can never
	// be matched, whatever its value.
	pf, err := node.Detect(context.Background(), t.TempDir(), func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	n := node.NewNode("worker-21", pf, nil)
	n.SetReachability(node.ProbeReachability(context.Background(),
		map[string]string{"nas1": "127.0.0.1:1"}, 200*time.Millisecond))
	published := n.GetInfo(context.Background()).AccessibleTopology

	for _, params := range []map[string]string{
		{"backend": "nas1", "protocol": "nfs"},
		{"backend": "nas1", "protocol": "iscsi"},
		{"backend": "nas1", "protocol": "iscsi", "fsType": "xfs"},
		{"backend": "nas1", "protocol": "iscsi", "multipath": "true"},
	} {
		required := requiredTopology(params["backend"], params["protocol"], params, nil)
		if len(required) != 1 {
			t.Fatalf("want exactly one topology term, got %d", len(required))
		}
		for key := range required[0].GetSegments() {
			if _, ok := published[key]; !ok {
				t.Errorf("CreateVolume for %v requires topology key %q, which no node publishes "+
					"(a node publishes %v) — every pod using this volume would stay Pending",
					params, key, published)
			}
		}
	}
}
