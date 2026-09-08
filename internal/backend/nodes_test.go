package backend

import (
	"context"
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestKubeNodeResolverReadsAddressesAndInitiatorNames: ControllerPublishVolume
// receives only a node id, and an NFS or SMB grant is written in addresses, so
// everything else has to come off the Node object.
func TestKubeNodeResolverReadsAddressesAndInitiatorNames(t *testing.T) {
	cases := []struct {
		name      string
		node      *corev1.Node
		wantAddrs []string
		wantIQN   string
	}{
		{
			name: "internal and external IPs, in order",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
				Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
					{Type: corev1.NodeExternalIP, Address: "203.0.113.5"},
					{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
				}},
			},
			wantAddrs: []string{"10.0.0.1", "203.0.113.5"},
		},
		{
			// A host list built from a name is only as reliable as the
			// appliance's own resolver, so names are not addresses.
			name: "hostnames are not addresses",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
				Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
					{Type: corev1.NodeHostName, Address: "worker-1.example.com"},
				}},
			},
			wantAddrs: []string{"10.0.0.1"},
		},
		{
			name: "duplicates collapse",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
				Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
					{Type: corev1.NodeExternalIP, Address: "10.0.0.1"},
				}},
			},
			wantAddrs: []string{"10.0.0.1"},
		},
		{
			name: "initiator name from its annotation",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "worker-1",
					Annotations: map[string]string{AnnotationIQN: "iqn.1993-08.org.debian:01:abc"},
				},
				Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
				}},
			},
			wantAddrs: []string{"10.0.0.1"},
			wantIQN:   "iqn.1993-08.org.debian:01:abc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newKubeNodeResolver(fake.NewSimpleClientset(tc.node))
			got, err := r.Resolve(context.Background(), "worker-1")
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if !slices.Equal(got.Addrs, tc.wantAddrs) {
				t.Errorf("addresses = %v, want %v", got.Addrs, tc.wantAddrs)
			}
			if got.IQN != tc.wantIQN {
				t.Errorf("IQN = %q, want %q", got.IQN, tc.wantIQN)
			}
			if got.ID != "worker-1" {
				t.Errorf("ID = %q, want worker-1", got.ID)
			}
		})
	}
}

// TestKubeNodeResolverReportsAnAbsentNode: CSI requires publishing to an
// unknown node to be NotFound, and the caller can only produce that code if the
// resolver distinguishes "no such node" from every other failure.
func TestKubeNodeResolverReportsAnAbsentNode(t *testing.T) {
	r := newKubeNodeResolver(fake.NewSimpleClientset())
	if _, err := r.Resolve(context.Background(), "ghost"); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("want ErrNodeNotFound, got %v", err)
	}
	if _, err := r.Resolve(context.Background(), ""); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("an empty node id names nothing, got %v", err)
	}
}

// TestLocalNodeResolverKnowsOnlyItself: outside Kubernetes the only node the
// driver can honestly claim to know is the one it was configured as. A resolver
// that accepted any id would grant access to addresses belonging to some other
// machine.
func TestLocalNodeResolverKnowsOnlyItself(t *testing.T) {
	r := NewLocalNodeResolver("worker-21")
	got, err := r.Resolve(context.Background(), "worker-21")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ID != "worker-21" {
		t.Errorf("ID = %q, want worker-21", got.ID)
	}
	if len(got.Addrs) == 0 {
		t.Error("a grant of no addresses is not a grant: the local resolver must always answer with one")
	}
	if _, err := r.Resolve(context.Background(), "worker-22"); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("want ErrNodeNotFound for another node, got %v", err)
	}
}
