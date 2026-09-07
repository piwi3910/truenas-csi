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

// idComponents is the exact number of components in a volume handle.
const idComponents = 5

// ErrMalformedID is returned by ParseID for any handle that is not exactly five
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
	Name     string
}

// String renders the handle as <backend>/<protocol>/<pool>/<parent>/<name>.
func (id ID) String() string {
	return strings.Join([]string{id.Backend, id.Protocol, id.Pool, id.Parent, id.Name}, idSeparator)
}

// DatasetPath renders the ZFS dataset path <pool>/<parent>/<name>.
//
// The result is only trustworthy after Confine has approved the ID: on its own
// this is plain string concatenation and will happily produce a traversing path
// from a hostile handle.
func (id ID) DatasetPath() string {
	return strings.Join([]string{id.Pool, id.Parent, id.Name}, idSeparator)
}

// ParseID splits a volume handle into its five components, rejecting anything
// that could resolve to a dataset other than the one it names.
//
// Handles are not escaped: a component containing a path separator is rejected
// rather than unescaped, so there is no encoding under which two distinct
// handles can name the same dataset.
func ParseID(s string) (ID, error) {
	parts := strings.Split(s, idSeparator)
	if len(parts) != idComponents {
		return ID{}, fmt.Errorf("%w: %q has %d components, want %d", ErrMalformedID, s, len(parts), idComponents)
	}
	for _, p := range parts {
		if err := validComponent(p); err != nil {
			return ID{}, fmt.Errorf("%w: %q: %w", ErrMalformedID, s, err)
		}
	}
	return ID{
		Backend:  parts[0],
		Protocol: parts[1],
		Pool:     parts[2],
		Parent:   parts[3],
		Name:     parts[4],
	}, nil
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
