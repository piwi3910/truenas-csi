package truenas

import "context"

// ISCSIGlobalConfig returns the global iSCSI settings, including the IQN base
// name that every target name is prefixed with.
func (c *Ops) ISCSIGlobalConfig(ctx context.Context) (*ISCSIGlobal, error) {
	var g ISCSIGlobal
	if err := c.CallJSON(ctx, &g, "iscsi.global.config"); err != nil {
		return nil, err
	}
	return &g, nil
}

// PortalList returns the configured portals.
func (c *Ops) PortalList(ctx context.Context) ([]ISCSIPortal, error) {
	var out []ISCSIPortal
	if err := c.CallJSON(ctx, &out, "iscsi.portal.query"); err != nil {
		return nil, err
	}
	return out, nil
}

// PortalCreate adds a portal listening on the given addresses.
func (c *Ops) PortalCreate(ctx context.Context, comment string, ips []string) (*ISCSIPortal, error) {
	listen := make([]map[string]string, 0, len(ips))
	for _, ip := range ips {
		listen = append(listen, map[string]string{"ip": ip})
	}
	var p ISCSIPortal
	if err := c.CallJSON(ctx, &p, "iscsi.portal.create",
		map[string]any{"comment": comment, "listen": listen}); err != nil {
		return nil, err
	}
	return &p, nil
}

// PortalDelete removes a portal.
func (c *Ops) PortalDelete(ctx context.Context, id int) error {
	err := c.CallJSON(ctx, nil, "iscsi.portal.delete", id)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}

// TargetByName finds a target, or (nil, nil) when absent.
func (c *Ops) TargetByName(ctx context.Context, name string) (*ISCSITarget, error) {
	var out []ISCSITarget
	err := c.CallJSON(ctx, &out, "iscsi.target.query",
		[]any{[]any{"name", "=", name}}, map[string]any{})
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// TargetCreate creates a target bound to a portal, optionally restricted to an
// initiator group.
func (c *Ops) TargetCreate(ctx context.Context, name string, portalID, initiatorID int) (*ISCSITarget, error) {
	group := map[string]any{"portal": portalID}
	if initiatorID > 0 {
		group["initiator"] = initiatorID
	}
	var t ISCSITarget
	if err := c.CallJSON(ctx, &t, "iscsi.target.create",
		map[string]any{"name": name, "groups": []any{group}}); err != nil {
		return nil, err
	}
	return &t, nil
}

// TargetDelete removes a target.
func (c *Ops) TargetDelete(ctx context.Context, id int) error {
	err := c.CallJSON(ctx, nil, "iscsi.target.delete", id, true)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}

// ExtentByName finds an extent, or (nil, nil) when absent.
func (c *Ops) ExtentByName(ctx context.Context, name string) (*ISCSIExtent, error) {
	var out []ISCSIExtent
	err := c.CallJSON(ctx, &out, "iscsi.extent.query",
		[]any{[]any{"name", "=", name}}, map[string]any{})
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// ExtentCreate exposes a zvol as an extent. The returned NAA is what the node
// uses to find the device deterministically under /dev/disk/by-id.
func (c *Ops) ExtentCreate(ctx context.Context, name, zvolPath string) (*ISCSIExtent, error) {
	var e ISCSIExtent
	if err := c.CallJSON(ctx, &e, "iscsi.extent.create", map[string]any{
		"name": name, "type": "DISK", "disk": "zvol/" + zvolPath,
	}); err != nil {
		return nil, err
	}
	return &e, nil
}

// ExtentDelete removes an extent.
func (c *Ops) ExtentDelete(ctx context.Context, id int) error {
	err := c.CallJSON(ctx, nil, "iscsi.extent.delete", id, true, true)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}

// TargetExtentList returns the LUN mappings for a target. LUN ids are derived
// from this live query rather than cached, so a controller restart can neither
// lose nor duplicate an allocation.
func (c *Ops) TargetExtentList(ctx context.Context, targetID int) ([]ISCSITargetExtent, error) {
	var out []ISCSITargetExtent
	err := c.CallJSON(ctx, &out, "iscsi.targetextent.query",
		[]any{[]any{"target", "=", targetID}}, map[string]any{})
	if err != nil && !IsNotFound(err) {
		return nil, err
	}
	return out, nil
}

// TargetExtentCreate maps an extent into a target at a LUN id.
func (c *Ops) TargetExtentCreate(ctx context.Context, targetID, extentID, lun int) (*ISCSITargetExtent, error) {
	var te ISCSITargetExtent
	if err := c.CallJSON(ctx, &te, "iscsi.targetextent.create", map[string]any{
		"target": targetID, "extent": extentID, "lunid": lun,
	}); err != nil {
		return nil, err
	}
	return &te, nil
}

// TargetExtentDelete removes a LUN mapping.
//
// force is not optional here, the way it is not optional for ExtentDelete. The
// appliance refuses to unmap a LUN while the ASSOCIATED TARGET has any session
// -- "[EFAULT] Associated target iqn...:csi-pool0-k8s is in use." -- and this
// driver puts every volume on a backend on one shared target, so that target is
// in use whenever any volume anywhere on the backend is attached. Without force
// the unmap could only succeed when the entire backend was idle.
//
// The consequences were not subtle. ControllerUnpublishVolume failed and the
// attacher retried it for ever: VolumeAttachments were never removed, so
// PersistentVolumes could not be deleted and claims sat in Terminating, and
// nodes could not be drained. Worse, the per-publish LUN mapping is the
// appliance-side fence for iSCSI -- an unmap that never succeeds is a grant
// that is never revoked.
//
// Forcing is safe at this point in the protocol: CSI guarantees
// NodeUnstageVolume has completed for this volume on this node before
// ControllerUnpublishVolume is called, so the node has already flushed and
// stopped using the device. What the appliance objects to is the target being
// busy, not this LUN.
//
// Measured on 25.10.6 with a live session on the shared target: delete without
// force returns EFAULT, delete with force succeeds.
func (c *Ops) TargetExtentDelete(ctx context.Context, id int) error {
	err := c.CallJSON(ctx, nil, "iscsi.targetextent.delete", id, true)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}
