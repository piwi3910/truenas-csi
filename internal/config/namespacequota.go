package config

import (
	"fmt"
	"sort"
	"strings"
)

// NamespaceQuotas configures optional per-Kubernetes-namespace capacity
// accounting for one appliance.
//
// When enabled, a volume is provisioned into <pool>/<parentDataset>/<namespace>
// instead of <pool>/<parentDataset>, and that namespace dataset carries a ZFS
// quota. ZFS then enforces the ceiling itself, which is the reason to do it
// this way at all: an accounting ledger only counts what the driver was told
// about, while a dataset quota also holds against a write the driver never saw
// — a pod filling a volume, a snapshot growing, an operator copying data in
// over SSH.
//
// The quota is operator configuration and deliberately NOT read from a
// Kubernetes object. A namespace annotation or a ResourceQuota would be more
// flexible, but both are writable by whoever holds edit rights in the namespace
// being limited, which makes the limit self-service and therefore not a limit;
// honouring them safely would need a watch, RBAC, a cache and an authorisation
// story. Pool headroom (ReservedBytes/ReservedPercent) is already configured
// here for the same reason, and this is the same kind of policy.
type NamespaceQuotas struct {
	// Enabled turns the layout on. It is off by default because switching it
	// on changes where every subsequently created volume lives.
	//
	// Volumes provisioned BEFORE it was enabled stay where they are, directly
	// under parentDataset, and their space is therefore NOT counted against any
	// namespace quota — nothing moves a dataset, and a volume handle is
	// immutable once a PersistentVolume exists. This is accepted rather than
	// worked around: the alternative is rewriting handles Kubernetes has
	// already persisted. An operator who needs the pre-existing volumes counted
	// must migrate them (create a new PVC in the namespace, copy, delete the
	// old one), and until then a namespace's real consumption is its quota plus
	// whatever it held beforehand.
	Enabled bool `yaml:"enabled"`

	// DefaultBytes is the quota applied to a namespace with no explicit entry
	// in PerNamespace. Zero means unlimited: the namespace still gets its own
	// dataset, and therefore its own `zfs used` figure, but no ceiling.
	DefaultBytes int64 `yaml:"defaultBytes"`

	// PerNamespace overrides DefaultBytes for named namespaces. Zero is an
	// explicit "unlimited" override, not an absent entry.
	PerNamespace map[string]int64 `yaml:"perNamespace"`
}

// QuotaFor returns the quota in bytes for a namespace: its override when one
// exists, otherwise the default. Zero means unlimited.
func (q NamespaceQuotas) QuotaFor(namespace string) int64 {
	if v, ok := q.PerNamespace[namespace]; ok {
		return v
	}
	return q.DefaultBytes
}

// Equal reports whether two quota configurations are identical, which is what
// the reload check needs. Maps are compared by content; nil and empty are the
// same configuration.
func (q NamespaceQuotas) Equal(other NamespaceQuotas) bool {
	if q.Enabled != other.Enabled || q.DefaultBytes != other.DefaultBytes {
		return false
	}
	if len(q.PerNamespace) != len(other.PerNamespace) {
		return false
	}
	for k, v := range q.PerNamespace {
		if ov, ok := other.PerNamespace[k]; !ok || ov != v {
			return false
		}
	}
	return true
}

// String renders the configuration for a log line, with namespaces in a stable
// order so two log lines are comparable.
func (q NamespaceQuotas) String() string {
	if !q.Enabled {
		return "NamespaceQuotas{disabled}"
	}
	names := make([]string, 0, len(q.PerNamespace))
	for n := range q.PerNamespace {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", n, q.PerNamespace[n]))
	}
	return fmt.Sprintf("NamespaceQuotas{enabled default=%d overrides=[%s]}",
		q.DefaultBytes, strings.Join(parts, " "))
}

// validate rejects a configuration that could not be honoured.
//
// The namespace keys are checked even though they are operator-written: each
// one becomes a ZFS dataset path component, and a key with a slash in it would
// name a dataset somewhere else entirely.
func (q NamespaceQuotas) validate() error {
	if q.DefaultBytes < 0 {
		return fmt.Errorf("namespaceQuotas.defaultBytes must not be negative, got %d", q.DefaultBytes)
	}
	for ns, v := range q.PerNamespace {
		if err := validNamespaceKey(ns); err != nil {
			return fmt.Errorf("namespaceQuotas.perNamespace: %w", err)
		}
		if v < 0 {
			return fmt.Errorf("namespaceQuotas.perNamespace[%q] must not be negative, got %d", ns, v)
		}
	}
	if !q.Enabled && (q.DefaultBytes > 0 || len(q.PerNamespace) > 0) {
		// Silently ignoring a quota an operator took the trouble to write is
		// how a cluster ends up with no limit and someone believing there is
		// one. Refusing at startup is the only honest answer.
		return fmt.Errorf("namespaceQuotas has quotas configured but enabled is false: " +
			"set namespaceQuotas.enabled to true, or remove the quotas")
	}
	return nil
}

// maxNamespaceKey is the DNS-1123 label limit a Kubernetes namespace obeys.
const maxNamespaceKey = 63

// validNamespaceKey checks a configured namespace name against the same
// DNS-1123 label rule the runtime applies to the namespace CreateVolume
// reports, so a typo that could never match a real namespace is caught at
// startup instead of silently granting the default quota forever.
func validNamespaceKey(ns string) error {
	if ns == "" {
		return fmt.Errorf("namespace name must not be empty")
	}
	if len(ns) > maxNamespaceKey {
		return fmt.Errorf("namespace %q is %d characters, limit %d", ns, len(ns), maxNamespaceKey)
	}
	for i, r := range ns {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(ns)-1:
		default:
			return fmt.Errorf("namespace %q is not a DNS-1123 label (offending character %q)", ns, string(r))
		}
	}
	return nil
}
