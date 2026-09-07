package backend

import (
	"context"
	"strings"
	"time"

	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Snapshot is a point-in-time copy of a volume.
type Snapshot struct {
	ID             string
	SourceVolumeID string
	SizeBytes      int64
	CreationTime   time.Time
	ReadyToUse     bool
}

// CreateSnapshot snapshots a volume. ZFS snapshots are atomic and instant, so
// the result is ready to use immediately.
func (r *Registry) CreateSnapshot(ctx context.Context, source volume.ID, name string) (*Snapshot, error) {
	c, err := r.Client(ctx, source.Backend)
	if err != nil {
		return nil, err
	}
	ds := source.DatasetPath()
	id := ds + "@" + name

	if existing, err := c.SnapshotQuery(ctx, id); err == nil && existing != nil {
		return &Snapshot{ID: snapshotID(source.Backend, id), SourceVolumeID: source.String(),
			ReadyToUse: true}, nil
	}
	if _, err := c.SnapshotCreate(ctx, ds, name); err != nil {
		return nil, err
	}
	return &Snapshot{
		ID: snapshotID(source.Backend, id), SourceVolumeID: source.String(),
		CreationTime: time.Now(), ReadyToUse: true,
	}, nil
}

// DeleteSnapshot removes a snapshot.
//
// A snapshot with dependent clones cannot be deleted, and returns
// FailedPrecondition as the CSI spec prescribes. Promoting the clone to break
// the dependency is deliberately NOT done: promotion inverts the relationship
// and leaves the SOURCE volume undeletable, which is worse — users delete PVCs
// far more often than snapshots.
func (r *Registry) DeleteSnapshot(ctx context.Context, id string) error {
	backendName, snapID, err := parseSnapshotID(id)
	if err != nil {
		return err
	}
	c, err := r.Client(ctx, backendName)
	if err != nil {
		return err
	}
	snap, err := c.SnapshotQuery(ctx, snapID)
	if err != nil {
		return err
	}
	if snap == nil {
		return nil // already gone: success, per CSI
	}
	clones, err := r.dependentClones(ctx, c, snapID)
	if err != nil {
		return err
	}
	if len(clones) > 0 {
		return status.Errorf(codes.FailedPrecondition,
			"snapshot %s still has dependent volume(s) %v; delete them first", id, clones)
	}
	return c.SnapshotDelete(ctx, snapID)
}

// dependentClones finds datasets whose origin is this snapshot.
func (r *Registry) dependentClones(ctx context.Context, c *truenas.Client, snapID string) ([]string, error) {
	pool := snapID
	if i := strings.Index(pool, "/"); i > 0 {
		pool = pool[:i]
	}
	all, err := c.DatasetList(ctx, pool)
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range all {
		if all[i].Origin.Value == snapID {
			out = append(out, all[i].ID)
		}
	}
	return out, nil
}

// ListSnapshots returns snapshots under a backend's parent dataset.
func (r *Registry) ListSnapshots(ctx context.Context, backendName string) ([]Snapshot, error) {
	cfgB, err := r.Backend(backendName)
	if err != nil {
		return nil, err
	}
	c, err := r.Client(ctx, backendName)
	if err != nil {
		return nil, err
	}
	prefix := cfgB.Pool + "/" + cfgB.ParentDataset
	snaps, err := c.SnapshotList(ctx, prefix)
	if err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, Snapshot{
			ID: snapshotID(backendName, s.ID), SourceVolumeID: s.Dataset, ReadyToUse: true,
		})
	}
	return out, nil
}

// snapshotID prefixes the ZFS snapshot id with its appliance, so a snapshot can
// be resolved without guessing which appliance holds it.
func snapshotID(backendName, zfsID string) string { return backendName + "/" + zfsID }

func parseSnapshotID(id string) (backendName, zfsID string, err error) {
	i := strings.Index(id, "/")
	if i <= 0 || i == len(id)-1 {
		return "", "", status.Errorf(codes.InvalidArgument,
			"malformed snapshot id %q, want <backend>/<dataset>@<name>", id)
	}
	backendName, zfsID = id[:i], id[i+1:]
	if !strings.Contains(zfsID, "@") {
		return "", "", status.Errorf(codes.InvalidArgument,
			"malformed snapshot id %q: missing @<name>", id)
	}
	return backendName, zfsID, nil
}

// SnapshotSource turns a CSI snapshot id into the arguments a backend needs.
func SnapshotSource(id string) (backendName, zfsID string, err error) {
	return parseSnapshotID(id)
}
