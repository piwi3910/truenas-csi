package backend

import (
	"context"
	"errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
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

// TestResolveRetriesAFailedCacheSync pins the fix for a transient failure that
// turned into a permanent one.
//
// The sync used to run inside a sync.Once whose error was assigned to a
// variable declared per call. Once the first attempt had run, later calls
// skipped the closure entirely, saw a nil error, and queried a lister whose
// cache had never populated -- so every node in the cluster came back NotFound,
// for ever, until the process was restarted. A control-plane blip during the
// first ControllerPublishVolume was enough, and the error told the operator
// their node did not exist.
func TestResolveRetriesAFailedCacheSync(t *testing.T) {
	var attempts int
	r := &KubeNodeResolver{
		lister: stubNodeLister{node: &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.10.101"},
			}},
		}},
		start: func() {},
	}
	r.waitForSync = func(context.Context) error {
		attempts++
		if attempts == 1 {
			return errors.New("timed out waiting for the node cache to sync")
		}
		return nil
	}

	if _, err := r.Resolve(context.Background(), "worker-1"); err == nil {
		t.Fatal("the first Resolve must report the sync failure")
	}
	ref, err := r.Resolve(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("the second Resolve must retry the sync and succeed, got: %v", err)
	}
	if ref.ID != "worker-1" {
		t.Errorf("resolved %q, want worker-1", ref.ID)
	}
	if attempts != 2 {
		t.Errorf("waitForSync ran %d times, want 2: it must be checked on every call", attempts)
	}
}

// TestResolveStartsTheInformerOnce is the other half: the shared informer must
// not be started again per request.
func TestResolveStartsTheInformerOnce(t *testing.T) {
	var starts int
	r := &KubeNodeResolver{
		lister:      stubNodeLister{node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}},
		start:       func() { starts++ },
		waitForSync: func(context.Context) error { return nil },
	}
	for i := 0; i < 3; i++ {
		if _, err := r.Resolve(context.Background(), "worker-1"); err != nil {
			t.Fatal(err)
		}
	}
	if starts != 1 {
		t.Errorf("the informer was started %d times, want 1", starts)
	}
}

// stubNodeLister is the smallest NodeLister that answers Get.
type stubNodeLister struct{ node *corev1.Node }

func (s stubNodeLister) Get(name string) (*corev1.Node, error) {
	if s.node != nil && s.node.Name == name {
		return s.node, nil
	}
	return nil, apierrors.NewNotFound(corev1.Resource("nodes"), name)
}

func (s stubNodeLister) List(labels.Selector) ([]*corev1.Node, error) {
	if s.node == nil {
		return nil, nil
	}
	return []*corev1.Node{s.node}, nil
}
