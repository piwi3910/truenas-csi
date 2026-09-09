package nvme

import (
	"context"
	"fmt"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// portMu serialises port creation. The port is the ONE object this backend
// shares between volumes, so two concurrent CreateVolume calls on a cold
// appliance must issue exactly one create between them.
var portMu sync.Mutex

// trtypeFor maps a StorageClass transport onto the middleware's spelling.
func trtypeFor(transport string) string {
	if transport == TransportRDMA {
		return "RDMA"
	}
	return "TCP"
}

// ensureSubsystem returns this volume's subsystem, creating it on first use.
//
// allow_any_host is derived from the configured host NQNs, and an EMPTY list
// deliberately means OPEN rather than a closed subsystem with no hosts. On
// TrueNAS a subsystem with allow_any_host=false and an empty ACL accepts
// nobody, so writing that when the operator configured no NQNs would take the
// backend silently offline — every volume would provision cleanly and then fail
// to attach with a connect error nothing in the driver explains. This mirrors
// exactly the reasoning the iSCSI backend applies to an empty initiator group.
func (b *nvmeBackend) ensureSubsystem(ctx context.Context, id volume.ID, p Params, rollback *[]func()) (*truenas.NVMeSubsystem, error) {
	name := subsystemName(id)
	existing, err := b.c.NVMeSubsysByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("querying subsystem %s: %w", name, err)
	}
	if existing != nil {
		return existing, nil
	}

	open := len(p.HostNQNs) == 0
	if open {
		// Said out loud, once per subsystem, because nothing else says it. An
		// open subsystem is reachable by ANY initiator that can reach the
		// portal -- there is no ACL to consult -- and the driver closes one
		// only when it knows an NQN to admit: either hostNQNs on the
		// StorageClass, or the node's own csi.truenas.watteel.com/nqn
		// annotation. With neither, every NVMe volume on this backend is open.
		obs.Logger(ctx).Warn("creating an OPEN NVMe subsystem: any initiator that can "+
			"reach the portal may connect to this volume. Set hostNQNs on the "+
			"StorageClass, or annotate each node with its host NQN, to close it",
			"subsystem", name, "annotation", "csi.truenas.watteel.com/nqn")
	}
	created, err := b.c.NVMeSubsysCreate(ctx, name, open)
	if err != nil {
		// A concurrent creator may have won. Establish the state by query
		// rather than by classifying the error, whose errname is unreliable.
		if again, qErr := b.c.NVMeSubsysByName(ctx, name); qErr == nil && again != nil {
			return again, nil
		}
		return nil, fmt.Errorf("creating subsystem %s: %w", name, err)
	}
	subsysID := created.ID
	*rollback = append(*rollback, func() {
		_ = b.c.NVMeSubsysDelete(context.WithoutCancel(ctx), subsysID)
	})
	return created, nil
}

// ensureNamespace exports the volume's zvol inside its subsystem.
func (b *nvmeBackend) ensureNamespace(ctx context.Context, id volume.ID, subsysID int, rollback *[]func()) error {
	dev := devicePath(id)
	existing, err := b.c.NVMeNamespaceByDevice(ctx, dev)
	if err != nil {
		return fmt.Errorf("querying namespace for %s: %w", dev, err)
	}
	if existing != nil {
		return nil
	}
	created, err := b.c.NVMeNamespaceCreate(ctx, subsysID, dev)
	if err != nil {
		if again, qErr := b.c.NVMeNamespaceByDevice(ctx, dev); qErr == nil && again != nil {
			return nil
		}
		return fmt.Errorf("creating namespace for %s: %w", dev, err)
	}
	nsID := created.ID
	*rollback = append(*rollback, func() {
		_ = b.c.NVMeNamespaceDelete(context.WithoutCancel(ctx), nsID)
	})
	return nil
}

// ensurePort returns the transport port every subsystem is bound to, creating
// it only when no matching one exists.
//
// The port is appliance-wide and shared by every volume, so it is registered
// for rollback ONLY when this call created it. Deleting a port another volume
// is exported through would detach live workloads.
func (b *nvmeBackend) ensurePort(ctx context.Context, p Params, rollback *[]func()) (*truenas.NVMePort, error) {
	portMu.Lock()
	defer portMu.Unlock()

	trtype := trtypeFor(p.Transport)
	addr := p.PortAddress
	if addr == "" {
		addr = b.c.Host()
	}

	// Query first. An existing port on this transport and service id is used
	// whatever address it carries, including the wildcard an operator may have
	// configured by hand — the publish path resolves that to a dialable
	// address rather than creating a competing listener.
	ports, err := b.c.NVMePortList(ctx)
	if err != nil {
		return nil, fmt.Errorf("querying NVMe-oF ports: %w", err)
	}
	for i := range ports {
		if ports[i].TRType == trtype && ports[i].Port() == p.Port {
			return &ports[i], nil
		}
	}

	if addr == "" {
		return nil, fmt.Errorf("no address to bind an NVMe-oF port to: set the %s storage class parameter", ParamPortAddress)
	}
	created, err := b.c.NVMePortCreate(ctx, trtype, addr, p.Port)
	if err != nil {
		if again, qErr := b.c.NVMePortFind(ctx, trtype, addr, p.Port); qErr == nil && again != nil {
			return again, nil
		}
		return nil, fmt.Errorf("creating %s port on %s:%d: %w", trtype, addr, p.Port, err)
	}
	portID := created.ID
	*rollback = append(*rollback, func() {
		_ = b.c.NVMePortDelete(context.WithoutCancel(ctx), portID)
	})
	return created, nil
}

// ensureHostACL registers the configured initiator NQNs and grants them access
// to this volume's subsystem.
//
// It writes NOTHING when no host NQNs are configured: the subsystem was created
// with allow_any_host in that case, and an empty ACL would deny everything.
func (b *nvmeBackend) ensureHostACL(ctx context.Context, subsysID int, p Params, rollback *[]func()) error {
	if len(p.HostNQNs) == 0 {
		return nil
	}
	granted, err := b.c.NVMeHostSubsysList(ctx, subsysID)
	if err != nil {
		return fmt.Errorf("listing host ACLs: %w", err)
	}
	have := map[int]bool{}
	for _, hs := range granted {
		have[hs.HostID.ID] = true
	}

	for _, nqn := range p.HostNQNs {
		host, err := b.c.NVMeHostByNQN(ctx, nqn)
		if err != nil {
			return fmt.Errorf("querying host %s: %w", nqn, err)
		}
		if host == nil {
			// A host is an appliance-wide object shared with every other
			// subsystem, so it is never rolled back: removing it would revoke
			// another volume's ACL.
			if host, err = b.c.NVMeHostCreate(ctx, nqn); err != nil {
				if again, qErr := b.c.NVMeHostByNQN(ctx, nqn); qErr == nil && again != nil {
					host = again
				} else {
					return fmt.Errorf("registering host %s: %w", nqn, err)
				}
			}
		}
		if have[host.ID] {
			continue
		}
		created, err := b.c.NVMeHostSubsysCreate(ctx, host.ID, subsysID)
		if err != nil {
			return fmt.Errorf("granting %s access to subsystem %d: %w", nqn, subsysID, err)
		}
		hsID := created.ID
		*rollback = append(*rollback, func() {
			_ = b.c.NVMeHostSubsysDelete(context.WithoutCancel(ctx), hsID)
		})
	}
	return nil
}

// portalAddress renders "<ip>:<port>" for a port.
//
// A port bound to the wildcard reports 0.0.0.0 or ::, which is where the
// APPLIANCE listens, not an address a node can dial — handing it to
// `nvme connect` produces a connection refused against 0.0.0.0. The client's
// own host is the fallback, exactly as the iSCSI backend does for portals.
func portalAddress(c truenas.API, p *truenas.NVMePort) (string, error) {
	port := p.Port()
	if port == 0 {
		port = DefaultPort
	}
	addr := strings.TrimSpace(p.TRAddr)
	if addr != "" && !isWildcardAddress(addr) {
		return net.JoinHostPort(addr, strconv.Itoa(port)), nil
	}
	if host := c.Host(); host != "" {
		return net.JoinHostPort(host, strconv.Itoa(port)), nil
	}
	return "", fmt.Errorf("NVMe-oF port %d has no reachable listen address", p.ID)
}

// isWildcardAddress reports whether an address is a listen-anywhere placeholder
// rather than something an initiator can connect to.
func isWildcardAddress(ip string) bool {
	switch strings.TrimSpace(ip) {
	case "0.0.0.0", "::", "[::]", "*", "":
		return true
	}
	return false
}
