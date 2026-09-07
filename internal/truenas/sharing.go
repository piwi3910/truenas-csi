package truenas

import "context"

// NFSShareSpec describes an NFS export to create.
type NFSShareSpec struct {
	Path         string
	Comment      string
	Networks     []string
	MaprootUser  string
	MaprootGroup string
	ReadOnly     bool
}

// NFSShareCreate exports a dataset over NFS.
func (c *Client) NFSShareCreate(ctx context.Context, spec NFSShareSpec) (*NFSShare, error) {
	p := map[string]any{"path": spec.Path, "comment": spec.Comment, "ro": spec.ReadOnly}
	if len(spec.Networks) > 0 {
		p["networks"] = spec.Networks
	}
	if spec.MaprootUser != "" {
		p["maproot_user"] = spec.MaprootUser
	}
	if spec.MaprootGroup != "" {
		p["maproot_group"] = spec.MaprootGroup
	}
	var s NFSShare
	if err := c.CallJSON(ctx, &s, "sharing.nfs.create", p); err != nil {
		return nil, err
	}
	return &s, nil
}

// NFSShareByPath finds the export for a path, or (nil, nil) when there is none.
func (c *Client) NFSShareByPath(ctx context.Context, path string) (*NFSShare, error) {
	var out []NFSShare
	err := c.CallJSON(ctx, &out, "sharing.nfs.query",
		[]any{[]any{"path", "=", path}}, map[string]any{})
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

// NFSShareDelete removes an export; already absent is success.
func (c *Client) NFSShareDelete(ctx context.Context, id int) error {
	err := c.CallJSON(ctx, nil, "sharing.nfs.delete", id)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}
