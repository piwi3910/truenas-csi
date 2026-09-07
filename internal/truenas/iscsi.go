package truenas

import "context"

// ISCSIGlobalConfig returns the global iSCSI settings, including the IQN base
// name that every target name is prefixed with.
func (c *Client) ISCSIGlobalConfig(ctx context.Context) (*ISCSIGlobal, error) {
	var g ISCSIGlobal
	if err := c.CallJSON(ctx, &g, "iscsi.global.config"); err != nil {
		return nil, err
	}
	return &g, nil
}

// PortalList returns the configured portals.
func (c *Client) PortalList(ctx context.Context) ([]ISCSIPortal, error) {
	var out []ISCSIPortal
	if err := c.CallJSON(ctx, &out, "iscsi.portal.query"); err != nil {
		return nil, err
	}
	return out, nil
}

// PortalCreate adds a portal listening on the given addresses.
func (c *Client) PortalCreate(ctx context.Context, comment string, ips []string) (*ISCSIPortal, error) {
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
func (c *Client) PortalDelete(ctx context.Context, id int) error {
	err := c.CallJSON(ctx, nil, "iscsi.portal.delete", id)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}

// TargetByName finds a target, or (nil, nil) when absent.
func (c *Client) TargetByName(ctx context.Context, name string) (*ISCSITarget, error) {
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
func (c *Client) TargetCreate(ctx context.Context, name string, portalID, initiatorID int) (*ISCSITarget, error) {
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
func (c *Client) TargetDelete(ctx context.Context, id int) error {
	err := c.CallJSON(ctx, nil, "iscsi.target.delete", id, true)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}

// ExtentByName finds an extent, or (nil, nil) when absent.
func (c *Client) ExtentByName(ctx context.Context, name string) (*ISCSIExtent, error) {
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
func (c *Client) ExtentCreate(ctx context.Context, name, zvolPath string) (*ISCSIExtent, error) {
	var e ISCSIExtent
	if err := c.CallJSON(ctx, &e, "iscsi.extent.create", map[string]any{
		"name": name, "type": "DISK", "disk": "zvol/" + zvolPath,
	}); err != nil {
		return nil, err
	}
	return &e, nil
}

// ExtentDelete removes an extent.
func (c *Client) ExtentDelete(ctx context.Context, id int) error {
	err := c.CallJSON(ctx, nil, "iscsi.extent.delete", id, true, true)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}

// TargetExtentList returns the LUN mappings for a target. LUN ids are derived
// from this live query rather than cached, so a controller restart can neither
// lose nor duplicate an allocation.
func (c *Client) TargetExtentList(ctx context.Context, targetID int) ([]ISCSITargetExtent, error) {
	var out []ISCSITargetExtent
	err := c.CallJSON(ctx, &out, "iscsi.targetextent.query",
		[]any{[]any{"target", "=", targetID}}, map[string]any{})
	if err != nil && !IsNotFound(err) {
		return nil, err
	}
	return out, nil
}

// TargetExtentCreate maps an extent into a target at a LUN id.
func (c *Client) TargetExtentCreate(ctx context.Context, targetID, extentID, lun int) (*ISCSITargetExtent, error) {
	var te ISCSITargetExtent
	if err := c.CallJSON(ctx, &te, "iscsi.targetextent.create", map[string]any{
		"target": targetID, "extent": extentID, "lunid": lun,
	}); err != nil {
		return nil, err
	}
	return &te, nil
}

// TargetExtentDelete removes a LUN mapping.
func (c *Client) TargetExtentDelete(ctx context.Context, id int) error {
	err := c.CallJSON(ctx, nil, "iscsi.targetextent.delete", id)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}
