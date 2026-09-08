package volume

import (
	"sort"
	"strings"
	"unicode"
)

// StorageClass parameter keys the CSI external-provisioner injects into
// CreateVolume when it is started with --extra-create-metadata, which this
// driver's Helm chart does.
//
// They are ordinary map keys in CreateVolumeRequest.Parameters, indistinguishable
// from operator-authored StorageClass parameters, so nothing guarantees they are
// present, absent, or well formed. Every use here treats them as untrusted.
const (
	ParamPVCName      = "csi.storage.k8s.io/pvc/name"
	ParamPVCNamespace = "csi.storage.k8s.io/pvc/namespace"
	ParamPVName       = "csi.storage.k8s.io/pv/name"
)

// ZFS user properties recording which Kubernetes object a volume was
// provisioned for.
//
// These exist so a human reading the TrueNAS UI or `zfs get all` can tell which
// workload owns a `pvc-<uuid>` dataset. THEY ARE DIAGNOSTIC ONLY: nothing in
// this driver reads them back to make a decision, and nothing should start.
// Dell's equivalent merges PVC labels into StorageClass parameters, so
// relabelling a PVC silently changes array-side behaviour and the array's
// configuration becomes a function of whatever a workload author typed. A
// property the driver never branches on cannot grow that coupling.
const (
	// PVCNameProperty is the name of the PersistentVolumeClaim the volume was
	// provisioned for.
	PVCNameProperty = "io.truenas.csi:pvcname"

	// PVCNamespaceProperty is the namespace of that claim.
	PVCNamespaceProperty = "io.truenas.csi:pvcnamespace"

	// PVNameProperty is the PersistentVolume name, which is also the last
	// component of the dataset path. It is recorded anyway: an operator looking
	// at a restored or migrated dataset gets the Kubernetes object name without
	// having to know how volume handles are built.
	PVNameProperty = "io.truenas.csi:pvname"
)

// maxIdentityValue bounds one recorded value.
//
// A ZFS user property value may be 8 KiB, so the cap is not about ZFS: it is
// about what a legal Kubernetes name can be. A namespace is a DNS-1123 label
// (63 characters) and a claim name a DNS-1123 subdomain (253), so 255 accepts
// every real name while refusing to hand the appliance an unbounded string that
// arrived in a map anyone with StorageClass edit rights can write.
const maxIdentityValue = 255

// maxDescription bounds the human-readable description written to the dataset's
// comments field. Two names, a separator and a label fit comfortably.
const maxDescription = 512

// Identity is the Kubernetes object a volume was provisioned for.
//
// The zero value is the normal state for a volume created by csi-sanity, by a
// static provisioner, by a deployment without --extra-create-metadata, or by a
// release of this driver that predates the recording. Absence is never an
// error.
type Identity struct {
	PVCName      string
	PVCNamespace string
	PVName       string
}

// IdentityFrom extracts the PVC identity from CreateVolume parameters.
//
// Missing keys yield a zero Identity rather than an error, and every value is
// sanitised on the way in: these strings end up as ZFS property values on the
// appliance, and the driver is the last place that can stop a name it did not
// choose from reaching a dataset write.
func IdentityFrom(params map[string]string) Identity {
	return Identity{
		PVCName:      sanitiseIdentityValue(params[ParamPVCName]),
		PVCNamespace: sanitiseIdentityValue(params[ParamPVCNamespace]),
		PVName:       sanitiseIdentityValue(params[ParamPVName]),
	}
}

// Empty reports whether nothing about the claim is known, which is the case the
// caller must treat as ordinary rather than as a fault.
func (i Identity) Empty() bool {
	return i.PVCName == "" && i.PVCNamespace == "" && i.PVName == ""
}

// Properties returns the user properties to stamp, omitting anything unknown.
//
// A property that is absent says "this was not recorded"; a property present
// but empty would say "this volume belongs to a claim with no name", which is
// not a thing, and would still have to be written and stored.
func (i Identity) Properties() map[string]string {
	out := map[string]string{}
	for k, v := range map[string]string{
		PVCNameProperty:      i.PVCName,
		PVCNamespaceProperty: i.PVCNamespace,
		PVNameProperty:       i.PVName,
	} {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// PropertyKeys returns the recorded property names in a stable order, so the
// payload sent to the appliance does not depend on Go's map iteration.
func (i Identity) PropertyKeys() []string {
	props := i.Properties()
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Description renders the identity for the dataset's comments field, which is
// the one place the TrueNAS UI shows free text about a dataset — the Edit
// Dataset pane — and therefore where an operator actually looks.
//
// The user properties above stay authoritative; this is a mirror for human
// eyes. An operator who edits it in the UI changes nothing the driver reads.
func (i Identity) Description() string {
	if i.Empty() {
		return ""
	}
	// A half-known claim is spelled out rather than rendered as "prod/", which
	// reads as a claim whose name is the empty string.
	ns, name := i.PVCNamespace, i.PVCName
	if ns == "" {
		ns = "<unknown>"
	}
	if name == "" {
		name = "<unknown>"
	}
	out := "Kubernetes PVC " + ns + "/" + name
	if i.PVName != "" {
		out += " (" + i.PVName + ")"
	}
	return truncate(out, maxDescription)
}

// sanitiseIdentityValue reduces an untrusted name to something safe to store.
//
// Kubernetes names are already restricted to lowercase alphanumerics, '-' and
// '.', so a well-formed name passes through unchanged. This is here for the
// values that are NOT well formed: the parameter map is not validated by the
// provisioner, an operator can set these keys by hand in a StorageClass, and a
// value carrying a newline, a quote or a control character would be written
// verbatim into a ZFS property and read back by every tool that parses `zfs
// get` output a line at a time. Anything outside the allowed set becomes '_'
// rather than being dropped, so a mangled value still looks mangled instead of
// silently becoming a different valid name.
func sanitiseIdentityValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(v))
	for _, r := range v {
		switch {
		case r == '-' || r == '.' || r == '_':
			b.WriteRune(r)
		case r > unicode.MaxASCII || (!unicode.IsLetter(r) && !unicode.IsDigit(r)):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	return truncate(b.String(), maxIdentityValue)
}

// truncate cuts s to at most n bytes. Every rune it can produce is one byte
// wide, because sanitiseIdentityValue has already replaced everything else.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
