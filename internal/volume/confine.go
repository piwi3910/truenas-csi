package volume

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// ErrOutsideParent is returned when a volume handle names, or can be made to
// name, a dataset outside the operator-configured parent dataset.
var ErrOutsideParent = errors.New("volume resolves outside the configured parent dataset")

// Confine verifies that id's dataset path lies inside <allowedPool>/<allowedParent>.
//
// It is the last check before any destructive middleware call, and it does not
// assume the ID came from ParseID: every component is re-validated here, because
// a component carrying a separator or a ".." would otherwise let a cleaned path
// land outside the parent (or, with a separator in Name, inside a nested dataset
// the driver does not own).
func Confine(id ID, allowedPool, allowedParent string) error {
	if err := validComponent(allowedPool); err != nil {
		return fmt.Errorf("configured pool: %w", err)
	}
	if err := validComponent(allowedParent); err != nil {
		return fmt.Errorf("configured parent dataset: %w", err)
	}

	components := []struct {
		field string
		value string
	}{
		{"pool", id.Pool},
		{"parent", id.Parent},
		{"name", id.Name},
	}
	// The namespace is only a component when there is one; an empty Namespace
	// is the flat layout, not a malformed handle.
	if id.Namespace != "" {
		components = append(components, struct{ field, value string }{"namespace", id.Namespace})
	}
	for _, c := range components {
		if err := validComponent(c.value); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrOutsideParent, c.field, err)
		}
	}

	if id.Pool != allowedPool {
		return fmt.Errorf("%w: pool %q is not the configured pool %q", ErrOutsideParent, id.Pool, allowedPool)
	}
	if id.Parent != allowedParent {
		return fmt.Errorf("%w: parent %q is not the configured parent dataset %q", ErrOutsideParent, id.Parent, allowedParent)
	}

	// Belt and braces: even with every component validated, compare the cleaned
	// dataset path against the allowed prefix. The trailing separator on the
	// prefix is what stops "Pool0/k8s-other" matching "Pool0/k8s".
	prefix := path.Clean(allowedPool+idSeparator+allowedParent) + idSeparator
	resolved := path.Clean(id.DatasetPath())
	if !strings.HasPrefix(resolved, prefix) {
		return fmt.Errorf("%w: %q is not under %q", ErrOutsideParent, resolved, prefix)
	}
	return nil
}
