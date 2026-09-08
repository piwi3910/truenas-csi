// Package volume defines the driver's volume identity — the opaque handle CSI
// hands back to Kubernetes — and the safety checks that keep every dataset the
// driver touches inside the operator-configured parent dataset.
package volume

import (
	"errors"
	"fmt"
	"strings"
)

// idSeparator joins the components of a volume handle. It is also a ZFS dataset
// path separator, which is why no component may contain one.
const idSeparator = "/"

// The two legal shapes of a volume handle.
//
//	flat        <backend>/<protocol>/<pool>/<parent>/<name>
//	namespaced  <backend>/<protocol>/<pool>/<parent>/<namespace>/<name>
//
// Both are permanent. Per-namespace accounting is opt-in and can be switched on
// long after a cluster already has volumes, so every handle minted under the
// flat layout must keep resolving to the dataset it always named. It does,
// because the component count alone decides the shape and neither shape can
// render the other's dataset path: a flat handle is three path segments and a
// namespaced one is four, so no two distinct handles can name the same dataset.
const (
	idComponentsFlat       = 5
	idComponentsNamespaced = 6
)

// ErrMalformedID is returned by ParseID for any handle that is not five or six
// non-empty, non-traversing components.
var ErrMalformedID = errors.New("malformed volume id")

// ID is a volume handle: which appliance, which protocol, and where the dataset
// lives. It is persisted in the PersistentVolume's volumeHandle and is therefore
// immutable once a volume exists.
type ID struct {
	Backend  string
	Protocol string
	Pool     string
	Parent   string

	// Namespace is the Kubernetes namespace whose parent dataset this volume
	// lives under, or "" for the flat layout.
	//
	// Empty is the normal state, never an error: it is what every volume
	// created before per-namespace accounting was switched on carries, and what
	// csi-sanity, a static provisioner and any deployment without
	// --extra-create-metadata produce, none of which ever name a namespace.
	Namespace string

	Name string
}

// String renders the handle as <backend>/<protocol>/<pool>/<parent>/<name>,
// with <namespace> inserted before <name> when the volume is namespaced.
func (id ID) String() string {
	return strings.Join(id.components(), idSeparator)
}

// components returns the handle's parts in order, omitting the namespace when
// there is none.
func (id ID) components() []string {
	if id.Namespace == "" {
		return []string{id.Backend, id.Protocol, id.Pool, id.Parent, id.Name}
	}
	return []string{id.Backend, id.Protocol, id.Pool, id.Parent, id.Namespace, id.Name}
}

// DatasetPath renders the ZFS dataset path <pool>/<parent>[/<namespace>]/<name>.
//
// The result is only trustworthy after Confine has approved the ID: on its own
// this is plain string concatenation and will happily produce a traversing path
// from a hostile handle.
func (id ID) DatasetPath() string {
	if id.Namespace == "" {
		return strings.Join([]string{id.Pool, id.Parent, id.Name}, idSeparator)
	}
	return strings.Join([]string{id.Pool, id.Parent, id.Namespace, id.Name}, idSeparator)
}

// NamespaceDatasetPath renders the ZFS path of the namespace's parent dataset,
// or "" for a flat volume, which has none.
func (id ID) NamespaceDatasetPath() string {
	if id.Namespace == "" {
		return ""
	}
	return strings.Join([]string{id.Pool, id.Parent, id.Namespace}, idSeparator)
}

// ParseID splits a volume handle into its components, rejecting anything that
// could resolve to a dataset other than the one it names.
//
// Handles are not escaped: a component containing a path separator is rejected
// rather than unescaped, so there is no encoding under which two distinct
// handles can name the same dataset. That is also why the namespace is a
// component of its own rather than a separator smuggled into Parent or Name.
func ParseID(s string) (ID, error) {
	parts := strings.Split(s, idSeparator)
	if len(parts) != idComponentsFlat && len(parts) != idComponentsNamespaced {
		return ID{}, fmt.Errorf("%w: %q has %d components, want %d or %d",
			ErrMalformedID, s, len(parts), idComponentsFlat, idComponentsNamespaced)
	}
	for _, p := range parts {
		if err := validComponent(p); err != nil {
			return ID{}, fmt.Errorf("%w: %q: %w", ErrMalformedID, s, err)
		}
	}
	id := ID{Backend: parts[0], Protocol: parts[1], Pool: parts[2], Parent: parts[3]}
	if len(parts) == idComponentsNamespaced {
		id.Namespace = parts[4]
	}
	id.Name = parts[len(parts)-1]
	return id, nil
}

// IDFromLeaf rebuilds a volume handle from a dataset path relative to
// <pool>/<parent>/, which is how ListVolumes and the orphan scan turn appliance
// state back into handles.
//
// One leaf component is a flat volume; two are a namespace and a volume.
// Anything deeper is not a shape this driver creates — a hand-made dataset, or
// a layout from a future release — and is refused rather than guessed at,
// because a handle naming the wrong dataset is worse than no handle at all.
func IDFromLeaf(backend, protocol, pool, parent, leaf string) (ID, error) {
	parts := strings.Split(leaf, idSeparator)
	for _, p := range parts {
		if err := validComponent(p); err != nil {
			return ID{}, fmt.Errorf("%w: leaf %q: %w", ErrMalformedID, leaf, err)
		}
	}
	id := ID{Backend: backend, Protocol: protocol, Pool: pool, Parent: parent}
	switch len(parts) {
	case 1:
		id.Name = parts[0]
	case 2:
		id.Namespace, id.Name = parts[0], parts[1]
	default:
		return ID{}, fmt.Errorf("%w: leaf %q is %d levels below %s/%s",
			ErrMalformedID, leaf, len(parts), pool, parent)
	}
	return id, nil
}

// validComponent rejects empty, relative and separator-bearing components.
// Backslash is rejected alongside slash so a handle cannot mean one thing here
// and another on a path-handling layer that treats it as a separator.
func validComponent(p string) error {
	switch p {
	case "":
		return errors.New("empty component")
	case ".", "..":
		return fmt.Errorf("relative component %q", p)
	}
	if strings.ContainsAny(p, `/\`) {
		return fmt.Errorf("component %q contains a path separator", p)
	}
	return nil
}
