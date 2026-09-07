// Package rollout decides which node plugin pods may be replaced right now.
//
// The DaemonSet's own RollingUpdate strategy knows nothing about storage. It
// will delete a plugin pod on a node that is halfway through staging a volume,
// and the CSI call that was in flight fails; worse, a node that has an iSCSI
// session and a mounted filesystem loses the process that knows how to unstage
// them. Ripping the node plugin out from under a mounted volume is how a
// storage upgrade takes workloads down.
//
// So the operator sets the DaemonSet to OnDelete and drives the rollout here:
// one node at a time, each node rolled only when it reports no volume mid-stage,
// and the next node not started until the previous node's plugin is Ready again.
package rollout

import (
	"fmt"
	"sort"
)

// NodeState is everything the rollout needs to know about one node.
type NodeState struct {
	// Name of the node.
	Name string
	// PodName is the plugin pod currently on the node, empty if there is none.
	PodName string
	// UpToDate is true when the pod was created from the desired pod template.
	UpToDate bool
	// Ready is true when the plugin pod reports Ready.
	Ready bool
	// Busy is true when a volume is mid-stage on this node: a pod is mounting
	// or unmounting one of this driver's volumes, or an attachment is still in
	// flight. A busy node is never rolled.
	Busy bool
	// BusyReason names what is keeping the node busy, for the status field. An
	// administrator watching a rollout sit still deserves to know it is waiting
	// on a real workload, not stuck.
	BusyReason string
}

// Plan is what the operator should do next.
type Plan struct {
	// Roll is the set of plugin pods to delete now. It holds at most one entry:
	// the rollout is deliberately serial.
	Roll []string
	// WaitingFor explains why nothing is being rolled, empty when Roll is not.
	WaitingFor string
	// Done is true when every node is up to date and ready.
	Done bool
	// Updated and Total describe progress for the status subresource.
	Updated int
	Total   int
}

// Next computes the plan.
//
// The rules, in order:
//
//  1. If a node was already rolled and its new plugin is not Ready yet, wait.
//     Rolling a second node now would leave two nodes without a plugin.
//  2. Otherwise take the first out-of-date node that is not busy and roll it.
//  3. If every out-of-date node is busy, wait and say which node and why.
//
// Nodes are considered in name order so that a rollout interrupted by an
// operator restart resumes deterministically instead of picking a new victim.
func Next(nodes []NodeState) Plan {
	ordered := make([]NodeState, len(nodes))
	copy(ordered, nodes)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	plan := Plan{Total: len(ordered)}
	for _, n := range ordered {
		if n.UpToDate && n.Ready {
			plan.Updated++
		}
	}

	// Rule 1: never overlap. An up-to-date pod that is not ready is a node
	// whose plugin is still starting; a node with no pod at all is a node whose
	// pod we just deleted and the DaemonSet has not recreated.
	for _, n := range ordered {
		if n.PodName == "" {
			plan.WaitingFor = fmt.Sprintf("node %s has no plugin pod yet", n.Name)
			return plan
		}
		if n.UpToDate && !n.Ready {
			plan.WaitingFor = fmt.Sprintf("waiting for node %s plugin to become ready", n.Name)
			return plan
		}
	}

	// Rule 2: roll exactly one clear, out-of-date node.
	var busy []NodeState
	for _, n := range ordered {
		if n.UpToDate {
			continue
		}
		if n.Busy {
			busy = append(busy, n)
			continue
		}
		plan.Roll = []string{n.PodName}
		return plan
	}

	// Rule 3: everything left is busy, or there is nothing left.
	if len(busy) > 0 {
		reason := busy[0].BusyReason
		if reason == "" {
			reason = "a volume is mid-stage"
		}
		plan.WaitingFor = fmt.Sprintf("node %s is not clear to roll: %s (%d node(s) waiting)",
			busy[0].Name, reason, len(busy))
		return plan
	}
	plan.Done = plan.Updated == plan.Total
	return plan
}

// CurrentNode returns the node whose pod the plan rolls, for the status field.
func CurrentNode(nodes []NodeState, plan Plan) string {
	if len(plan.Roll) == 0 {
		return ""
	}
	for _, n := range nodes {
		if n.PodName == plan.Roll[0] {
			return n.Name
		}
	}
	return ""
}
