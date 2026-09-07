package node

// This file answers a question the capability preflight cannot: not what tooling
// this node has, but which appliances it can actually reach. With several
// backends configured, a node can have every binary and module a volume needs and
// still have no route to the NAS the volume lives on — a different VLAN, a
// firewall, a storage network the node is not attached to. Without a per-backend
// label the scheduler cannot tell those nodes apart, and a pod lands somewhere it
// can never mount.

import (
	"context"
	"net"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/obs"
)

// ProbeTimeout bounds one dial. It is deliberately short: the probe runs on the
// node's startup path, and an unreachable appliance must delay registration by
// seconds, not by a TCP connect timeout per backend.
const ProbeTimeout = 2 * time.Second

// DataPorts are the ports probed to decide reachability: NFS and iSCSI. Either
// one answering means the node has a route to the appliance's data path.
//
// The probe is a bounded TCP dial, never ICMP: ping is filtered on most storage
// networks and says nothing about whether the data port is actually served, so a
// node that pings but cannot mount would be labelled reachable.
var DataPorts = []string{"2049", "3260"}

// Reachability is which appliances this node could reach at startup.
type Reachability struct {
	// Reachable has an entry for every configured backend, so a missing key means
	// the backend was never probed rather than "unreachable".
	Reachable map[string]bool
}

// BackendDataAddresses maps each configured backend to the address to probe. The
// appliance's API endpoint host is used: it is the address the operator gave for
// this appliance, and the NFS/iSCSI data path is served from the same host unless
// a StorageClass overrides the server, which is per-volume policy rather than
// per-node connectivity.
func BackendDataAddresses(cfg *config.Config) map[string]string {
	if cfg == nil {
		return nil
	}
	out := make(map[string]string, len(cfg.Backends))
	for name, b := range cfg.Backends {
		out[name] = dataHost(b.Endpoint)
	}
	return out
}

// dataHost extracts the host from an appliance endpoint, dropping the API port:
// the data ports are probed, not the middleware's.
func dataHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Hostname()
}

// ProbeReachability dials each appliance's data address and reports which ones
// answered. It runs at node start, alongside Detect, and its result becomes part
// of NodeGetInfo's accessible topology.
//
// An address carrying an explicit port is dialled as given; otherwise every port
// in DataPorts is tried and the first to answer wins. Probes run concurrently, so
// several dead appliances cost one timeout, not one each.
func ProbeReachability(ctx context.Context, addresses map[string]string, timeout time.Duration) *Reachability {
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	r := &Reachability{Reachable: make(map[string]bool, len(addresses))}

	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, addr := range addresses {
		wg.Add(1)
		go func(name, addr string) {
			defer wg.Done()
			ok := dialAny(ctx, addr, timeout)
			mu.Lock()
			r.Reachable[name] = ok
			mu.Unlock()
		}(name, addr)
	}
	wg.Wait()

	names := make([]string, 0, len(r.Reachable))
	for n := range r.Reachable {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		obs.Logger(ctx).Info("backend reachability probed",
			"backend", n, "address", addresses[n], "reachable", r.Reachable[n])
	}
	return r
}

// dialAny reports whether a TCP connection to addr succeeds on any data port.
func dialAny(ctx context.Context, addr string, timeout time.Duration) bool {
	if addr == "" {
		return false
	}
	// An address that already names a port is dialled verbatim.
	if host, port, err := net.SplitHostPort(addr); err == nil && host != "" && port != "" {
		return dialOne(ctx, addr, timeout)
	}
	for _, p := range DataPorts {
		if dialOne(ctx, net.JoinHostPort(addr, p), timeout) {
			return true
		}
	}
	return false
}

func dialOne(ctx context.Context, addr string, timeout time.Duration) bool {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// TopologyLabels renders the probe as CSI topology segments, one per configured
// backend, e.g. csi.truenas.watteel.com/backend-nas1 = "true".
//
// IMMUTABILITY — READ BEFORE CHANGING THIS.
//
// Kubernetes treats topology label VALUES as immutable. Once a node carries
// backend-nas1="false", this driver can never report "true" for it again: the
// node-driver-registrar fails permanently with
//
//	detected topology value collision: driver reported
//	"csi.truenas.watteel.com/backend-nas1":"true" but existing label is
//	"csi.truenas.watteel.com/backend-nas1":"false"
//
// and the node plugin never registers, so every volume on that node stops
// mounting — not just the ones on nas1. Fixing a node's route to an appliance
// therefore requires clearing the driver's labels from that node by hand before
// the plugin restarts; see docs/troubleshooting.md, "detected topology value
// collision". The same trap applies to renaming a backend: the old label stays on
// the node forever.
//
// Values must stay exactly "true"/"false" and the key spelling must stay in step
// with what the controller requires — TestTopologyKeysMatchWhatNodesPublish is
// what keeps the two halves from drifting into a requirement no node can satisfy.
func (r *Reachability) TopologyLabels() map[string]string {
	if r == nil {
		return map[string]string{}
	}
	labels := make(map[string]string, len(r.Reachable))
	for name, ok := range r.Reachable {
		v := "false"
		if ok {
			v = "true"
		}
		labels[BackendTopologyKey(name)] = v
	}
	return labels
}

// BackendTopologyKey is the node label advertising that this node can reach one
// appliance. The controller uses it to express what a volume requires, so both
// halves cannot drift apart.
func BackendTopologyKey(name string) string { return topologyPrefix + "backend-" + name }
