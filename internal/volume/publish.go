package volume

import (
	"encoding/json"
	"fmt"
	"sort"
)

const (
	// PublishedProperty records which nodes hold an appliance-side access grant
	// on a volume, and what was granted to each.
	//
	// The ledger lives on the dataset rather than in controller memory or in a
	// Kubernetes object because the fence has to work when the node is gone:
	// ControllerUnpublishVolume for a node whose Node object has already been
	// deleted still has to know which addresses to strike off the share's host
	// list, and a controller that restarted remembers nothing. The appliance is
	// the only participant that survives both.
	PublishedProperty = "io.truenas.csi:published"

	// LUNProperty records the LUN id an iSCSI volume occupies on the shared
	// target, so a republish of the same volume lands on the same id.
	//
	// Without it the mapping is recreated at the lowest free id, which after a
	// delete elsewhere on the target can be an id another volume used to hold.
	// An initiator that cached the old mapping would then read one volume's
	// data at another volume's address.
	LUNProperty = "io.truenas.csi:lun"

	// NVMePortProperty records the nvmet port id an NVMe-oF volume is served
	// through.
	//
	// The port is chosen from StorageClass parameters — transport, address,
	// service id — and ControllerPublishVolume receives none of them. Without
	// this a controller that restarted could not tell an RDMA port from a TCP
	// one, and would rebind the volume to whichever it saw first.
	NVMePortProperty = "io.truenas.csi:nvmeport"

	// NetworksProperty records the operator's `networks` StorageClass value at
	// provisioning time.
	//
	// It cannot simply stay in the share's own `networks` field: on TrueNAS an
	// export is reachable by anything matching EITHER `hosts` OR `networks`, so
	// a network left there is a hole no per-node host list can close. Recording
	// it here keeps the operator's policy — only these networks may reach this
	// volume — as a filter applied to each node's addresses instead.
	NetworksProperty = "io.truenas.csi:networks"
)

// maxGrantsBytes bounds the encoded ledger. ZFS caps a user property value at
// 8 KiB; refusing at half of that leaves room for the encoding to grow without
// a volume ever reaching a state its own bookkeeping cannot be written back in.
const maxGrantsBytes = 4096

// ErrGrantsTooLarge means the ledger no longer fits in a ZFS user property.
var ErrGrantsTooLarge = fmt.Errorf("publish ledger exceeds %d bytes", maxGrantsBytes)

// Grants maps a CSI node id to the access this driver granted that node.
//
// The value is the list of node addresses written into the share's host access
// list. It is deliberately what was GRANTED rather than what the node currently
// has: a node whose addresses changed after publishing must still have its old
// addresses revoked, or the fence leaves them behind.
type Grants map[string][]string

// DecodeGrants parses a ledger. An empty value is an empty ledger, not an
// error: every volume provisioned before this driver kept one reads that way.
func DecodeGrants(s string) (Grants, error) {
	if s == "" {
		return Grants{}, nil
	}
	var g Grants
	if err := json.Unmarshal([]byte(s), &g); err != nil {
		return nil, fmt.Errorf("decoding the publish ledger %q: %w", s, err)
	}
	if g == nil {
		g = Grants{}
	}
	return g, nil
}

// Encode renders the ledger for storage in a ZFS user property.
func (g Grants) Encode() (string, error) {
	if len(g) == 0 {
		// An empty object rather than "" so a cleared ledger is distinguishable
		// from a volume that never had one, in `zfs get` output as well as here.
		return "{}", nil
	}
	b, err := json.Marshal(g)
	if err != nil {
		return "", fmt.Errorf("encoding the publish ledger: %w", err)
	}
	if len(b) > maxGrantsBytes {
		return "", fmt.Errorf("%w: %d nodes", ErrGrantsTooLarge, len(g))
	}
	return string(b), nil
}

// Nodes lists the node ids holding a grant, in a stable order.
func (g Grants) Nodes() []string {
	out := make([]string, 0, len(g))
	for id := range g {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Others lists the node ids holding a grant apart from nodeID.
//
// This is what makes a single-node access mode enforceable: the appliance has
// no notion of which node a shared iSCSI target's LUN belongs to, so the second
// publisher of a SINGLE_NODE volume can only be recognised here.
func (g Grants) Others(nodeID string) []string {
	out := make([]string, 0, len(g))
	for id := range g {
		if id != nodeID {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
