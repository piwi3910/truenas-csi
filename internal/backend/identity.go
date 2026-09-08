package backend

import (
	"context"

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
	// format namespace:property is the one the middleware's own schema requires
	// (verified against https://192.168.10.253/api/docs/current/, API v25.10.5;
	// note that pool.dataset.query returns comments as a SIBLING of
	// user_properties, not inside it — nothing here reads it back, but anything
	// that later does must look in the right place).
	//
	// UNVERIFIED: sending comments and user_properties_update in one update has
	// not been run against the appliance — no API key was available — only
	// checked against the published schema. A hardware run should confirm both
	// land, and that a failure here is the harmless no-op this treats it as.
	if _, err := c.DatasetUpdate(ctx, dsPath, map[string]any{
		"user_properties_update": updates,
		"comments":               id.Description(),
	}); err != nil {
		obs.Logger(ctx).Warn("recording the PVC identity failed; the volume is fine but unlabelled",
			"dataset", dsPath, "error", err)
	}
}
