package config

import (
	"fmt"
	"strings"
	"time"
)

// DefaultGraveyardDataset is the dataset name retired volumes are moved into,
// relative to the backend's parentDataset.
//
// The leading dot is deliberate and was checked against OpenZFS rather than
// assumed. entity_namecheck() permits any component made of
// [A-Za-z0-9_.: -] and rejects only "." and ".." as WHOLE components; the
// "must begin with a letter" rule (NAME_ERR_NOLETTER) applies to POOL names
// alone. TrueNAS relies on this itself for its own "<pool>/.system" dataset.
//
// The dot is what makes a collision structurally impossible rather than
// unlikely: a Kubernetes namespace is a DNS-1123 label and a PersistentVolume
// name a DNS-1123 subdomain, and neither may begin with a dot — so no namespace
// dataset and no volume dataset can ever be named this.
const DefaultGraveyardDataset = ".trash"

// DeleteProtection is an optional grace period between DeleteVolume and the
// destruction of the data.
//
// When it is on, DeleteVolume tears the volume's shares, extents and target
// mappings down exactly as it does today and then RENAMES the dataset into
// <pool>/<parentDataset>/<graveyardDataset>/ instead of destroying it. A reaper
// destroys graveyard datasets once their grace period has expired.
//
// THE TRADE, WHICH IS NOT A BUG: capacity is NOT reclaimed during the grace
// period. Deleting ten PVCs to free space leaves the pool exactly as full as it
// was for gracePeriod, and the space keeps counting against the pool reserve
// (reservedBytes/reservedPercent) and against what GetCapacity reports to the
// scheduler, because it is genuinely still allocated. Per-namespace accounting
// is the one exception: a retired dataset leaves its namespace dataset, so a
// namespace's `zfs used` does drop immediately.
//
// It is OFF by default. Turning it on changes what `kubectl delete pvc` means,
// and nobody should get that without asking for it.
type DeleteProtection struct {
	// Enabled turns delete protection on for this appliance.
	Enabled bool `yaml:"enabled"`

	// GracePeriod is how long a retired volume is kept before the reaper may
	// destroy it, as a Go duration ("168h" for a week).
	//
	// Empty or "0" means no protection AT ALL: DeleteVolume takes today's code
	// path and destroys the dataset, because a grace period of zero and the
	// feature being off are the same request, and a graveyard whose contents
	// are immediately reapable would be a slower way to lose the same data.
	GracePeriod string `yaml:"gracePeriod"`

	// GraveyardDataset is the name of the dataset retired volumes are moved
	// into, relative to parentDataset. Empty means DefaultGraveyardDataset.
	GraveyardDataset string `yaml:"graveyardDataset"`

	// ReapInterval is how often the reaper looks for expired graveyard
	// datasets, as a Go duration. Empty means DefaultReapInterval.
	//
	// It only bounds how long past its grace period a dataset survives; it is
	// never what decides whether one may be destroyed.
	ReapInterval string `yaml:"reapInterval"`
}

// DefaultReapInterval is used when reapInterval is unset. It is deliberately
// unhurried: nothing goes wrong if a dataset is destroyed an hour after its
// grace period rather than a second after, and a sweep is a query against an
// appliance with a small concurrency budget.
const DefaultReapInterval = time.Hour

// Grace is the configured grace period, or zero when delete protection is off.
//
// Zero is the single answer to "disabled", "unset" and "gracePeriod: 0s", so
// every caller has exactly one condition to test and cannot accidentally
// implement a third meaning. validate() has already rejected an unparsable
// value, so the zero here only covers a Config built in code.
func (d DeleteProtection) Grace() time.Duration {
	if !d.Enabled {
		return 0
	}
	s := strings.TrimSpace(d.GracePeriod)
	if s == "" {
		return 0
	}
	v, err := time.ParseDuration(s)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// Graveyard is the configured graveyard dataset name, defaulted.
func (d DeleteProtection) Graveyard() string {
	if s := strings.TrimSpace(d.GraveyardDataset); s != "" {
		return s
	}
	return DefaultGraveyardDataset
}

// Reap is the configured reaper interval, defaulted.
func (d DeleteProtection) Reap() time.Duration {
	s := strings.TrimSpace(d.ReapInterval)
	if s == "" {
		return DefaultReapInterval
	}
	v, err := time.ParseDuration(s)
	if err != nil || v <= 0 {
		return DefaultReapInterval
	}
	return v
}

// Equal reports whether two delete-protection configurations are identical,
// which is what the reload check needs.
func (d DeleteProtection) Equal(other DeleteProtection) bool {
	return d.Enabled == other.Enabled &&
		d.Grace() == other.Grace() &&
		d.Graveyard() == other.Graveyard() &&
		d.Reap() == other.Reap()
}

// String renders the configuration for a log line.
func (d DeleteProtection) String() string {
	if d.Grace() <= 0 {
		return "DeleteProtection{disabled}"
	}
	return fmt.Sprintf("DeleteProtection{grace=%s graveyard=%s reap=%s}",
		d.Grace(), d.Graveyard(), d.Reap())
}

// validate rejects a configuration that could not be honoured.
func (d DeleteProtection) validate() error {
	for _, f := range []struct {
		name, value string
	}{
		{"gracePeriod", d.GracePeriod},
		{"reapInterval", d.ReapInterval},
	} {
		if s := strings.TrimSpace(f.value); s != "" {
			v, err := time.ParseDuration(s)
			if err != nil || v < 0 {
				return fmt.Errorf("deleteProtection.%s %q is not a Go duration (e.g. 168h)", f.name, f.value)
			}
		}
	}
	if err := validGraveyardName(d.GraveyardDataset); err != nil {
		return fmt.Errorf("deleteProtection.graveyardDataset: %w", err)
	}
	if !d.Enabled {
		// Silently ignoring a grace period an operator took the trouble to
		// write is how a cluster ends up with no protection and someone
		// believing there is a week of it. This is the same refusal
		// namespaceQuotas makes, for the same reason.
		if strings.TrimSpace(d.GracePeriod) != "" || strings.TrimSpace(d.GraveyardDataset) != "" ||
			strings.TrimSpace(d.ReapInterval) != "" {
			return fmt.Errorf("deleteProtection is configured but enabled is false: " +
				"set deleteProtection.enabled to true, or remove the settings")
		}
		return nil
	}
	if d.Grace() <= 0 {
		return fmt.Errorf("deleteProtection.enabled is true but gracePeriod %q is not a positive "+
			"duration: a zero grace period destroys the volume immediately, which is what "+
			"leaving deleteProtection off already does", d.GracePeriod)
	}
	return nil
}

// maxGraveyardName bounds the name so the full retired-dataset path stays well
// inside ZFS_MAX_DATASET_NAME_LEN (256) once the pool, the parent dataset and a
// timestamped entry name are added to it.
const maxGraveyardName = 64

// validGraveyardName checks that the name is a legal ZFS dataset component and
// cannot be confused with a namespace or a volume.
//
// The charset is OpenZFS's valid_char() minus the space, which ZFS permits and
// nothing here needs; "." and ".." are what entity_namecheck() itself rejects.
// The empty string is accepted because it means "use the default".
func validGraveyardName(name string) error {
	if name == "" {
		return nil
	}
	if len(name) > maxGraveyardName {
		return fmt.Errorf("%q is %d characters, limit %d", name, len(name), maxGraveyardName)
	}
	switch name {
	case ".", "..":
		return fmt.Errorf("%q is not a dataset name ZFS accepts", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':':
		default:
			return fmt.Errorf("%q contains %q, which is not legal in a ZFS dataset name", name, string(r))
		}
	}
	return nil
}
