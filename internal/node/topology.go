package node

import (
	"github.com/piwi3910/truenas-csi/internal/driver"
)

// topologyPrefix namespaces every topology key this driver publishes. It is derived
// from the driver name, which is immutable once PersistentVolumes exist.
const topologyPrefix = driver.DriverName + "/"

// TopologyLabels renders the preflight result as CSI topology segments, one per
// capability, so the external-provisioner can steer a volume to a node that can
// actually mount it. Every known capability is emitted, present ones as "true" and
// absent ones as "false": a segment that vanished when tooling was missing would let
// a nodeAffinity match a node that cannot serve the volume.
//
// Label VALUES here are immutable once a node has registered — installing missing
// tooling on a node the driver already labelled "false" makes registration fail
// permanently. The full explanation, and the same trap for the per-backend
// reachability labels, is on Reachability.TopologyLabels in reachability.go; the
// operator-facing recovery is in docs/troubleshooting.md under "detected topology
// value collision".
func (p *Preflight) TopologyLabels() map[string]string {
	labels := make(map[string]string, len(capabilityOrder))
	for _, c := range capabilityOrder {
		v := "false"
		if p != nil && p.Found[c] {
			v = "true"
		}
		labels[topologyPrefix+string(c)] = v
	}
	return labels
}

// TopologyKey is the node label that advertises a capability. The controller
// uses it to express what a volume requires, so both halves cannot drift apart.
func TopologyKey(c Capability) string { return topologyPrefix + string(c) }
