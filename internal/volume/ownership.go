package volume

import (
	"errors"
	"fmt"
)

const (
	// OwnerProperty is the ZFS user property this driver stamps on every dataset
	// it creates.
	OwnerProperty = "io.truenas.csi:managed"

	// OwnerValue is the value OwnerProperty must carry for a dataset to count as
	// driver-owned.
	OwnerValue = "truenas-csi"

	// OwnerIDProperty carries the id of the dataset the marker was set on.
	//
	// It exists because the Source field cannot be trusted to mean what it says.
	// Verified against 25.10.6: pool.dataset.query reports EVERY user property
	// with source "LOCAL", inherited or not, so a dataset created by hand
	// underneath a marked one is indistinguishable from one this driver created.
	// (zfs.dataset.query does distinguish them, but only at the cost of a second
	// middleware call on every query, against an appliance with a 20-call
	// concurrency ceiling.)
	//
	// A self-identifying marker needs no source at all: a child inherits its
	// ANCESTOR's id, which is not its own, so inheritance is visible in the
	// value. That also means no mock can be more permissive than the appliance
	// here — the evidence is the data itself rather than a field the middleware
	// fills in.
	OwnerIDProperty = "io.truenas.csi:owner-id"

	// sourceLocal is the value TrueNAS reports for a property set on the dataset
	// itself, as opposed to "INHERITED" from an ancestor or "DEFAULT".
	sourceLocal = "LOCAL"
)

// ErrNotManaged is returned when a dataset does not carry proof that this driver
// created it. Every destructive middleware call is gated on this.
var ErrNotManaged = errors.New("dataset is not managed by this driver — refusing to delete")

// Property is one ZFS user property as TrueNAS reports it: the value plus where
// it came from.
type Property struct {
	Value  string
	Source string
}

// Dataset is the minimal view of a TrueNAS dataset the ownership guard needs.
//
// internal/truenas will produce a compatible type from pool.dataset.query; this
// package deliberately declares its own so the safety guard has no dependency on
// the middleware client and can be tested without one.
type Dataset struct {
	ID             string
	UserProperties map[string]Property
}

// VerifyOwned reports whether ds was created by this driver, and must be called
// before any destructive operation on it.
//
// The Source check is the whole point. ZFS user properties are inherited by
// children, so if the operator-configured parent dataset ever carried the marker
// — set by hand, or by an earlier install — every pre-existing dataset beneath it
// would report the marker and a presence-only check would clear the driver to
// destroy the operator's real data. Only "LOCAL" means "set on this dataset".
func VerifyOwned(ds *Dataset) error {
	if ds == nil {
		return fmt.Errorf("%w: no dataset was returned", ErrNotManaged)
	}
	prop, ok := ds.UserProperties[OwnerProperty]
	if !ok {
		return fmt.Errorf("%w: %q has no %s property", ErrNotManaged, ds.ID, OwnerProperty)
	}
	if prop.Value != OwnerValue {
		return fmt.Errorf("%w: %q has %s=%q, want %q", ErrNotManaged, ds.ID, OwnerProperty, prop.Value, OwnerValue)
	}
	// The strong check, when the dataset carries it: the marker names the
	// dataset it was set on, so an inherited one names an ancestor instead.
	if id, ok := ds.UserProperties[OwnerIDProperty]; ok {
		if id.Value != ds.ID {
			return fmt.Errorf("%w: %q carries %s=%q, which names a different dataset — "+
				"the marker is inherited from an ancestor, not set here",
				ErrNotManaged, ds.ID, OwnerIDProperty, id.Value)
		}
		return nil
	}

	// Legacy datasets, stamped before OwnerIDProperty existed, have only the
	// source to go on. It is kept so an upgrade does not orphan every existing
	// volume, and it is no weaker than what those datasets have always had — but
	// it cannot detect inheritance on TrueNAS, which is why every dataset this
	// driver stamps from now on carries the id as well.
	if prop.Source != sourceLocal {
		return fmt.Errorf("%w: %q has %s with source %q, want %q — the marker is inherited, not owned",
			ErrNotManaged, ds.ID, OwnerProperty, prop.Source, sourceLocal)
	}
	return nil
}

// StampProperties returns the user_properties payload for pool.dataset.create,
// which is how a dataset acquires the marker at birth.
//
// datasetID is stamped alongside the marker so ownership can be established
// from the value rather than from a source field the middleware does not report
// faithfully. An empty id still produces a usable marker, for callers that do
// not know the final path, but such a dataset falls back to the weaker legacy
// check — so pass it wherever it is known.
func StampProperties(datasetID string) []map[string]string {
	out := []map[string]string{{
		"key":   OwnerProperty,
		"value": OwnerValue,
	}}
	if datasetID != "" {
		out = append(out, map[string]string{"key": OwnerIDProperty, "value": datasetID})
	}
	return out
}

// Stamping an existing dataset — Stamp(ctx, client, datasetID) issuing
// pool.dataset.update with user_properties_update — is deliberately left to a
// later task: it needs the internal/truenas client, which does not exist yet.
// It is required for clones, which inherit neither the marker nor refquota from
// their origin, and would otherwise be undeletable by this guard.

// ProtocolProperty records which protocol serves a volume.
//
// The dataset alone cannot say: an iSCSI volume and an NVMe-oF volume are both
// zvols, and SMB and NFS are both filesystems. Anything reconstructing a volume
// id from appliance state — ListVolumes, the orphan reconciler — would
// otherwise have to guess, produce an id matching no PersistentVolume, and
// report live volumes as orphans.
const ProtocolProperty = "io.truenas.csi:protocol"

// ProtocolOr returns the recorded protocol, or the caller's fallback when the
// volume predates this property.
//
// Volumes created before the protocol was recorded carry no value, so the
// caller supplies the historical inference rather than getting an empty
// protocol that resolves to nothing.
func ProtocolOr(recorded, fallback string) string {
	if recorded == "" {
		return fallback
	}
	return recorded
}
