package backend

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"

	"github.com/piwi3910/truenas-csi/internal/driver"
	"github.com/piwi3910/truenas-csi/internal/obs"
)

// Node annotations a node may use to advertise its initiator names.
//
// Nothing in this driver writes them today — the iSCSI backend fences through
// its shared target's LUN mapping and NVMe-oF through its port binding, so
// neither needs one. They are read rather than required so that a node-side
// publisher, or an operator with a static initiator layout, can make a
// per-initiator grant possible without a new resolution mechanism.
const (
	// AnnotationIQN carries the node's iSCSI initiator name.
	AnnotationIQN = driver.DriverName + "/iqn"
	// AnnotationNQN carries the node's NVMe host NQN.
	AnnotationNQN = driver.DriverName + "/nqn"
)

// NodeResolver turns the CSI node id ControllerPublishVolume receives into the
// facts an appliance-side access grant is written in terms of.
type NodeResolver interface {
	// Resolve returns the node, or ErrNodeNotFound when the cluster has no such
	// node. CSI requires publishing to an unknown node to be NotFound, so the
	// distinction between "absent" and "could not ask" has to survive here.
	Resolve(ctx context.Context, nodeID string) (NodeRef, error)
}

// NewNodeResolver returns the resolver appropriate to where the driver runs.
//
// In a cluster that is the API server's Node objects. Outside one — the
// conformance suite, a single-host deployment — there is no Node object to
// read, and the only node the driver can honestly claim to know is the one it
// was configured as; see LocalNodeResolver.
func NewNodeResolver(selfNodeID string) NodeResolver {
	r, err := NewKubeNodeResolver()
	if err != nil {
		obs.Logger(context.Background()).Info(
			"no Kubernetes API available for node resolution; per-node grants will only be issued for this driver's own node id",
			"node_id", selfNodeID, "reason", err)
		return NewLocalNodeResolver(selfNodeID)
	}
	return r
}

// KubeNodeResolver answers from a cached watch of the cluster's Node objects.
//
// A cache rather than a GET per publish: ControllerPublishVolume is on the
// critical path of every pod start, and a cluster-wide rolling restart would
// otherwise turn into a burst of API reads proportional to the number of pods.
type KubeNodeResolver struct {
	lister listersv1.NodeLister

	// synced is closed once the informer's cache is populated. Answering
	// NotFound from an empty cache would fence a healthy node.
	syncOnce sync.Once
	synced   func(context.Context) error
}

// NewKubeNodeResolver builds a resolver from the pod's in-cluster credentials.
func NewKubeNodeResolver() (*KubeNodeResolver, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return newKubeNodeResolver(cs), nil
}

// resyncInterval is long deliberately: node addresses change rarely, and the
// watch — not the resync — is what makes a change visible promptly.
const resyncInterval = 10 * time.Minute

func newKubeNodeResolver(cs kubernetes.Interface) *KubeNodeResolver {
	factory := informers.NewSharedInformerFactory(cs, resyncInterval)
	nodes := factory.Core().V1().Nodes()
	lister := nodes.Lister()
	informer := nodes.Informer()

	r := &KubeNodeResolver{lister: lister}
	r.synced = func(ctx context.Context) error {
		// The informer outlives every request, so it is started against the
		// process rather than the caller's context: a publish that is cancelled
		// must not tear the shared cache down under the next one.
		stop := make(chan struct{})
		factory.Start(stop)
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if !cacheSync(waitCtx, informer.HasSynced) {
			return fmt.Errorf("timed out waiting for the node cache to sync")
		}
		return nil
	}
	return r
}

// cacheSync polls a HasSynced func until it reports true or ctx ends.
func cacheSync(ctx context.Context, hasSynced func() bool) bool {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		if hasSynced() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
	}
}

// Resolve implements NodeResolver.
func (r *KubeNodeResolver) Resolve(ctx context.Context, nodeID string) (NodeRef, error) {
	if nodeID == "" {
		return NodeRef{}, fmt.Errorf("%w: empty node id", ErrNodeNotFound)
	}
	var syncErr error
	r.syncOnce.Do(func() { syncErr = r.synced(ctx) })
	if syncErr != nil {
		// Deliberately NOT ErrNodeNotFound: a cache that never synced says
		// nothing about whether the node exists, and reporting NotFound would
		// let a control-plane outage look like a deleted node.
		return NodeRef{}, syncErr
	}
	n, err := r.lister.Get(nodeID)
	if err != nil {
		return NodeRef{}, fmt.Errorf("%w: %s", ErrNodeNotFound, nodeID)
	}
	return nodeRefFrom(n), nil
}

// nodeRefFrom reads the addresses and initiator names off a Node object.
func nodeRefFrom(n *corev1.Node) NodeRef {
	ref := NodeRef{
		ID:  n.Name,
		IQN: n.Annotations[AnnotationIQN],
		NQN: n.Annotations[AnnotationNQN],
	}
	seen := map[string]bool{}
	for _, a := range n.Status.Addresses {
		// Only routable addresses: Hostname and InternalDNS are names, and an
		// export host list built from a name is only as reliable as the
		// appliance's own resolver.
		switch a.Type {
		case corev1.NodeInternalIP, corev1.NodeExternalIP:
		default:
			continue
		}
		if a.Address == "" || seen[a.Address] {
			continue
		}
		seen[a.Address] = true
		ref.Addrs = append(ref.Addrs, a.Address)
	}
	sort.Strings(ref.Addrs)
	return ref
}

// LocalNodeResolver knows exactly one node: the one this process was
// configured as, addressed by this host's own interfaces.
//
// It exists so the driver is testable and usable outside Kubernetes. It is
// deliberately strict about the id — any other node id is ErrNodeNotFound —
// because a resolver that accepted every id would grant access to addresses
// that belong to some other machine.
type LocalNodeResolver struct {
	nodeID string
	addrs  func() []string
}

// NewLocalNodeResolver builds a resolver for a single known node id.
func NewLocalNodeResolver(nodeID string) *LocalNodeResolver {
	return &LocalNodeResolver{nodeID: nodeID, addrs: localAddresses}
}

// Resolve implements NodeResolver.
func (r *LocalNodeResolver) Resolve(_ context.Context, nodeID string) (NodeRef, error) {
	if nodeID == "" || nodeID != r.nodeID {
		return NodeRef{}, fmt.Errorf("%w: %s", ErrNodeNotFound, nodeID)
	}
	return NodeRef{ID: nodeID, Addrs: r.addrs()}, nil
}

// localAddresses lists this host's usable unicast addresses.
//
// Loopback is the last resort rather than the first: on a host with a real
// address, granting 127.0.0.1 would produce an export the node cannot reach.
// On a host with nothing else it is the only honest answer, and an empty list
// is not an option — a grant of no addresses is not a grant.
func localAddresses() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return []string{"127.0.0.1"}
	}
	var routable, loopback []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP == nil || ipnet.IP.IsLinkLocalUnicast() {
				continue
			}
			if ipnet.IP.IsLoopback() {
				loopback = append(loopback, ipnet.IP.String())
				continue
			}
			routable = append(routable, ipnet.IP.String())
		}
	}
	if len(routable) > 0 {
		sort.Strings(routable)
		return routable
	}
	if len(loopback) > 0 {
		sort.Strings(loopback)
		return loopback
	}
	return []string{"127.0.0.1"}
}
