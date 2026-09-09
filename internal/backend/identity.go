package backend

import (
	"context"
	"fmt"

	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// RecordIdentity stamps the PVC identity of a freshly provisioned volume onto
// its dataset or zvol, as user properties plus a human-readable comment.
//
// It is called once, at birth, by every backend and on both the empty-create
// and the clone paths. A clone gets the identity of the request that created
// IT: a ZFS clone takes its properties from its position in the hierarchy
// rather than from its origin (verified on TrueNAS 25.10.6 — a clone inherits
// neither the ownership marker nor refquota), so nothing of the source's
// identity survives, and the new identity is written explicitly rather than
// relied upon to be absent.
//
// It deliberately does not run for a volume that already exists. CreateVolume
// is retried, and CSI's idempotency contract is about capacity and access
// capabilities, not about labels: a repeat with the same parameters would
// rewrite identical values for nothing, and a repeat with DIFFERENT ones means
// the same volume handle is being claimed by a second PVC — a static-PV
// misconfiguration or a rebind. Overwriting there would erase the record of who
// created the dataset at the exact moment that record becomes the thing an
// operator needs, and failing there would make a cosmetic label a reason to
// refuse an otherwise valid volume. So the first writer wins and the mismatch
// is left visible.
//
// Failure is logged, not returned. The properties are diagnostic; a volume
// whose label could not be written is a working volume, and rolling back a
// successfully provisioned dataset over a missing description would turn a
// cosmetic problem into a provisioning outage.
func RecordIdentity(ctx context.Context, c truenas.API, dsPath string, id volume.Identity) {
	if id.Empty() {
		// The normal case for csi-sanity, static provisioning, and any
		// deployment without --extra-create-metadata. Not an error, and not
		// worth a middleware round trip.
		return
	}

	props := id.Properties()
	updates := make([]map[string]string, 0, len(props))
	for _, k := range id.PropertyKeys() {
		updates = append(updates, map[string]string{"key": k, "value": props[k]})
	}

	// One call carries both: user_properties_update for the machine-readable
	// record and comments for the field the UI actually shows.
	//
	// Both are fields of the same pool.dataset.update `data` object, and the key
	// format namespace:property is the one the middleware's own schema requires.
	//
	// VERIFIED against the live appliance (25.10.6, 2026-09-08): one update
	// carrying both is accepted, and both land with source=LOCAL. Note the
	// read-back path, which is the opposite of what the schema suggests:
	// pool.dataset.query returns the top-level `comments` field as null and
	// surfaces the comment INSIDE `user_properties` as `user_properties.comments`.
	// Nothing here reads it back, but anything that later does must look there.
	//
	// A failure is treated as a harmless no-op: the volume is provisioned and
	// usable, and a diagnostic label is not worth rolling that back.
	if _, err := c.DatasetUpdate(ctx, dsPath, map[string]any{
		"user_properties_update": updates,
		"comments":               id.Description(),
	}); err != nil {
		obs.Logger(ctx).Warn("recording the PVC identity failed; the volume is fine but unlabelled",
			"dataset", dsPath, "error", err)
	}
}

// StampClone writes the markers a ZFS clone does not inherit: the ownership
// marker, the owner id, and the protocol.
//
// A clone takes its properties from its POSITION in the hierarchy, never from
// its origin, so a freshly cloned volume arrives carrying none of them. The
// ownership marker and owner id were already written by hand on every clone
// path; the protocol was not, and it has two readers that silently guess when
// it is absent -- the orphan reconciler and per-protocol capacity accounting.
// Both guess from the dataset type alone, so a FILESYSTEM became "nfs" and a
// VOLUME became "iscsi": every cloned SMB volume was reported under an nfs
// handle that names nothing, and every cloned NVMe volume under an iscsi one.
func StampClone(ctx context.Context, c truenas.API, dsPath, protocol string) error {
	for _, p := range []struct{ key, value string }{
		{volume.OwnerIDProperty, dsPath},
		{volume.OwnerProperty, volume.OwnerValue},
		{volume.ProtocolProperty, protocol},
	} {
		if err := c.SetUserProperty(ctx, dsPath, p.key, p.value); err != nil {
			return fmt.Errorf("stamping %s on clone %s: %w", p.key, dsPath, err)
		}
	}
	return nil
}

// AbandonedCloneOf reports whether an existing dataset is a clone THIS request
// made and then failed to finish, rather than a volume that genuinely already
// exists or something an operator put there.
//
// All three conditions must hold: the dataset carries no ownership marker, this
// request is a restore, and the dataset's origin is exactly the snapshot this
// request asked to restore. A dataset an operator created at a PV's generated
// path would not be a clone of that particular snapshot, and a volume this
// driver finished would carry the marker.
func AbandonedCloneOf(ds *truenas.Dataset, r CreateRequest) bool {
	if ds == nil || r.SourceSnapshot == "" {
		return false
	}
	if _, marked := ds.UserProperties[volume.OwnerProperty]; marked {
		return false
	}
	// RawValue, never Value: the middleware returns origin's display form
	// UPPERCASED, so comparing Value never matches a real snapshot name.
	return ds.Origin.RawValue == r.SourceSnapshot
}
