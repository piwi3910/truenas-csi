package volume

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ZFS user properties that mark a namespace's parent dataset.
//
// The dataset carries the ordinary ownership marker too, so the existing
// VerifyOwned guard applies to it unchanged. These two say what KIND of thing
// it is, which the ownership marker alone cannot: a namespace dataset holds no
// volume data of its own and must never be reported as a volume, published,
// snapshotted as one, or deleted as one.
const (
	// NamespaceProperty holds the Kubernetes namespace this dataset accounts
	// for. Its presence with source LOCAL is what identifies the dataset as a
	// namespace container rather than a volume.
	NamespaceProperty = "io.truenas.csi:namespace"

	// NamespaceQuotaProperty records, in bytes, the ZFS quota the driver last
	// applied to the dataset.
	//
	// It exists because the driver cannot read the quota back: the middleware's
	// dataset view this driver consumes does not carry it. Recording what was
	// applied is what lets a later CreateVolume tell "the configured quota is
	// already in force" from "the operator changed it", so the common path
	// costs a query and no write. "0" means deliberately unlimited.
	NamespaceQuotaProperty = "io.truenas.csi:nsquota"
)

// ErrInvalidNamespace means a string that would become a dataset path component
// is not a name Kubernetes could have produced.
var ErrInvalidNamespace = errors.New("not a valid Kubernetes namespace")

// maxNamespaceLength is the DNS-1123 label limit, which is what a Kubernetes
// namespace is.
const maxNamespaceLength = 63

// ValidateNamespace checks that ns is safe to use as a dataset path component.
//
// This is the security boundary for the whole feature. The namespace arrives in
// CreateVolumeRequest.Parameters, an untrusted map that anyone with StorageClass
// edit rights can also write by hand, and it is then concatenated into a ZFS
// path the driver creates and later deletes. Sanitising it — the approach
// Identity takes for the diagnostic properties — is wrong here: two namespaces
// that sanitise to the same string would share a quota and a dataset. So this
// refuses anything that is not already a DNS-1123 label, and the caller falls
// back to the flat layout rather than inventing a name.
func ValidateNamespace(ns string) error {
	if ns == "" {
		return fmt.Errorf("%w: empty", ErrInvalidNamespace)
	}
	if len(ns) > maxNamespaceLength {
		return fmt.Errorf("%w: %d characters, limit %d", ErrInvalidNamespace, len(ns), maxNamespaceLength)
	}
	for i, r := range ns {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(ns)-1:
		default:
			return fmt.Errorf("%w: %q contains %q", ErrInvalidNamespace, ns, string(r))
		}
	}
	return nil
}

// NamespaceProperties returns the user properties a namespace dataset is
// created with: the ownership marker, so the existing guard protects it, plus
// the namespace marker.
func NamespaceProperties(ns string) map[string]string {
	return map[string]string{
		OwnerProperty:     OwnerValue,
		NamespaceProperty: ns,
	}
}

// IsNamespaceDataset reports whether a dataset's local user properties mark it
// as a namespace container.
//
// LocalProperty semantics matter as much here as they do for ownership: the
// marker is inherited by every volume dataset beneath it, so a presence-only
// check would classify every volume in the namespace as a container.
func IsNamespaceDataset(localNamespaceProperty string) bool {
	return localNamespaceProperty != ""
}

// AppliedQuota decodes the recorded quota, returning (0, false) when nothing
// was ever recorded — a dataset created by an older release, or one whose
// property an operator removed. Unreadable is treated as unrecorded so the next
// CreateVolume rewrites it rather than refusing to provision.
func AppliedQuota(recorded string) (int64, bool) {
	s := strings.TrimSpace(recorded)
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// FormatQuota renders a quota for NamespaceQuotaProperty.
func FormatQuota(bytes int64) string { return strconv.FormatInt(bytes, 10) }
