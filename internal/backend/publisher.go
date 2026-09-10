package backend

import (
	"context"
	"errors"
	"fmt"

	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// NodeRef identifies the node an access grant applies to.
//
// Addrs is what NFS and SMB grants are written in terms of. IQN and NQN may be
// empty: the iSCSI backend's shared-target design does not need an initiator
// name, and an NVMe-oF volume is fenced by unbinding its subsystem from the
// transport port, which works whether or not the host NQN is known.
type NodeRef struct {
	ID    string   // CSI node ID as ControllerPublishVolume receives it
	Addrs []string // node IP addresses, for share host access lists
	IQN   string   // iSCSI initiator name, when known
	NQN   string   // NVMe host NQN, when known
}

// Publisher grants and revokes appliance-side access per node. Unpublish is the
// fence: after it returns the named node must not reach the volume's data,
// whatever state its kernel is in.
//
// Publish is idempotent — publishing a volume already published to the same
// node must converge on the same appliance state rather than fail or duplicate
// it — and Unpublish must succeed for a node that never held a grant.
type Publisher interface {
	Publish(ctx context.Context, id volume.ID, node NodeRef) (map[string]string, error)
	Unpublish(ctx context.Context, id volume.ID, node NodeRef) error
}

// ErrNodeNotFound means the CSI node id names no node this driver can resolve.
var ErrNodeNotFound = errors.New("node is not known to this cluster")

// ErrNotFenceable means a grant cannot be expressed for this node, so
// publishing would produce access the fence could not later revoke.
var ErrNotFenceable = errors.New("access for this node cannot be granted in a revocable form")

// ErrVolumeGone means the dataset behind a volume id is not there. Unpublish
// treats it as success — a volume that does not exist is fenced.
var ErrVolumeGone = errors.New("volume does not exist")

// ErrShrinkNotAllowed means an expansion asked for less than the volume already
// has. It lives here rather than in each backend so the CSI layer can map it
// once: a shrink is permanently impossible, and reporting it as Internal made
// the external-resizer retry it for ever.
var ErrShrinkNotAllowed = errors.New("volume cannot be shrunk")

// ReadGrants reads a volume's publish ledger from the appliance.
//
// A volume with no ledger — every volume provisioned before this driver kept
// one — reads as an empty ledger rather than an error, so the first publish
// after an upgrade simply starts the record.
func ReadGrants(ctx context.Context, c truenas.API, id volume.ID) (volume.Grants, error) {
	ds, err := c.DatasetQuery(ctx, id.DatasetPath())
	if err != nil {
		return nil, fmt.Errorf("querying %s: %w", id.DatasetPath(), err)
	}
	if ds == nil {
		return nil, fmt.Errorf("%w: %s", ErrVolumeGone, id)
	}
	return volume.DecodeGrants(ds.LocalProperty(volume.PublishedProperty))
}

// SaveGrants writes a volume's publish ledger back to the appliance.
func SaveGrants(ctx context.Context, c truenas.API, id volume.ID, g volume.Grants) error {
	encoded, err := g.Encode()
	if err != nil {
		return err
	}
	if err := c.SetUserProperty(ctx, id.DatasetPath(), volume.PublishedProperty, encoded); err != nil {
		return fmt.Errorf("recording the publish ledger on %s: %w", id.DatasetPath(), err)
	}
	return nil
}
