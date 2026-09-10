package nfs

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// Publish adds the node's addresses to the export's host access list.
//
// It is idempotent by construction: the new list is the union of what the
// export already allows and what this node needs, so a repeated publish of the
// same volume to the same node rewrites the same list.
func (b *Backend) Publish(ctx context.Context, id volume.ID, node backend.NodeRef) (map[string]string, error) {
	if err := volume.Confine(id, b.pool, b.parent); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	ctx = obs.WithVolume(ctx, id.String())
	dsPath := id.DatasetPath()

	ds, err := b.c.DatasetQuery(ctx, dsPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query dataset %s: %v", dsPath, err)
	}
	if ds == nil {
		return nil, status.Errorf(codes.NotFound, "volume %s does not exist", id)
	}

	granted, err := grantHosts(node.Addrs, splitNetworks(ds.LocalProperty(volume.NetworksProperty)))
	if err != nil {
		return nil, err
	}
	if len(granted) == 0 {
		// Publishing with nothing to grant would produce a volume the node
		// cannot mount and a fence with nothing to revoke — a failure that
		// would otherwise surface much later as an unexplained mount timeout.
		return nil, status.Errorf(codes.FailedPrecondition,
			"%v: node %s has no address inside the networks volume %s is exported to",
			backend.ErrNotFenceable, node.ID, id)
	}

	mountpoint := mountpointOf(ds, dsPath)
	share, err := b.shareByPath(ctx, mountpoint)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query NFS share for %s: %v", mountpoint, err)
	}
	if share != nil && !share.serving() {
		// This covers a share disabled AFTER the volume was provisioned, which
		// CreateVolume never sees again. It fires when the volume ATTACHES: a
		// pod landing on a different node, or a VolumeAttachment recreated.
		//
		// It does NOT fire when a pod restarts on the same node with the
		// attachment still in place — no controller RPC runs at all there, and
		// the node only sees the appliance's own "reason given by server: No
		// such file or directory". Both were measured on a real cluster; this
		// catches the case a controller can catch, and the volume recovers on
		// its own once the share is re-enabled.
		return nil, status.Errorf(codes.FailedPrecondition,
			"the NFS share for %s is disabled on the appliance, so this volume cannot "+
				"be mounted; re-enable it or delete it and let the driver recreate it",
			mountpoint)
	}
	if share == nil {
		// Self-healing rather than a failure: an export removed by hand is
		// recreated already fenced to this node, which is the state the volume
		// is supposed to be in. Creating it open and narrowing it afterwards is
		// exactly what createShare exists to avoid.
		if _, err := b.createShare(ctx, mountpoint, granted, b.recall(id)); err != nil {
			return nil, status.Errorf(codes.Internal, "create NFS share for %s: %v", mountpoint, err)
		}
		return b.PublishContext(ctx, id)
	}
	if err := b.setHosts(ctx, share.ID, append(append([]string{}, share.Hosts...), granted...)); err != nil {
		return nil, status.Errorf(codes.Internal, "granting %s access to %s: %v", node.ID, mountpoint, err)
	}
	return b.PublishContext(ctx, id)
}

// Unpublish removes the node's addresses from the export's host access list.
//
// The list it writes back always retains DenyHost, so revoking the LAST node
// leaves an export nobody matches rather than an export to everyone.
func (b *Backend) Unpublish(ctx context.Context, id volume.ID, node backend.NodeRef) error {
	if err := volume.Confine(id, b.pool, b.parent); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	ctx = obs.WithVolume(ctx, id.String())
	dsPath := id.DatasetPath()

	ds, err := b.c.DatasetQuery(ctx, dsPath)
	if err != nil {
		return status.Errorf(codes.Internal, "query dataset %s: %v", dsPath, err)
	}
	if ds == nil {
		return nil // a volume that is gone is fenced: CSI requires success here
	}

	mountpoint := mountpointOf(ds, dsPath)
	share, err := b.shareByPath(ctx, mountpoint)
	if err != nil {
		return status.Errorf(codes.Internal, "query NFS share for %s: %v", mountpoint, err)
	}
	if share == nil {
		return nil // nothing is exported, so nothing reaches the data
	}
	if err := b.setHosts(ctx, share.ID, withoutHosts(share.Hosts, node.Addrs)); err != nil {
		return status.Errorf(codes.Internal, "revoking %s access to %s: %v", node.ID, mountpoint, err)
	}
	obs.Logger(ctx).Info("revoked NFS access", "node", node.ID, "share", mountpoint)
	return nil
}

// recall returns the parameters seen at Create time, or the defaults when this
// process never saw them — a controller that restarted must still be able to
// repair an export for a volume it did not create.
func (b *Backend) recall(id volume.ID) params {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := params{nfsVersion: defaultNFSVersion, mode: defaultMode, maproot: defaultMaproot}
	if v, ok := b.versions[id.String()]; ok && v != "" {
		p.nfsVersion = v
	}
	p.server = b.server
	return p
}
