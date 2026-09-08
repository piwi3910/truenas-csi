// Package upgrade decides whether one driver version may replace another.
//
// Version skew (internal/skew) answers "can this driver run with these
// sidecars". This package answers a different question: "can this driver be
// reached from the one that is already installed". The two are independent —
// a driver can be perfectly compatible with its sidecars and still be
// unreachable from the release currently on the cluster, because the step
// between them requires a migration that only an intermediate release performs.
//
// The failure this prevents is specific. A driver release that changes how
// volume IDs, ZFS user properties or share host-access lists are shaped
// normally ships the conversion in the release that introduces it. Skipping
// that release leaves volumes on disk in the old shape and a driver that only
// understands the new one: PVCs that already exist stop resolving, and the
// damage is discovered one workload at a time. Refusing the jump costs an
// administrator one extra `kubectl apply`; not refusing it costs them their
// data path.
//
// # Why an embedded Go table rather than a config directory
//
// Dell's csm-operator ships one directory per driver version inside the
// operator image, each holding an `upgrade-path.yaml` with a `minUpgradePath`
// key, and reads the file at reconcile time. That indirection buys them
// something real — one operator image manages several distinct drivers whose
// release trains move independently — but it costs the consumer the ability to
// see the rule without unpacking the image, and it means an unreadable or
// absent file is discovered only when someone attempts the upgrade.
//
// This operator manages exactly one driver, shipped from this repository, so
// the table below is the same information with none of the indirection: it is
// reviewed in the same pull request as the release that changes it, it is
// compiled (so a malformed entry is a test failure rather than a runtime one),
// and it is as auditable as the YAML would be. The one thing it gives up is
// patchability without a rebuild, which for a rule whose entire purpose is to
// stop an operator being talked out of a refusal is not a loss.
package upgrade

import (
	"errors"
	"fmt"
	"sort"

	"github.com/Masterminds/semver/v3"
)

// ErrUnsupportedPath is returned for every step this package refuses. The
// wrapped message names both versions and the version to move through, because
// a refusal an administrator cannot act on is just an outage with better
// wording.
var ErrUnsupportedPath = errors.New("unsupported upgrade path")

// Path is the declared reachability of one shipped driver version.
//
// Both fields are inclusive bounds, and an empty field means "no constraint":
// a version that ships without a migration declares nothing and is reachable
// from anywhere this operator supports at all.
type Path struct {
	// MinUpgradeFrom is the oldest version that may be upgraded *to* this one
	// directly. It is consulted when this version is the target.
	MinUpgradeFrom string

	// MinDowngradeTo is the oldest version this one may be rolled back *to*.
	// It is consulted when this version is the version being left behind, and
	// it is what stops a rollback past a release whose on-disk changes the
	// older driver cannot read.
	MinDowngradeTo string
}

// Table maps a shipped driver version to its declared reachability. Keys are
// semantic versions with or without the "v" prefix.
type Table map[string]Path

// Shipped is the reachability of every driver version this repository has
// released.
//
// The rule for adding an entry: when a release changes the on-disk or
// appliance-side shape of anything an older driver wrote — volume ID format,
// ZFS user properties, share host-access lists, the iSCSI extent layout — give
// that release a MinUpgradeFrom naming the oldest release whose data it can
// still convert, and give it a MinDowngradeTo naming the oldest release that
// can still read what it writes. A release that changes none of that gets an
// entry with empty fields, which documents that the omission was considered
// rather than forgotten.
//
// Only 0.1.0 has shipped, so there is nothing yet to refuse; the entry exists
// so that the first release which does need a floor is an edit to an existing
// table rather than the invention of a mechanism under time pressure.
var Shipped = Table{
	"0.1.0": {},
}

// Check reports whether the driver may move from the currently applied version
// to the requested one, consulting the shipped table.
func Check(from, to string) error { return Shipped.Check(from, to) }

// Check reports whether the driver may move from the currently applied version
// `from` to the requested version `to`.
//
// An empty `from` is a fresh install and is never gated: there is no data
// written by a previous driver to be stranded, and gating it would mean an
// operator refused to install anything at all until someone seeded an
// annotation by hand.
//
// A version on either side that is not a semantic version — a "main" tag, a
// developer's branch build — is also not gated, for the same reason
// skew.Check accepts one: nobody arrives at such a tag by accident, and an
// operator that refuses to deploy them is unusable for the people writing the
// driver.
func (t Table) Check(from, to string) error {
	if from == "" || from == to {
		return nil
	}
	oldV, err := semver.NewVersion(from)
	if err != nil {
		return nil
	}
	newV, err := semver.NewVersion(to)
	if err != nil {
		return nil
	}

	// Compare, not string equality: it orders pre-releases correctly, so
	// 0.5.0-rc.1 sorts below 0.5.0 and a jump from the release candidate to
	// the release is the no-op step it should be.
	if newV.GreaterThan(oldV) {
		p, ok := t.lookup(newV)
		if !ok || p.MinUpgradeFrom == "" {
			return nil
		}
		floor, err := semver.NewVersion(p.MinUpgradeFrom)
		if err != nil {
			return fmt.Errorf("%w: version %s declares an unparseable minUpgradeFrom %q; this is an operator bug",
				ErrUnsupportedPath, to, p.MinUpgradeFrom)
		}
		if oldV.LessThan(floor) {
			return fmt.Errorf("%w: upgrade from v%s to v%s is not supported; upgrade to v%s first",
				ErrUnsupportedPath, oldV, newV, floor)
		}
		return nil
	}

	// A downgrade is gated by the version being left behind, because that is
	// the release that knows what it wrote.
	p, ok := t.lookup(oldV)
	if !ok || p.MinDowngradeTo == "" {
		return nil
	}
	floor, err := semver.NewVersion(p.MinDowngradeTo)
	if err != nil {
		return fmt.Errorf("%w: version %s declares an unparseable minDowngradeTo %q; this is an operator bug",
			ErrUnsupportedPath, from, p.MinDowngradeTo)
	}
	if newV.LessThan(floor) {
		return fmt.Errorf("%w: downgrade from v%s to v%s is not supported; v%s is the oldest version v%s can be rolled back to",
			ErrUnsupportedPath, oldV, newV, floor, oldV)
	}
	return nil
}

// lookup finds the entry for a version, comparing semantically rather than by
// string, so "v0.5.0" in a CR matches a "0.5.0" key and build metadata does not
// cause a miss.
//
// A pre-release falls back to its own release's entry: 0.9.0-rc.1 contains
// whatever migration 0.9.0 ships, so it must be reached the same way. Without
// the fallback, installing a release candidate would be the documented way to
// get around the floor.
func (t Table) lookup(v *semver.Version) (Path, bool) {
	var fallback *Path
	for k, p := range t {
		kv, err := semver.NewVersion(k)
		if err != nil {
			continue
		}
		if kv.Equal(v) {
			return p, true
		}
		if v.Prerelease() != "" && sameCore(kv, v) {
			entry := p
			fallback = &entry
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return Path{}, false
}

// sameCore reports whether two versions share a major.minor.patch.
func sameCore(a, b *semver.Version) bool {
	return a.Major() == b.Major() && a.Minor() == b.Minor() && a.Patch() == b.Patch()
}

// Validate reports every malformed entry in the table. It exists so that a
// typo in a version key is a unit-test failure at the moment it is written,
// rather than a silently skipped constraint discovered during an upgrade.
func Validate(t Table) error {
	var problems []string
	for _, k := range sortedKeys(t) {
		p := t[k]
		if _, err := semver.NewVersion(k); err != nil {
			problems = append(problems, fmt.Sprintf("key %q is not a version: %v", k, err))
		}
		for name, val := range map[string]string{"minUpgradeFrom": p.MinUpgradeFrom, "minDowngradeTo": p.MinDowngradeTo} {
			if val == "" {
				continue
			}
			if _, err := semver.NewVersion(val); err != nil {
				problems = append(problems, fmt.Sprintf("%s of %q is not a version: %q", name, k, val))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("upgrade table is malformed: %v", problems)
	}
	return nil
}

func sortedKeys(t Table) []string {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
