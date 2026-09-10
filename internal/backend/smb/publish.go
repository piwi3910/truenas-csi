package smb

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// Publish adds the node's addresses to the share's hostsallow list.
//
// It is idempotent by construction: the new list is the union of what the share
// already allows and what this node needs, so a repeated publish of the same
// volume to the same node rewrites the same list.
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
	if len(node.Addrs) == 0 {
		// hostsallow is written in addresses. With none, publishing would grant
		// nothing and the fence would have nothing to revoke.
		return nil, status.Errorf(codes.FailedPrecondition,
			"%v: no address is known for node %s", backend.ErrNotFenceable, node.ID)
	}

	mountpoint := mountpointOf(ds, dsPath)
	share, err := b.shareByPath(ctx, mountpoint)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query SMB share for %s: %v", mountpoint, err)
	}
	if share != nil && !share.serving() {
		// The publish path matters more than the create path here: an existing
		// volume's pod being rescheduled comes through HERE, not through
		// CreateVolume, so a share disabled after provisioning was still
		// adopted and the node then failed to mount with the appliance's own
		// "No such file or directory" and nothing naming the cause. Observed
		// on a real cluster.
		return nil, status.Errorf(codes.FailedPrecondition,
			"the SMB share for %s is disabled on the appliance, so this volume cannot "+
				"be mounted; re-enable it or delete it and let the driver recreate it",
			mountpoint)
	}
	if share == nil {
		// Self-healing: a share removed by hand is recreated already fenced to
		// this node, rather than published open and narrowed afterwards.
		p := b.recall(id)
		if _, err := b.createShare(ctx, mountpoint, shareName(id, p.sharePrefix), node.Addrs); err != nil {
			return nil, status.Errorf(codes.Internal, "create SMB share for %s: %v", mountpoint, err)
		}
		return b.PublishContext(ctx, id)
	}
	allow := append(append([]string{}, share.hostsAllow()...), node.Addrs...)
	if err := b.setHostsAllow(ctx, share, allow); err != nil {
		return nil, status.Errorf(codes.Internal, "granting %s access to %s: %v", node.ID, mountpoint, err)
	}
	return b.PublishContext(ctx, id)
}

// Unpublish removes the node's addresses from the share's hostsallow list.
//
// Revoking the LAST node leaves hostsallow empty, which with hostsdeny=["ALL"]
// denies everyone. That pairing is the whole reason DenyAll is written on every
// update: an empty allow list on its own would mean "no restriction".
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
		return status.Errorf(codes.Internal, "query SMB share for %s: %v", mountpoint, err)
	}
	if share == nil {
		return nil // nothing is shared, so nothing reaches the data
	}
	if err := b.setHostsAllow(ctx, share, withoutHosts(share.hostsAllow(), node.Addrs)); err != nil {
		return status.Errorf(codes.Internal, "revoking %s access to %s: %v", node.ID, mountpoint, err)
	}
	obs.Logger(ctx).Info("revoked SMB access", "node", node.ID, "share", mountpoint)
	return nil
}
