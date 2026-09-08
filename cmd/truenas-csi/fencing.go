package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/fencing"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/podmon"
)

// startFencing starts the pod fencing controller, if an operator asked for it.
//
// Three refusals are deliberate, and each one leaves the cluster in the state it
// is already in rather than force-deleting anything:
//
//   - no -fencing-label: fencing is off. Force-deleting pods is destructive and
//     nobody gets it by default.
//   - no -fencing-lease: fencing is refused. Two controller replicas sweeping
//     the same pod would each decide independently that it is safe to fence and
//     then race their revokes; one replica may fence, and a lease is the only
//     thing that makes that true.
//   - no in-cluster API access, or the election could not start: fencing is
//     refused for the same reason.
//
// Refusing is safe. The pods stay stuck, which is the state they were in
// before, and an operator sees the log line saying why.
func startFencing(ctx context.Context, o options, reg *backend.Registry,
	nodes backend.NodeResolver, conn *podmon.Connectivity) {
	if o.fencingLabel == "" {
		return
	}
	key, value, ok := strings.Cut(o.fencingLabel, "=")
	if !ok || key == "" || value == "" {
		slog.Error("pod fencing disabled: -fencing-label must be key=value", "value", o.fencingLabel)
		return
	}
	if o.fencingLease == "" {
		slog.Error("pod fencing disabled: -fencing-lease is required. Fencing force-deletes pods " +
			"after revoking appliance access, and two replicas doing that concurrently would each " +
			"revoke access the other had just checked")
		return
	}
	// Defence in depth. Today this cannot trigger -- the node resolver and the
	// lease below are both built from the same in-cluster config, so if one
	// works the other does -- but fencing revokes a failed node's storage
	// access and force-deletes its pods, and it must not depend on two
	// unrelated code paths happening to fail together. A resolver that cannot
	// see the cluster's Node objects cannot describe the node being fenced.
	if !backend.ClusterScoped(nodes) {
		slog.Error("pod fencing disabled: the node resolver cannot read the cluster's Node objects, " +
			"so it cannot describe the node being fenced. This driver is not running with in-cluster " +
			"credentials")
		return
	}

	leader, err := obs.InClusterLeaderConfig(o.fencingLease)
	if err != nil {
		slog.Error("pod fencing disabled: no in-cluster API access for the fencing lease",
			"error", obs.Redact(err.Error()))
		return
	}

	events, shutdown := fencing.NewEventRecorder(leader.Client, "")
	ctrl := fencing.New(leader.Client, conn, fencing.NewRegistryFencer(reg, nodes), events)
	ctrl.LabelKey, ctrl.LabelValue = key, value
	if o.fencingInterval > 0 {
		ctrl.Interval = o.fencingInterval
	}

	go func() {
		defer shutdown()
		slog.Info("pod fencing armed, waiting for the lease",
			"lease", o.fencingLease, "identity", leader.Identity, "label", o.fencingLabel)
		err := obs.RunLeader(ctx, leader, ctrl.Run, func() {
			slog.Info("lost the fencing lease; this replica no longer fences anything",
				"lease", o.fencingLease)
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("fencing leader election stopped; nothing will be fenced by this replica",
				"error", obs.Redact(err.Error()))
		}
	}()
}
