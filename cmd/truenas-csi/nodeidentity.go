package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	kconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/node"
	"github.com/piwi3910/truenas-csi/internal/obs"
)

// publishNodeIdentityTimeout bounds the whole thing. It runs on the node
// plugin's startup path and must not delay registration.
const publishNodeIdentityTimeout = 30 * time.Second

// publishNodeIdentity annotates this node with its own NVMe host NQN and iSCSI
// initiator name, so the controller can restrict a volume to the nodes that
// should hold it.
//
// WHY THIS IS OPT-IN. The controller reads these annotations
// (backend.AnnotationNQN / AnnotationIQN) and uses them to create an NVMe
// subsystem closed to one initiator, and to populate an iSCSI initiator group.
// Without them a subsystem is created OPEN -- any initiator that can reach the
// portal may use it -- which is deliberate, because a subsystem closed with an
// empty ACL admits nobody and would take the backend silently offline.
//
// Writing them needs `nodes: patch`, and Kubernetes RBAC cannot scope that to
// "your own Node object": NodeRestriction, which does exactly that, applies to
// kubelet identities and not to this ServiceAccount. So enabling this grants
// every node's plugin the ability to patch any Node in the cluster. That is a
// real trade and the operator makes it deliberately -- the chart renders both
// the flag and the RBAC only when nodeIdentity.enabled is set.
//
// Nothing here is fatal. A node that cannot annotate itself keeps serving
// volumes exactly as before; it simply does not get per-node access control,
// which is the same position every node was in before this existed.
func publishNodeIdentity(ctx context.Context, nodeID, hostRoot string) {
	nqn := node.HostNQN(hostRoot)
	iqn := node.InitiatorIQN(hostRoot)
	if nqn == "" && iqn == "" {
		slog.Warn("node identity publishing is on, but this node has neither an NVMe " +
			"host NQN nor an iSCSI initiator name to publish; per-node access control " +
			"will not apply to it")
		return
	}

	annotations := map[string]string{}
	if nqn != "" {
		annotations[backend.AnnotationNQN] = nqn
	}
	if iqn != "" {
		annotations[backend.AnnotationIQN] = iqn
	}

	cfg, err := kconfig.GetConfig()
	if err != nil {
		slog.Warn("cannot publish this node's storage identity: no in-cluster API access. "+
			"Per-node access control will not apply to this node",
			"error", obs.Redact(err.Error()))
		return
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		slog.Warn("cannot publish this node's storage identity", "error", obs.Redact(err.Error()))
		return
	}

	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": annotations},
	})
	if err != nil {
		slog.Warn("cannot publish this node's storage identity", "error", obs.Redact(err.Error()))
		return
	}

	ctx, cancel := context.WithTimeout(ctx, publishNodeIdentityTimeout)
	defer cancel()
	if _, err := cs.CoreV1().Nodes().Patch(ctx, nodeID, types.MergePatchType, patch,
		metav1.PatchOptions{}); err != nil {
		slog.Warn("could not annotate this node with its storage identity; per-node "+
			"access control will not apply to it, and volumes remain open to any "+
			"initiator that can reach the appliance",
			"node", nodeID, "error", obs.Redact(err.Error()))
		return
	}
	slog.Info("published this node's storage identity for per-node access control",
		"node", nodeID, "nqn", nqn != "", "iqn", iqn != "",
		"annotations", fmt.Sprintf("%s,%s", backend.AnnotationNQN, backend.AnnotationIQN))
}
