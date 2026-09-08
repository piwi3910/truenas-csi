package upgrade_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/piwi3910/truenas-csi/operator/internal/upgrade"
)

// table is a deliberately richer table than the one that has shipped, so every
// branch is exercised against something with real shape: 0.5.0 is a release
// with a migration in both directions, 0.9.0 requires that migration to have
// happened, and 0.3.0 declares nothing at all.
var table = upgrade.Table{
	"0.1.0": {},
	"0.3.0": {},
	"0.5.0": {MinUpgradeFrom: "0.3.0", MinDowngradeTo: "0.3.0"},
	"0.9.0": {MinUpgradeFrom: "0.5.0", MinDowngradeTo: "0.5.0"},
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name       string
		from, to   string
		wantRefuse bool
		wantIn     string // substring the refusal must name, so it stays actionable
	}{
		{
			name: "fresh install is never gated",
			from: "", to: "0.9.0",
		},
		{
			name: "fresh install of an old version is not gated either",
			from: "", to: "0.1.0",
		},
		{
			name: "no change",
			from: "0.9.0", to: "0.9.0",
		},
		{
			name: "single step upgrade",
			from: "0.5.0", to: "0.9.0",
		},
		{
			name: "upgrade from exactly the floor is allowed: the bound is inclusive",
			from: "0.5.0", to: "0.9.0",
		},
		{
			name: "skipping the migration release is refused",
			from: "0.3.0", to: "0.9.0",
			wantRefuse: true,
			wantIn:     "upgrade to v0.5.0 first",
		},
		{
			name: "a much older version is refused with the same instruction",
			from: "0.1.0", to: "0.9.0",
			wantRefuse: true,
			wantIn:     "upgrade to v0.5.0 first",
		},
		{
			name: "upgrade to a version that declares no floor",
			from: "0.1.0", to: "0.3.0",
		},
		{
			name: "upgrade to a version absent from the table is not gated",
			from: "0.1.0", to: "0.11.0",
		},
		{
			name: "the v prefix on either side is not a different version",
			from: "v0.3.0", to: "v0.9.0",
			wantRefuse: true,
			wantIn:     "upgrade from v0.3.0 to v0.9.0",
		},
		{
			name: "downgrade within the declared window",
			from: "0.9.0", to: "0.5.0",
		},
		{
			name: "downgrade past the version's own floor is refused",
			from: "0.9.0", to: "0.3.0",
			wantRefuse: true,
			wantIn:     "downgrade from v0.9.0 to v0.3.0 is not supported",
		},
		{
			name: "downgrade is gated by the version being left, not the target",
			from: "0.5.0", to: "0.1.0",
			wantRefuse: true,
			wantIn:     "v0.3.0 is the oldest version v0.5.0 can be rolled back to",
		},
		{
			name: "downgrade from a version that declares no floor",
			from: "0.3.0", to: "0.1.0",
		},
		{
			name: "downgrade from a version absent from the table is not gated",
			from: "0.11.0", to: "0.1.0",
		},
		{
			// String comparison would put "0.10.0" below "0.9.0" and let this
			// through as a downgrade, skipping 0.9.0's upgrade floor entirely.
			name: "double-digit minors order numerically",
			from: "0.3.0", to: "0.10.0",
		},
		{
			name: "a pre-release sorts below its release, so this is an upgrade",
			from: "0.9.0-rc.1", to: "0.9.0",
		},
		{
			// 0.9.0-rc.1 is below 0.9.0, so reaching it from 0.3.0 is an
			// upgrade and 0.9.0's floor applies to the candidate as well.
			name: "a release candidate carries its release's floor",
			from: "0.3.0", to: "0.9.0-rc.1",
			wantRefuse: true,
			wantIn:     "upgrade to v0.5.0 first",
		},
		{
			name: "an unparseable current version is not gated",
			from: "main", to: "0.9.0",
		},
		{
			name: "an unparseable target is not gated",
			from: "0.1.0", to: "main",
		},
		{
			name: "build metadata does not hide a table entry",
			from: "0.3.0", to: "0.9.0+build.7",
			wantRefuse: true,
			wantIn:     "upgrade to v0.5.0 first",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := table.Check(tt.from, tt.to)
			if !tt.wantRefuse {
				if err != nil {
					t.Fatalf("Check(%q, %q) = %v, want acceptance", tt.from, tt.to, err)
				}
				return
			}
			if !errors.Is(err, upgrade.ErrUnsupportedPath) {
				t.Fatalf("Check(%q, %q) = %v, want an ErrUnsupportedPath refusal", tt.from, tt.to, err)
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("refusal %q does not say %q; an administrator cannot act on it", err, tt.wantIn)
			}
		})
	}
}

// TestCheckReportsMalformedDeclarations catches a floor that was typed wrong.
// Skipping such an entry silently would turn a refusal nobody wrote correctly
// into no refusal at all, which is the one outcome worse than either.
func TestCheckReportsMalformedDeclarations(t *testing.T) {
	broken := upgrade.Table{
		"0.9.0": {MinUpgradeFrom: "zero-point-five", MinDowngradeTo: "also-not-a-version"},
	}
	if err := broken.Check("0.3.0", "0.9.0"); !errors.Is(err, upgrade.ErrUnsupportedPath) {
		t.Errorf("upgrade with a malformed floor: err = %v, want a refusal", err)
	}
	if err := broken.Check("0.9.0", "0.3.0"); !errors.Is(err, upgrade.ErrUnsupportedPath) {
		t.Errorf("downgrade with a malformed floor: err = %v, want a refusal", err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		in      upgrade.Table
		wantErr bool
	}{
		{name: "empty", in: upgrade.Table{}},
		{name: "well formed", in: table},
		{name: "unset floors are allowed", in: upgrade.Table{"0.1.0": {}}},
		{name: "bad key", in: upgrade.Table{"nightly": {}}, wantErr: true},
		{name: "bad upgrade floor", in: upgrade.Table{"0.5.0": {MinUpgradeFrom: "old"}}, wantErr: true},
		{name: "bad downgrade floor", in: upgrade.Table{"0.5.0": {MinDowngradeTo: "old"}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := upgrade.Validate(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestShippedTableIsWellFormed is the test that matters at release time: the
// table an operator image actually carries is the one nobody re-reads.
func TestShippedTableIsWellFormed(t *testing.T) {
	if err := upgrade.Validate(upgrade.Shipped); err != nil {
		t.Fatalf("the shipped upgrade table is malformed: %v", err)
	}
	if len(upgrade.Shipped) == 0 {
		t.Error("the shipped table is empty; every released driver version needs an entry, even one declaring no floor")
	}
}

// TestPackageCheckUsesTheShippedTable guards the thin wrapper, which is the
// only entry point the reconciler calls.
func TestPackageCheckUsesTheShippedTable(t *testing.T) {
	if err := upgrade.Check("", "0.1.0"); err != nil {
		t.Fatalf("fresh install of the shipped version: err = %v, want acceptance", err)
	}
	if err := upgrade.Check("0.1.0", "0.1.0"); err != nil {
		t.Fatalf("no-op reconcile of the shipped version: err = %v, want acceptance", err)
	}
}
