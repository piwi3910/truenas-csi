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
	if prop.Source != sourceLocal {
		return fmt.Errorf("%w: %q has %s with source %q, want %q — the marker is inherited, not owned",
			ErrNotManaged, ds.ID, OwnerProperty, prop.Source, sourceLocal)
	}
	return nil
}

// StampProperties returns the user_properties payload for pool.dataset.create,
// which is how a dataset acquires the marker with source LOCAL at birth.
func StampProperties() []map[string]string {
	return []map[string]string{{
		"key":   OwnerProperty,
		"value": OwnerValue,
	}}
}

// Stamping an existing dataset — Stamp(ctx, client, datasetID) issuing
// pool.dataset.update with user_properties_update — is deliberately left to a
// later task: it needs the internal/truenas client, which does not exist yet.
// It is required for clones, which inherit neither the marker nor refquota from
// their origin, and would otherwise be undeletable by this guard.
