// Package retention implements delete protection: an optional grace period
// between DeleteVolume and the destruction of a volume's data.
//
// With it enabled, DeleteVolume tears down the volume's shares, extents and
// target mappings exactly as it does without it, and then RENAMES the dataset
// into a graveyard dataset instead of destroying it. A reaper destroys
// graveyard datasets whose grace period has expired.
//
// Two facts from the middleware's own documentation shape everything here.
//
//   - pool.dataset.rename "performs no safety checks"; renaming a dataset in
//     use by SMB, iSCSI, snapshot tasks or replication "may cause disruptions
//     or service failures" and needs force to proceed. So the rename happens
//     only AFTER teardown, never instead of it, and force is never passed: a
//     refusal is the appliance telling us teardown did not finish.
//   - pool.dataset.delete returns null rather than an error when the dataset
//     does not exist, which is what makes both retiring and reaping safely
//     repeatable.
//
// THE TRADE: capacity is NOT reclaimed during the grace period. This is stated
// in the values comment, in docs/delete-protection.md and in the log line
// DeleteVolume emits, because it is genuinely surprising and it interacts with
// the pool reserve and GetCapacity, both of which keep counting the space as
// used. That is correct — the space IS still allocated — not a bug.
package retention

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/piwi3910/truenas-csi/internal/config"
)

// Policy is one appliance's delete-protection configuration, resolved against
// the pool and parent dataset it applies to.
//
// The zero Policy is "off", and every caller tests exactly that: On(). There is
// no separate "enabled but zero grace" state to get wrong.
type Policy struct {
	// Pool and Parent are the operator-configured location this driver is
	// confined to. The graveyard is derived from them rather than configured
	// as a path, so no configuration mistake can put it outside the parent.
	Pool   string
	Parent string

	// Graveyard is the name of the dataset retired volumes are moved into,
	// relative to Parent.
	Graveyard string

	// Grace is how long a retired dataset is kept. Zero means delete
	// protection is off and DeleteVolume destroys as it always has.
	Grace time.Duration

	// ReapInterval is how often the reaper sweeps. It bounds how long past its
	// grace period a dataset survives and is never part of deciding whether one
	// may be destroyed.
	ReapInterval time.Duration
}

// PolicyFor resolves a backend's configuration into a Policy.
func PolicyFor(b config.Backend) Policy {
	return Policy{
		Pool:         b.Pool,
		Parent:       b.ParentDataset,
		Graveyard:    b.DeleteProtection.Graveyard(),
		Grace:        b.DeleteProtection.Grace(),
		ReapInterval: b.DeleteProtection.Reap(),
	}
}

// On reports whether delete protection applies.
//
// Pool and Parent are part of the test because a Policy without them cannot
// name a graveyard, and the safe reading of "I do not know where the graveyard
// is" is "protection is off", which is today's behaviour, rather than a rename
// to a path assembled from empty strings.
func (p Policy) On() bool {
	return p.Grace > 0 && p.Pool != "" && p.Parent != "" && p.Graveyard != ""
}

// Root is the graveyard dataset's full ZFS path.
func (p Policy) Root() string {
	return p.Pool + "/" + p.Parent + "/" + p.Graveyard
}

// prefix is Root plus a separator: the string a dataset id must begin with to
// be INSIDE the graveyard. The trailing separator is what stops a sibling
// dataset named ".trash-old" from matching ".trash".
func (p Policy) prefix() string { return p.Root() + "/" }

// ErrOutsideGraveyard means a dataset is not a direct child of the graveyard.
var ErrOutsideGraveyard = errors.New("dataset is not inside the graveyard")

// ConfineToGraveyard verifies that a dataset id names a DIRECT child of this
// policy's graveyard, and nothing else.
//
// It is the first of the reaper's four preconditions and the one that stops
// every catastrophic mistake the others cannot: a query that returned more than
// it was asked for, a prefix that happens to match a sibling, a nested dataset
// somebody created by hand, and the graveyard root itself. Depth is checked as
// well as prefix because the reaper destroys recursively — a match two levels
// down would take its siblings with it.
func (p Policy) ConfineToGraveyard(id string) error {
	if !p.On() {
		return fmt.Errorf("%w: delete protection is off, so there is no graveyard", ErrOutsideGraveyard)
	}
	if id == p.Root() {
		return fmt.Errorf("%w: %q IS the graveyard, not something in it", ErrOutsideGraveyard, id)
	}
	rest, ok := strings.CutPrefix(id, p.prefix())
	if !ok {
		return fmt.Errorf("%w: %q is not under %q", ErrOutsideGraveyard, id, p.prefix())
	}
	if rest == "" {
		return fmt.Errorf("%w: %q names no dataset under %q", ErrOutsideGraveyard, id, p.prefix())
	}
	if strings.Contains(rest, "/") {
		return fmt.Errorf("%w: %q is %d levels below %q; only direct children are ever destroyed",
			ErrOutsideGraveyard, id, strings.Count(rest, "/")+1, p.Root())
	}
	return nil
}

// String renders the policy for a log line.
func (p Policy) String() string {
	if !p.On() {
		return "retention{off}"
	}
	return fmt.Sprintf("retention{grace=%s graveyard=%s reap=%s}", p.Grace, p.Root(), p.ReapInterval)
}
