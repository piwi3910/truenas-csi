package main

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	ctrlruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/replication"
	replcontroller "github.com/piwi3910/truenas-csi/internal/replication/controller"
	"github.com/piwi3910/truenas-csi/internal/replication/v1alpha1"
)

// startReplication starts the StorageProtectionGroup reconciler, if an operator
// asked for it.
//
// It runs HERE, in the driver's controller pod, rather than in the operator,
// because the replication manager needs appliance clients and those credentials
// exist only in this pod. It is the same placement Dell uses for
// dell-csi-replicator, for the same reason.
//
// Two refusals, both leaving the cluster exactly as it is:
//
//   - no -replication-lease: replication is off. Reconciling a protection group
//     means promoting and demoting appliances, and two replicas doing that
//     concurrently would each act on a group the other had just moved.
//   - no in-cluster API access: refused rather than run half-configured.
//
// Refusing is safe. A StorageProtectionGroup simply keeps whatever status it
// has, which is the state it was already in.
func startReplication(ctx context.Context, o options, reg *backend.Registry) {
	if o.replicationLease == "" {
		return
	}

	cfg, err := config.GetConfig()
	if err != nil {
		slog.Error("replication disabled: no in-cluster API access",
			"error", obs.Redact(err.Error()))
		return
	}

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		slog.Error("replication disabled: could not register the StorageProtectionGroup types",
			"error", obs.Redact(err.Error()))
		return
	}

	// Leader election is controller-runtime's own here rather than obs.RunLeader:
	// the manager owns the caches and the reconcile loop, so it has to be the
	// thing that starts and stops with the lease.
	mgr, err := ctrlruntime.NewManager(cfg, manager.Options{
		Scheme: scheme,
		// The driver already serves its own metrics and health endpoints; a
		// second listener here would collide with them.
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		LeaderElection:          true,
		LeaderElectionID:        o.replicationLease,
		LeaderElectionNamespace: podNamespace(),
	})
	if err != nil {
		slog.Error("replication disabled: could not build the controller manager",
			"error", obs.Redact(err.Error()))
		return
	}

	r := &replcontroller.Reconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Manager: replication.NewManager(reg, nil),
	}
	if err := r.SetupWithManager(mgr); err != nil {
		slog.Error("replication disabled: could not register the reconciler",
			"error", obs.Redact(err.Error()))
		return
	}

	go func() {
		slog.Info("replication armed, waiting for the lease",
			"lease", o.replicationLease, "namespace", podNamespace())
		if err := mgr.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("the replication manager stopped; StorageProtectionGroups are no "+
				"longer reconciled by this replica", "error", obs.Redact(err.Error()))
		}
	}()
}

// podNamespace is the namespace the driver runs in, for the lease.
//
// The downward API supplies it; without it controller-runtime would fall back
// to reading the service-account namespace file, and a wrong guess would elect
// a leader in a namespace the driver has no RBAC for.
func podNamespace() string { return os.Getenv("POD_NAMESPACE") }
