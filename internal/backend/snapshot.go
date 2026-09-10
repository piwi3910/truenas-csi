package backend

import (
	"context"
	"strings"
	"time"

	"github.com/piwi3910/truenas-csi/internal/retention"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Snapshot is a point-in-time copy of a volume.
//
// SizeBytes is the PROVISIONED size of the source volume, not the space the
// snapshot itself occupies. That is what the CSI spec asks for and what the
// consumer needs: external-snapshotter copies it into the VolumeSnapshot's
// status.restoreSize, and external-provisioner then refuses a restore claim
// smaller than it. Reporting the snapshot's own (near-zero) referenced bytes
// would let a 1Gi claim restore a 3Gi volume and silently truncate nothing —
// the restore would succeed and the PV would lie about its capacity.
type Snapshot struct {
	ID             string
	SourceVolumeID string
	SizeBytes      int64
	CreationTime   time.Time
	ReadyToUse     bool
}

// provisionedBytes is the source volume's provisioned size for a snapshot.
//
// A zvol snapshot carries volsize, so it answers on its own. A filesystem
// snapshot carries no quota property at all, so its live dataset is queried
// instead. Neither is fatal when it fails: a zero size costs the restore-size
// check, which is better than failing a snapshot that ZFS already took.
func provisionedBytes(ctx context.Context, c truenas.API, snap *truenas.Snapshot, dataset string) int64 {
	if snap != nil && snap.Properties.VolSize.Parsed > 0 {
		return snap.Properties.VolSize.Parsed
	}
	ds, err := c.DatasetQuery(ctx, dataset)
	if err != nil || ds == nil {
		return 0
	}
	if ds.VolSize.Parsed > 0 {
		return ds.VolSize.Parsed
	}
	return ds.RefQuota.Parsed
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

	// The re-call path reports the SAME creation time and size as the first
	// call, read back off the appliance. CSI requires CreateSnapshot to be
	// idempotent, and a sidecar that saw a different creation time on a retry
	// would treat it as a different snapshot.
	if existing, err := c.SnapshotQuery(ctx, id); err == nil && existing != nil {
		return &Snapshot{ID: snapshotID(source.Backend, id), SourceVolumeID: source.String(),
			SizeBytes:    provisionedBytes(ctx, c, existing, ds),
			CreationTime: existing.CreationTime(), ReadyToUse: true}, nil
	}
	created, err := c.SnapshotCreate(ctx, ds, name)
	if err != nil {
		return nil, err
	}
	// pool.snapshot.create does not return the property block, so the creation
	// time is read back. Falling back to the local clock keeps a snapshot that
	// exists from being reported as failed over a cosmetic field.
	when := time.Now().UTC()
	snap, err := c.SnapshotQuery(ctx, id)
	if err != nil || snap == nil {
		snap = created
	} else if t := snap.CreationTime(); !t.IsZero() {
		when = t
	}
	return &Snapshot{
		ID: snapshotID(source.Backend, id), SourceVolumeID: source.String(),
		SizeBytes: provisionedBytes(ctx, c, snap, ds), CreationTime: when, ReadyToUse: true,
	}, nil
}

// Snapshot returns one snapshot by its driver id, or (nil, nil) when it does
// not exist. It fills in the same size and creation time CreateSnapshot
// reports, so ListSnapshots for a single id and the create response agree.
func (r *Registry) Snapshot(ctx context.Context, id string) (*Snapshot, error) {
	backendName, zfsID, err := parseSnapshotID(id)
	if err != nil {
		return nil, err
	}
	c, err := r.Client(ctx, backendName)
	if err != nil {
		return nil, err
	}
	snap, err := c.SnapshotQuery(ctx, zfsID)
	if err != nil || snap == nil {
		return nil, err
	}
	return &Snapshot{
		ID: id, SourceVolumeID: snap.Dataset,
		SizeBytes:    provisionedBytes(ctx, c, snap, snap.Dataset),
		CreationTime: snap.CreationTime(), ReadyToUse: true,
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
func (r *Registry) dependentClones(ctx context.Context, c truenas.API, snapID string) ([]string, error) {
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
		if all[i].OriginSnapshot() == snapID {
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
	// One listing of the source datasets, so filesystem snapshots can report a
	// size without a query each. A failure here is not fatal: it costs the
	// size, not the listing.
	sources := map[string]truenas.Dataset{}
	if all, err := c.DatasetList(ctx, prefix); err == nil {
		for i := range all {
			sources[all[i].ID] = all[i]
		}
	}

	graveyard := retention.PolicyFor(cfgB)
	out := make([]Snapshot, 0, len(snaps))
	for i, s := range snaps {
		// A retired volume's snapshots travel with it into the graveyard — ZFS
		// renames a dataset's snapshots along with the dataset. Listing them
		// would report snapshots whose source volume no longer exists, so they
		// are omitted; the recursive destroy at the end of the grace period
		// takes them with the dataset.
		if graveyard.On() && graveyard.ConfineToGraveyard(s.Dataset) == nil {
			continue
		}
		size := snaps[i].Properties.VolSize.Parsed
		if size == 0 {
			// A filesystem snapshot carries no quota, so the size comes from
			// the live dataset — looked up in the map rather than with a call
			// per snapshot, which would make listing cost O(snapshots).
			if ds, ok := sources[s.Dataset]; ok {
				size = ds.RefQuota.Parsed
			}
		}
		out = append(out, Snapshot{
			ID: snapshotID(backendName, s.ID), SourceVolumeID: s.Dataset,
			SizeBytes: size, CreationTime: s.CreationTime(), ReadyToUse: true,
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
