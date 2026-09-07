package node

import (
	"context"
	"net"
	"testing"
	"time"
)

// closedAddr returns an address nothing is listening on: a listener is opened to
// borrow a free port and then closed, so the connection is refused rather than
// left hanging on a firewalled black hole.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// TestBackendReachabilityLabels is the property the scheduler depends on: a node
// publishes, per configured appliance, whether it can actually open a data
// connection to it. A node on a management VLAN with no route to nas2 must say
// so, or a pod using a nas2 volume can be scheduled there and never mount.
func TestBackendReachabilityLabels(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	r := ProbeReachability(context.Background(), map[string]string{
		"nas1": l.Addr().String(),
		"nas2": closedAddr(t),
	}, 2*time.Second)

	labels := r.TopologyLabels()
	for name, want := range map[string]string{"nas1": "true", "nas2": "false"} {
		key := BackendTopologyKey(name)
		got, ok := labels[key]
		if !ok {
			t.Fatalf("no label %q published; got %v", key, labels)
		}
		if got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if len(labels) != 2 {
		t.Errorf("published %d labels, want exactly one per configured backend: %v", len(labels), labels)
	}
}

// TestReachabilityLabelsAreAlwaysPublished pins that an unprobed or empty result
// still yields no half-truths: a missing label would let a nodeAffinity match a
// node that was never asked whether it can reach the appliance.
func TestReachabilityLabelsAreAlwaysPublished(t *testing.T) {
	var nilReach *Reachability
	if got := nilReach.TopologyLabels(); len(got) != 0 {
		t.Errorf("a nil probe published %v, want nothing", got)
	}

	r := ProbeReachability(context.Background(), map[string]string{"nas1": ""}, 50*time.Millisecond)
	if got := r.TopologyLabels()[BackendTopologyKey("nas1")]; got != "false" {
		t.Errorf("a backend with no data address reported %q, want %q", got, "false")
	}
}

// TestNodeInfoCarriesBackendReachability proves the probe reaches NodeGetInfo —
// a label computed and never published steers nothing.
func TestNodeInfoCarriesBackendReachability(t *testing.T) {
	n := NewNode("worker-21", &Preflight{Found: map[Capability]bool{CapNFS: true}}, nil)
	n.SetReachability(ProbeReachability(context.Background(),
		map[string]string{"nas1": closedAddr(t)}, time.Second))

	topo := n.GetInfo(context.Background()).AccessibleTopology
	if got, ok := topo[BackendTopologyKey("nas1")]; !ok || got != "false" {
		t.Fatalf("NodeGetInfo topology %v lacks %s=false", topo, BackendTopologyKey("nas1"))
	}
	if _, ok := topo[TopologyKey(CapNFS)]; !ok {
		t.Errorf("backend labels displaced the capability labels: %v", topo)
	}
}
