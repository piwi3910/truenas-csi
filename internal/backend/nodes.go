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
		// Warn, not Info. In a pod this cannot happen: rest.InClusterConfig
		// reads only the service-account token and the KUBERNETES_SERVICE_*
		// environment, and never contacts the API server, so an API outage does
		// not land here -- only running outside Kubernetes does. If this line
		// appears in a cluster something is wrong with the deployment, and the
		// consequence is not cosmetic: every publish for a node other than this
		// process's own is refused as NotFound, so pods do not start.
		obs.Logger(context.Background()).Warn(
			"no Kubernetes API available for node resolution: per-node grants can only be issued "+
				"for this process's own node id, every other publish will be refused as NotFound, "+
				"and pod fencing will not run",
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

	// startOnce starts the shared informer exactly once; waitForSync then
	// blocks until its cache is populated, on EVERY call.
	//
	// Answering NotFound from an empty cache would tell the caller a healthy
	// node does not exist. That is why the wait cannot be behind the same Once
	// as the start: a sync that failed on the first call -- a control-plane
	// blip, a slow apiserver, the 30s bound -- would then be skipped by every
	// later call, which would query an unsynced lister and get NotFound for
	// every node in the cluster until the process was restarted.
	startOnce   sync.Once
	start       func()
	waitForSync func(context.Context) error
}

// NewKubeNodeResolverFor builds a resolver over an existing clientset.
//
// It exists for callers that already hold credentials the in-cluster path
// cannot produce -- notably the node-side end-to-end suite, which runs outside
// the cluster against a kubeconfig and must resolve the REAL addresses of the
// node it mounts from. Without it such a test can only reach LocalNodeResolver,
// which answers with the test machine's own interfaces, so a publish would
// grant the laptop and the cluster node would still be fenced out.
func NewKubeNodeResolverFor(cs kubernetes.Interface) *KubeNodeResolver {
	return newKubeNodeResolver(cs)
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
	r.start = func() {
		// The informer outlives every request, so it is started against the
		// process rather than the caller's context: a publish that is cancelled
		// must not tear the shared cache down under the next one.
		factory.Start(make(chan struct{}))
	}
	r.waitForSync = func(ctx context.Context) error {
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
	r.startOnce.Do(r.start)
	// Checked on every call, not once: see the comment on waitForSync.
	if syncErr := r.waitForSync(ctx); syncErr != nil {
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

// ClusterScoped reports whether this resolver can answer for nodes other than
// the process's own -- that is, whether it reads the cluster's Node objects.
//
// It exists so a caller whose correctness depends on resolving OTHER nodes can
// refuse to run rather than discover the limitation one publish at a time. Pod
// fencing is exactly such a caller: it revokes a failed node's access, and a
// resolver that only knows this process's own host would either refuse (safe but
// silent) or, if the ids happened to match, grant the wrong machine's addresses.
func (r *KubeNodeResolver) ClusterScoped() bool { return true }

// ClusterScoped is false: this resolver knows exactly one node, its own.
func (r *LocalNodeResolver) ClusterScoped() bool { return false }

// ClusterScoped reports whether a resolver can answer for nodes other than the
// process's own. A resolver that does not say is assumed not to, because the
// answer gates a destructive operation.
func ClusterScoped(r NodeResolver) bool {
	cs, ok := r.(interface{ ClusterScoped() bool })
	return ok && cs.ClusterScoped()
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
