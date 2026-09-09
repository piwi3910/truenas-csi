package volume

import (
	"fmt"
	"time"
)

// The ZFS user properties that mark delete protection's two kinds of dataset.
//
// They live here, beside the ownership marker, for the same reason it does:
// every reader of appliance state has to agree on what they mean, and the
// readers are spread across the CSI service, the orphan reporter and the
// reaper. Only internal/retention writes them.
const (
	// ZFS user property NAMES MUST BE LOWERCASE. zfs accepts only lowercase
	// letters, digits and ":-._" in the part after the namespace, and answers
	// anything else with
	//
	//	cannot set property for '<dataset>': invalid property '<name>'
	//
	// These two were written in camelCase and every unit test passed, because
	// the fake stores whatever key it is handed. On the appliance the stamping
	// failed, the retired dataset carried no timestamp, and the reaper — which
	// refuses anything it cannot date — would have kept it for ever. Verified
	// against 25.10; hyphens are the readable spelling that is actually legal.

	// GraveyardProperty marks the graveyard dataset itself — the container
	// retired volumes are renamed into. Its presence with source LOCAL is what
	// says "this is the graveyard root", never "this is a retired volume".
	GraveyardProperty = "io.truenas.csi:graveyard"

	// DeletedAtProperty records, as an RFC 3339 UTC timestamp, when
	// DeleteVolume retired the dataset. It is the clock the grace period is
	// measured from and, because it is stamped on the dataset rather than held
	// in the driver, it survives a controller restart, a rescheduled pod and a
	// driver upgrade.
	DeletedAtProperty = "io.truenas.csi:deleted-at"

	// RetiredFromProperty records the CSI volume handle the dataset served
	// before it was retired.
	//
	// The graveyard name cannot carry that: a volume handle contains "/" and a
	// ZFS name component cannot. Recording it is what lets an operator answer
	// "which PVC was this?" from the appliance alone, and it is the only
	// evidence that survives once the PersistentVolume is gone.
	RetiredFromProperty = "io.truenas.csi:retired-from"
)

// GraveyardValue is the value GraveyardProperty carries. It is a constant
// rather than the graveyard's own name so that renaming the graveyard in
// configuration does not orphan the datasets already in it.
const GraveyardValue = "truenas-csi"

// IsGraveyard reports whether a dataset's LOCAL GraveyardProperty marks it as
// the graveyard root.
//
// It takes the already-resolved local value rather than the dataset because the
// LOCAL rule is the whole point: every retired volume inside the graveyard
// INHERITS this property, and a presence-only check would call each of them a
// graveyard root — which is exactly the mistake that would let a reaper destroy
// the container instead of its contents.
func IsGraveyard(localGraveyardProperty string) bool {
	return localGraveyardProperty == GraveyardValue
}

// IsRetired reports whether a dataset carries a LOCAL deletion timestamp and is
// therefore a retired volume awaiting its grace period, not a live one.
//
// Everything that turns appliance state back into volume handles — ListVolumes,
// the orphan report — must skip these. A retired dataset sits at a depth and a
// path this driver never provisions into, so a handle derived from it would
// name a volume that does not exist, and DeleteVolume on that handle would be
// asked to destroy something the CO never created.
func IsRetired(localDeletedAt string) bool { return localDeletedAt != "" }

// FormatDeletedAt renders a deletion time for DeletedAtProperty.
//
// UTC and RFC 3339 are not cosmetic: the value is compared against the reaper's
// clock, and the appliance, the controller and whoever reads the property by
// hand are three different machines with three different local zones.
func FormatDeletedAt(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// ParseDeletedAt decodes DeletedAtProperty.
//
// An unparsable value is an error rather than "assume it is old": the grace
// period is the only thing standing between a retired dataset and destruction,
// and a value the driver cannot read is a value it must not act on.
func ParseDeletedAt(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("no %s property", DeletedAtProperty)
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s=%q is not an RFC 3339 timestamp: %w", DeletedAtProperty, s, err)
	}
	return t.UTC(), nil
}
