package truenas

import "context"

// NFSShareByPath finds the export for a path, or (nil, nil) when there is none.
func (c *Ops) NFSShareByPath(ctx context.Context, path string) (*NFSShare, error) {
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
func (c *Ops) NFSShareDelete(ctx context.Context, id int) error {
	err := c.CallJSON(ctx, nil, "sharing.nfs.delete", id)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}
