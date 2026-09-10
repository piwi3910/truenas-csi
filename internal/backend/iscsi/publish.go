package iscsi

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// unmappedLUN is the sentinel publishContext takes for "this volume occupies no
// LUN yet", so a provisioned-but-unpublished volume reports no address rather
// than a wrong one.
const unmappedLUN = -1

// Publish maps the volume's extent into the shared target.
//
// The mapping IS the fence. Initiator ACLs on TrueNAS belong to the target, not
// to a LUN, so there is no per-node grant to make here — but this backend only
// ever serves SINGLE_NODE_* access modes, so exactly one node holds a volume at
// a time and removing the mapping removes it from that node. The node argument
// is therefore recorded by the caller's ledger rather than written to the
// appliance; nothing about the mapping differs per node.
//
// It is idempotent, and it ADOPTS an existing mapping rather than replacing it.
// Every volume provisioned before the mapping moved out of CreateVolume already
// has one, and re-creating it would either fail on the duplicate or hand the
// volume a second address.
func (b *iscsiBackend) Publish(ctx context.Context, id volume.ID, node backend.NodeRef) (map[string]string, error) {
	ctx = obs.WithVolume(ctx, id.String())

	ds, err := b.c.DatasetQuery(ctx, id.DatasetPath())
	if err != nil {
		return nil, fmt.Errorf("querying zvol %s: %w", id.DatasetPath(), err)
	}
	if ds == nil {
		return nil, fmt.Errorf("%w: %s", ErrVolumeNotFound, id)
	}

	name := extentName(id)
	extent, err := b.c.ExtentByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("querying extent %s: %w", name, err)
	}
	if extent == nil {
		return nil, fmt.Errorf("%w: extent %s", ErrVolumeNotFound, name)
	}
	// An extent carries its own enabled switch, and a disabled one presents no
	// device: the LUN maps, the publish reports success, and the node then
	// waits for a device that never appears. Verified on a real appliance —
	// iscsi.extent.update accepts {"enabled": false} and the extent still
	// answers a query under the same name.
	if !extent.Serving() {
		return nil, status.Errorf(codes.FailedPrecondition,
			"the iSCSI extent %s is disabled on the appliance, so this volume presents "+
				"no device to the node; re-enable it in Shares > Block Shares > Extents",
			name)
	}

	tname := targetName(b.pool, b.parent)
	target, err := queryTarget(ctx, b.c, tname)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, fmt.Errorf("shared target %s does not exist", tname)
	}
	iqn, err := targetIQN(ctx, b.c, tname)
	if err != nil {
		return nil, err
	}

	lun, err := b.mapExtent(ctx, ds, target.ID, extent.ID)
	if err != nil {
		return nil, err
	}
	obs.Logger(ctx).Info("mapped iSCSI LUN", "node", node.ID, "target", tname, "lun", lun)

	p := Params{Pool: b.pool, Parent: b.parent}
	if len(target.Groups) > 0 {
		p.PortalID = target.Groups[0].Portal
		p.CHAP = strings.EqualFold(target.Groups[0].AuthMethod, "CHAP")
	}
	return b.publishContext(ctx, p, iqn, extent.NAA, lun)
}

// Unpublish removes the volume's LUN mapping from the shared target.
//
// After it returns the extent has no address on the target, so the node's
// kernel can hold whatever session state it likes: there is nothing behind the
// LUN to read or write. Removing a mapping that is already gone is success,
// which is what makes the CSI retry loop and the fencing controller safe to
// call this repeatedly.
func (b *iscsiBackend) Unpublish(ctx context.Context, id volume.ID, node backend.NodeRef) error {
	ctx = obs.WithVolume(ctx, id.String())

	name := extentName(id)
	extent, err := b.c.ExtentByName(ctx, name)
	if err != nil {
		return fmt.Errorf("querying extent %s: %w", name, err)
	}
	if extent == nil {
		return nil // no extent, no address, nothing to fence
	}
	target, err := queryTarget(ctx, b.c, targetName(b.pool, b.parent))
	if err != nil {
		return err
	}
	if target == nil {
		return nil
	}
	mappings, err := b.c.TargetExtentList(ctx, target.ID)
	if err != nil {
		return fmt.Errorf("listing LUN mappings: %w", err)
	}
	for _, m := range mappings {
		if m.Extent != extent.ID {
			continue
		}
		if err := b.c.TargetExtentDelete(ctx, m.ID); err != nil {
			return fmt.Errorf("removing LUN %d: %w", m.LUNID, err)
		}
		releaseLUN(target.ID, m.LUNID)
		obs.Logger(ctx).Info("unmapped iSCSI LUN", "node", node.ID, "lun", m.LUNID)
	}
	return nil
}

// mapExtent returns the LUN the extent occupies, creating the mapping when it
// has none.
func (b *iscsiBackend) mapExtent(ctx context.Context, ds *truenas.Dataset, targetID, extentID int) (int, error) {
	mappings, err := b.c.TargetExtentList(ctx, targetID)
	if err != nil {
		return 0, fmt.Errorf("listing LUN mappings: %w", err)
	}
	used := make(map[int]bool, len(mappings))
	for _, m := range mappings {
		if m.Extent == extentID {
			// Adopted, not rewritten. Record the id so a later republish lands
			// on it too, including for volumes provisioned before the mapping
			// became a publish-time object.
			b.rememberLUN(ctx, ds.ID, m.LUNID)
			return m.LUNID, nil
		}
		used[m.LUNID] = true
	}

	lun, err := b.claimLUN(ctx, ds, targetID, used)
	if err != nil {
		return 0, err
	}
	if _, err := b.c.TargetExtentCreate(ctx, targetID, extentID, lun); err != nil {
		releaseLUN(targetID, lun)
		return 0, fmt.Errorf("mapping extent %d to LUN %d: %w", extentID, lun, err)
	}
	b.rememberLUN(ctx, ds.ID, lun)
	return lun, nil
}

// claimLUN prefers the id this volume held last time.
//
// Stability matters because LUN ids are recycled: the allocator hands out the
// lowest free id, so after an unrelated volume is deleted a republish could
// land this volume on an id another volume used to occupy. An initiator that
// cached the old mapping would then address the wrong device. Preferring the
// recorded id keeps a volume's address fixed for its whole life, and the
// fallback keeps a volume publishable when its old id has since been taken.
func (b *iscsiBackend) claimLUN(ctx context.Context, ds *truenas.Dataset, targetID int, used map[int]bool) (int, error) {
	if v := ds.LocalProperty(volume.LUNProperty); v != "" {
		n, err := strconv.Atoi(v)
		switch {
		case err != nil || n < 0 || n >= maxLUNs:
			obs.Logger(ctx).Warn("ignoring an unusable recorded LUN id", "value", v)
		case used[n]:
			obs.Logger(ctx).Warn("recorded LUN id is taken by another volume; allocating a new one",
				"lun", n, "target", targetID)
		default:
			return n, nil
		}
	}
	return allocateLUN(ctx, b.c, targetID)
}

// rememberLUN records the volume's LUN id on its own zvol.
//
// A failure here is logged and not returned: the mapping already exists and the
// volume is usable, and refusing the publish would strand a pod over a piece of
// bookkeeping whose only job is to make the NEXT publish land on the same id.
func (b *iscsiBackend) rememberLUN(ctx context.Context, dsPath string, lun int) {
	if err := b.c.SetUserProperty(ctx, dsPath, volume.LUNProperty, strconv.Itoa(lun)); err != nil {
		obs.Logger(ctx).Warn("could not record the LUN id", "dataset", dsPath, "lun", lun, "error", err)
	}
}

// ensure the interfaces stay satisfied even if either grows a method.
var (
	_ backend.Backend   = (*iscsiBackend)(nil)
	_ backend.Publisher = (*iscsiBackend)(nil)
)
