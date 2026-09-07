package truenas

import (
	"context"
	"encoding/json"
)

// ISCSISessionCount returns the number of iSCSI sessions the appliance
// currently has open, across every target.
//
// iscsi.global.sessions returns one object per session with initiator, target
// and connection detail. Only the count is kept: an initiator IQN or a target
// name as a metric label would be unbounded, and it is not the driver's data to
// publish.
func (c *Ops) ISCSISessionCount(ctx context.Context) (int, error) {
	var out []json.RawMessage
	if err := c.CallJSON(ctx, &out, "iscsi.global.sessions"); err != nil {
		return 0, err
	}
	return len(out), nil
}
