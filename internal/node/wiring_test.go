package node

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// TestGetInfoAdvertisesABackendSegment fails if NodeGetInfo stops reporting a
// reachability segment for a configured backend.
//
// This is deliberately an assertion about the ENTRY POINT rather than about
// Reachability.TopologyLabels, which was always correct and always tested.
// ProbeReachability and SetReachability were simply never called from
// cmd/truenas-csi, so nodes advertised capability labels and no backend label
// at all -- while the controller required one for every volume. The result was
// that every dynamically provisioned volume bound and then failed to schedule
// with "node(s) didn't match PersistentVolume's node affinity", on a cluster
// whose unit tests were entirely green.
//
// A test that exercises a function proves the function works. Only a test that
// goes through the entry point proves it is reached.
func TestGetInfoAdvertisesABackendSegment(t *testing.T) {
	// A listener stands in for an appliance so the probe has something to find.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lis.Close() }()
	addr := lis.Addr().String()

	n := NewNode("node-a", &Preflight{}, nil)
	n.SetReachability(ProbeReachability(context.Background(),
		map[string]string{"nas1": addr}, 2*time.Second))

	info := n.GetInfo(context.Background())
	key := BackendTopologyKey("nas1")
	got, ok := info.AccessibleTopology[key]
	if !ok {
		t.Fatalf("NodeGetInfo advertises no %q segment.\n"+
			"The controller REQUIRES this segment for every volume on that backend, so a "+
			"node without it is excluded from scheduling and every PVC stays Pending after "+
			"binding. Advertised: %v", key, info.AccessibleTopology)
	}
	if got != "true" {
		t.Errorf("%s = %q, want \"true\": the probe reached the listener", key, got)
	}
}

// TestGetInfoReportsAnUnreachableBackendAsFalse pins the other direction: a
// backend the node cannot reach must be advertised as false, not omitted.
// Omitting it would let the scheduler treat the node as unconstrained.
func TestGetInfoReportsAnUnreachableBackendAsFalse(t *testing.T) {
	n := NewNode("node-a", &Preflight{}, nil)
	// 198.51.100.0/24 is TEST-NET-2: reserved, and routed nowhere.
	n.SetReachability(ProbeReachability(context.Background(),
		map[string]string{"gone": "198.51.100.7:3260"}, 300*time.Millisecond))

	info := n.GetInfo(context.Background())
	key := BackendTopologyKey("gone")
	got, ok := info.AccessibleTopology[key]
	if !ok {
		t.Fatalf("an unreachable backend must still be advertised, as false: %v",
			info.AccessibleTopology)
	}
	if got != "false" {
		t.Errorf("%s = %q, want \"false\"", key, got)
	}
	if !strings.HasPrefix(key, "csi.truenas.watteel.com/") {
		t.Errorf("unexpected label namespace: %s", key)
	}
}
