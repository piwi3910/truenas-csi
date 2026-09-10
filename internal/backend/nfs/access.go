package nfs

import (
	"context"
	"net"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/piwi3910/truenas-csi/internal/truenas"
)

// DenyHost is the host entry that keeps a driver-managed export from ever
// collapsing into an export to everyone.
//
// On TrueNAS an NFS share is unrestricted when `hosts` AND `networks` are BOTH
// empty — the appliance's own schema says so ("If empty, all IP's/hostnames are
// allowed"), and it was confirmed on live hardware where two production shares
// with both lists empty are exported to the world. Revoking the last node's
// address therefore does not fence the volume: it flings it open. So the
// driver's export always carries this one entry that no client can ever
// present. 192.0.2.1 is from RFC 5737 TEST-NET-1, reserved for documentation
// and never routed, so it matches nothing while keeping `hosts` non-empty.
//
// Nothing may remove it. The invariant is: a driver-managed export's `hosts`
// list is never empty, so the "both empty" state is unreachable.
const DenyHost = "192.0.2.1"

// nfsShare is sharing.nfs as the appliance actually returns it.
//
// internal/truenas models an export without its access lists, and those lists
// are the whole fence, so the access path reads and writes the raw shape here
// rather than through the typed helper.
type nfsShare struct {
	ID       int      `json:"id"`
	Path     string   `json:"path"`
	Hosts    []string `json:"hosts"`
	Networks []string `json:"networks"`

	// Enabled is the export's own switch. A POINTER because absent must not
	// read as disabled: middleware that stopped reporting the field would
	// otherwise make every share look dead and stop provisioning outright.
	Enabled *bool `json:"enabled"`
}

// serving reports whether the appliance is actually exporting this share. A
// share that does not report the field is assumed to be.
func (s *nfsShare) serving() bool { return s == nil || s.Enabled == nil || *s.Enabled }

func (b *Backend) shareByPath(ctx context.Context, path string) (*nfsShare, error) {
	var out []nfsShare
	err := b.c.CallJSON(ctx, &out, "sharing.nfs.query",
		[]any{[]any{"path", "=", path}}, map[string]any{})
	if err != nil {
		if truenas.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// createShare exports a path with an access list that grants exactly hosts.
//
// The export is created with its access list already in place rather than
// created open and narrowed afterwards: a share published with both lists empty
// is world-readable for as long as the second call takes, and a controller that
// dies in between leaves it that way.
func (b *Backend) createShare(ctx context.Context, path string, hosts []string, p params) (*nfsShare, error) {
	payload := map[string]any{
		"path":    path,
		"comment": "truenas-csi",
		"hosts":   withDenyHost(hosts),
		// networks is deliberately emptied, not filled from the StorageClass:
		// the appliance ORs the two lists, so any network left here is a hole
		// no per-node host grant can close. The operator's list survives as a
		// policy filter — see grantHosts.
		"networks": []string{},
	}
	if p.maproot != "" {
		payload["maproot_user"] = p.maproot
		payload["maproot_group"] = p.maproot
	}
	var out nfsShare
	if err := b.c.CallJSON(ctx, &out, "sharing.nfs.create", payload); err != nil {
		return nil, err
	}
	return &out, nil
}

// setHosts rewrites an export's access list.
//
// Both fields are written on every call. Sending only `hosts` would leave a
// `networks` entry an operator added by hand in place, and a single network
// entry defeats every per-node grant on the share.
func (b *Backend) setHosts(ctx context.Context, shareID int, hosts []string) error {
	return b.c.CallJSON(ctx, nil, "sharing.nfs.update", shareID, map[string]any{
		"hosts":    withDenyHost(hosts),
		"networks": []string{},
	})
}

// withDenyHost returns the access list the driver is willing to write: the
// caller's hosts, deduplicated and ordered, always including DenyHost.
//
// This is the single choke point for the empty-list trap. Every write of an
// export's host list goes through it, so no future caller can produce the
// unrestricted state by passing an empty slice.
func withDenyHost(hosts []string) []string {
	set := map[string]bool{DenyHost: true}
	for _, h := range hosts {
		if h = strings.TrimSpace(h); h != "" {
			set[h] = true
		}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// withoutHosts removes a node's addresses from an access list, keeping the
// result in the form withDenyHost guarantees.
func withoutHosts(current, remove []string) []string {
	drop := map[string]bool{}
	for _, r := range remove {
		drop[strings.TrimSpace(r)] = true
	}
	kept := make([]string, 0, len(current))
	for _, h := range current {
		if !drop[h] {
			kept = append(kept, h)
		}
	}
	return withDenyHost(kept)
}

// grantHosts filters a node's addresses through the operator's network policy.
//
// The `networks` StorageClass parameter used to be written straight onto the
// export, where it OR'd with the host list and made the fence ineffective for
// anything inside it. It is applied here instead: an address outside every
// configured network is not granted at all, which is a strictly tighter reading
// of the same operator intent, and one a per-node fence can actually honour.
func grantHosts(addrs []string, networks []string) ([]string, error) {
	if len(networks) == 0 {
		return addrs, nil
	}
	nets := make([]*net.IPNet, 0, len(networks))
	for _, n := range networks {
		_, ipnet, err := net.ParseCIDR(strings.TrimSpace(n))
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"%s=%q is not CIDR notation: %v", ParamNetworks, n, err)
		}
		nets = append(nets, ipnet)
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ip := net.ParseIP(strings.TrimSpace(a))
		if ip == nil {
			continue
		}
		for _, n := range nets {
			if n.Contains(ip) {
				out = append(out, a)
				break
			}
		}
	}
	return out, nil
}

// splitNetworks parses the recorded `networks` policy back into a list.
func splitNetworks(v string) []string {
	var out []string
	for _, n := range strings.Split(v, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}
