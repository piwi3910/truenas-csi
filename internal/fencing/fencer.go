package fencing

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/driver"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// RegistryFencer revokes appliance-side access through the same Publisher that
// ControllerUnpublishVolume uses.
//
// It is deliberately not a second implementation of the fence. There is exactly
// one place in this driver that knows how to take a node's access away — the
// backend's Unpublish — and a fencing controller with its own idea of how to do
// it would drift from it in precisely the way that leaves a volume looking
// fenced and reachable.
type RegistryFencer struct {
	Registry *backend.Registry
	// Nodes resolves the node id to the addresses and initiator names the grant
	// was written in terms of. Unpublish cannot revoke what it cannot name.
	Nodes backend.NodeResolver
}

// NewRegistryFencer builds a fencer over the driver's backend registry.
func NewRegistryFencer(reg *backend.Registry, nodes backend.NodeResolver) *RegistryFencer {
	return &RegistryFencer{Registry: reg, Nodes: nodes}
}

// Fence revokes appliance-side access to one volume for one node.
//
// A volume whose dataset is already gone counts as fenced: there is nothing
// left to reach. Everything else that goes wrong is an error, and an error here
// stops the whole cleanup — see Controller.handle.
func (f *RegistryFencer) Fence(ctx context.Context, id volume.ID, nodeID string) error {
	if f.Registry == nil || f.Nodes == nil {
		return errors.New("fencer is not configured with a registry and a node resolver")
	}
	node, err := f.Nodes.Resolve(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("resolving node %s: %w", nodeID, err)
	}
	b, err := f.Registry.For(ctx, id.Backend, id.Protocol)
	if err != nil {
		return fmt.Errorf("backend for %s: %w", id, err)
	}
	p, ok := b.(backend.Publisher)
	if !ok {
		return fmt.Errorf("backend %s/%s cannot revoke access per node", id.Backend, id.Protocol)
	}
	if err := p.Unpublish(ctx, id, node); err != nil {
		if errors.Is(err, backend.ErrVolumeGone) {
			return nil
		}
		return err
	}
	return nil
}

// NewEventRecorder builds a recorder that writes Events to the API server.
//
// Fencing is the one thing this driver does that destroys a running workload's
// pod, so every decision — and above all every refusal — has to be visible in
// `kubectl describe pod` without reading the controller's logs.
func NewEventRecorder(client kubernetes.Interface, namespace string) (record.EventRecorder, func()) {
	b := record.NewBroadcaster()
	b.StartRecordingToSink(&typedv1.EventSinkImpl{Interface: client.CoreV1().Events(namespace)})
	rec := b.NewRecorder(scheme.Scheme, corev1.EventSource{Component: driver.DriverName + "-fencing"})
	return rec, b.Shutdown
}
