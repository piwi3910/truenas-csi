package nvme

import (
	"context"
	"fmt"
	"strconv"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// Publish binds the volume's subsystem to its transport port, and grants the
// node's host NQN when one is known.
//
// The PORT BINDING is the fence. A namespace lives inside a subsystem and a
// subsystem is only discoverable through a port it is bound to, so a subsystem
// bound to nothing is unreachable from every node whatever its ACL says — and,
// unlike a host ACL, it needs no host NQN to be effective. That matters because
// a node id is all ControllerPublishVolume receives: if the NQN happens to be
// known the ACL is tightened as well, and if it is not the fence still holds.
func (b *nvmeBackend) Publish(ctx context.Context, id volume.ID, node backend.NodeRef) (map[string]string, error) {
	ctx = obs.WithVolume(ctx, id.String())

	ds, err := b.c.DatasetQuery(ctx, id.DatasetPath())
	if err != nil {
		return nil, fmt.Errorf("querying zvol %s: %w", id.DatasetPath(), err)
	}
	if ds == nil {
		return nil, fmt.Errorf("%w: %s", ErrVolumeNotFound, id)
	}

	name := subsystemName(id)
	subsys, err := b.c.NVMeSubsysByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("querying subsystem %s: %w", name, err)
	}
	if subsys == nil {
		return nil, fmt.Errorf("%w: subsystem %s", ErrVolumeNotFound, name)
	}

	port, err := b.portFor(ctx, ds)
	if err != nil {
		return nil, err
	}
	if err := b.bindPort(ctx, port.ID, subsys.ID); err != nil {
		return nil, err
	}
	if err := b.grantHost(ctx, subsys, node); err != nil {
		return nil, err
	}

	portal, err := portalAddress(b.c, port)
	if err != nil {
		return nil, err
	}
	obs.Logger(ctx).Info("published NVMe-oF volume", "node", node.ID, "subsystem", name, "port", port.ID)
	return publishContext(subsys, portal, port.TRType), nil
}

// Unpublish unbinds the volume's subsystem from every port, and revokes the
// node's host grant when one was made.
//
// Unbinding is what makes this a fence rather than a request: a connected node
// loses the path on the appliance side, so it cannot re-establish the
// connection its kernel is still trying to keep.
func (b *nvmeBackend) Unpublish(ctx context.Context, id volume.ID, node backend.NodeRef) error {
	ctx = obs.WithVolume(ctx, id.String())

	name := subsystemName(id)
	subsys, err := b.c.NVMeSubsysByName(ctx, name)
	if err != nil {
		return fmt.Errorf("querying subsystem %s: %w", name, err)
	}
	if subsys == nil {
		return nil // no subsystem, no path: CSI requires success here
	}

	if node.NQN != "" {
		host, err := b.c.NVMeHostByNQN(ctx, node.NQN)
		if err != nil {
			return fmt.Errorf("querying host %s: %w", node.NQN, err)
		}
		if host != nil {
			granted, err := b.c.NVMeHostSubsysList(ctx, subsys.ID)
			if err != nil {
				return fmt.Errorf("listing host ACLs of %s: %w", name, err)
			}
			for _, hs := range granted {
				if hs.HostID.ID != host.ID {
					continue
				}
				if err := b.c.NVMeHostSubsysDelete(ctx, hs.ID); err != nil {
					return fmt.Errorf("revoking %s on subsystem %s: %w", node.NQN, name, err)
				}
			}
		}
	}

	bindings, err := b.c.NVMePortSubsysList(ctx, subsys.ID)
	if err != nil {
		return fmt.Errorf("listing port bindings of %s: %w", name, err)
	}
	for _, ps := range bindings {
		if err := b.c.NVMePortSubsysDelete(ctx, ps.ID); err != nil {
			return fmt.Errorf("unbinding subsystem %s from its port: %w", name, err)
		}
	}
	obs.Logger(ctx).Info("fenced NVMe-oF volume", "node", node.ID, "subsystem", name)
	return nil
}

// portFor resolves the transport port this volume is served through.
//
// The id is read from the volume's own zvol rather than recomputed from
// StorageClass parameters, because ControllerPublishVolume receives none: a
// controller that restarted has no other way to tell an RDMA port from a TCP
// one, or a StorageClass that pinned port 4421 from one that did not.
func (b *nvmeBackend) portFor(ctx context.Context, ds *truenas.Dataset) (*truenas.NVMePort, error) {
	ports, err := b.c.NVMePortList(ctx)
	if err != nil {
		return nil, fmt.Errorf("querying NVMe-oF ports: %w", err)
	}
	if recorded := ds.LocalProperty(volume.NVMePortProperty); recorded != "" {
		if want, convErr := strconv.Atoi(recorded); convErr == nil {
			for i := range ports {
				if ports[i].ID == want {
					return &ports[i], nil
				}
			}
			obs.Logger(ctx).Warn("the recorded NVMe-oF port is gone; falling back to the appliance's remaining port",
				"port", want)
		}
	}
	// A volume provisioned before the port id was recorded, or one whose port
	// an operator replaced. One port is the normal appliance state and is
	// unambiguous; more than one is not something to guess at.
	if len(ports) == 1 {
		return &ports[0], nil
	}
	return nil, fmt.Errorf("cannot tell which of %d NVMe-oF ports serves %s: set the %s storage class parameter and reprovision",
		len(ports), ds.ID, ParamPortAddress)
}

// bindPort exports the subsystem through the port, tolerating a binding that
// already exists.
func (b *nvmeBackend) bindPort(ctx context.Context, portID, subsysID int) error {
	bindings, err := b.c.NVMePortSubsysList(ctx, subsysID)
	if err != nil {
		return fmt.Errorf("listing port bindings: %w", err)
	}
	for _, ps := range bindings {
		if ps.PortID.ID == portID {
			return nil
		}
	}
	if _, err := b.c.NVMePortSubsysCreate(ctx, portID, subsysID); err != nil {
		// The list above is a check-then-act, and ControllerPublishVolume is
		// retried and can run concurrently for the same volume: two callers
		// both see no binding and both create one, and the loser gets
		// "[EINVAL] ...port_id: This record already exists". Observed on a live
		// cluster, where it failed a publish that had in fact succeeded.
		//
		// Establish the state by query rather than by classifying the error,
		// exactly as ensureSubsystem does -- the middleware's errname is not
		// reliable enough to branch on.
		if again, qErr := b.c.NVMePortSubsysList(ctx, subsysID); qErr == nil {
			for _, ps := range again {
				if ps.PortID.ID == portID {
					return nil
				}
			}
		}
		return fmt.Errorf("binding subsystem %d to port %d: %w", subsysID, portID, err)
	}
	return nil
}

// grantHost registers the node's NQN and gives it access to the subsystem.
//
// It also turns allow_any_host OFF, which is the point: a subsystem created
// open — as one is when no host NQNs were configured — would ignore the ACL
// entirely, so adding an entry without closing the door grants nothing and
// fences nothing. Doing it here rather than at create time is what keeps a
// cluster with no known NQNs working: such a subsystem stays open and is fenced
// by its port binding alone.
func (b *nvmeBackend) grantHost(ctx context.Context, subsys *truenas.NVMeSubsystem, node backend.NodeRef) error {
	if node.NQN == "" {
		return nil
	}
	host, err := b.c.NVMeHostByNQN(ctx, node.NQN)
	if err != nil {
		return fmt.Errorf("querying host %s: %w", node.NQN, err)
	}
	if host == nil {
		if host, err = b.c.NVMeHostCreate(ctx, node.NQN); err != nil {
			if again, qErr := b.c.NVMeHostByNQN(ctx, node.NQN); qErr == nil && again != nil {
				host = again
			} else {
				return fmt.Errorf("registering host %s: %w", node.NQN, err)
			}
		}
	}
	granted, err := b.c.NVMeHostSubsysList(ctx, subsys.ID)
	if err != nil {
		return fmt.Errorf("listing host ACLs: %w", err)
	}
	have := false
	for _, hs := range granted {
		if hs.HostID.ID == host.ID {
			have = true
		}
	}
	if !have {
		if _, err := b.c.NVMeHostSubsysCreate(ctx, host.ID, subsys.ID); err != nil {
			// Same check-then-act race as bindPort: a concurrent publish for
			// the same volume and node may already have granted it.
			if again, qErr := b.c.NVMeHostSubsysList(ctx, subsys.ID); qErr == nil {
				for _, hs := range again {
					if hs.HostID.ID == host.ID {
						have = true
					}
				}
			}
			if !have {
				return fmt.Errorf("granting %s access to subsystem %d: %w", node.NQN, subsys.ID, err)
			}
		}
	}
	if subsys.AllowAnyHost {
		// VERIFIED on 25.10.6: nvmet.subsys.update accepts a lone allow_any_host
		// patch, and the subsystem reads back allow_any_host=false with the host
		// ACL entry intact.
		if err := b.c.CallJSON(ctx, nil, "nvmet.subsys.update", subsys.ID,
			map[string]any{"allow_any_host": false}); err != nil {
			return fmt.Errorf("closing subsystem %d to unlisted hosts: %w", subsys.ID, err)
		}
		subsys.AllowAnyHost = false
	}
	return nil
}

// ensure the interfaces stay satisfied even if either grows a method.
var (
	_ backend.Backend   = (*nvmeBackend)(nil)
	_ backend.Publisher = (*nvmeBackend)(nil)
)
