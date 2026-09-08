package config

import (
	"strings"
	"testing"
	"time"
)

func backendWith(dp DeleteProtection) Backend {
	return Backend{
		Name: "nas1", Endpoint: "wss://192.168.10.253/api/current",
		Username: "truenas_admin", APIKey: "1-secret",
		Pool: "Pool0", ParentDataset: "k8s",
		DeleteProtection: dp,
	}
}

// TestDeleteProtectionIsOffByDefault is the whole point of the feature's
// default: an operator who has never heard of it must get today's behaviour,
// where DeleteVolume destroys the data.
func TestDeleteProtectionIsOffByDefault(t *testing.T) {
	var zero DeleteProtection
	if zero.Grace() != 0 {
		t.Errorf("the zero DeleteProtection has a grace period of %s", zero.Grace())
	}
	if err := backendWith(zero).validate(); err != nil {
		t.Errorf("a backend with no deleteProtection block is invalid: %v", err)
	}
}

// TestGraceCollapsesEveryOffStateToZero. Callers test On()/Grace() > 0 and
// nothing else, so every way of saying "no protection" must produce the same
// zero rather than a third state somebody forgets to handle.
func TestGraceCollapsesEveryOffStateToZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		dp   DeleteProtection
		want time.Duration
	}{
		{"disabled", DeleteProtection{}, 0},
		{"disabled with a grace period set", DeleteProtection{GracePeriod: "168h"}, 0},
		{"enabled but empty", DeleteProtection{Enabled: true}, 0},
		{"enabled with zero", DeleteProtection{Enabled: true, GracePeriod: "0s"}, 0},
		{"enabled with a negative period", DeleteProtection{Enabled: true, GracePeriod: "-1h"}, 0},
		{"enabled with nonsense", DeleteProtection{Enabled: true, GracePeriod: "soon"}, 0},
		{"enabled with a week", DeleteProtection{Enabled: true, GracePeriod: "168h"}, 168 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.dp.Grace(); got != tc.want {
				t.Errorf("Grace() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestDeleteProtectionValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dp      DeleteProtection
		flavour string
		want    string // substring of the expected error; "" means valid
	}{
		{name: "a week", dp: DeleteProtection{Enabled: true, GracePeriod: "168h"}},
		{
			name: "a custom graveyard name",
			dp:   DeleteProtection{Enabled: true, GracePeriod: "24h", GraveyardDataset: "csi-deleted"},
		},
		{
			name: "enabled with no grace period",
			dp:   DeleteProtection{Enabled: true},
			want: "is not a positive duration",
		},
		{
			name: "enabled with a zero grace period",
			dp:   DeleteProtection{Enabled: true, GracePeriod: "0s"},
			want: "is not a positive duration",
		},
		{
			// Silently ignoring a grace period an operator wrote is how a
			// cluster ends up with no protection and someone believing there is
			// a week of it.
			name: "a grace period with enabled left false",
			dp:   DeleteProtection{GracePeriod: "168h"},
			want: "enabled is false",
		},
		{
			name: "an unparsable grace period",
			dp:   DeleteProtection{Enabled: true, GracePeriod: "a week"},
			want: "is not a Go duration",
		},
		{
			name: "a graveyard name with a path separator",
			dp:   DeleteProtection{Enabled: true, GracePeriod: "1h", GraveyardDataset: "a/b"},
			want: "not legal in a ZFS dataset name",
		},
		{
			// entity_namecheck() rejects "." and ".." as whole components; a
			// LEADING dot is legal, which is why the default uses one.
			name: "a graveyard named .",
			dp:   DeleteProtection{Enabled: true, GracePeriod: "1h", GraveyardDataset: "."},
			want: "not a dataset name ZFS accepts",
		},
		{
			name: "a graveyard named ..",
			dp:   DeleteProtection{Enabled: true, GracePeriod: "1h", GraveyardDataset: ".."},
			want: "not a dataset name ZFS accepts",
		},
		{
			name: "the dot-prefixed default is accepted",
			dp:   DeleteProtection{Enabled: true, GracePeriod: "1h", GraveyardDataset: ".trash"},
		},
		{
			// The rename is a method the CORE REST mapping has never been
			// exercised against, so protection that cannot work must be refused
			// rather than appear to be on.
			name:    "enabled on a CORE appliance",
			dp:      DeleteProtection{Enabled: true, GracePeriod: "168h"},
			flavour: FlavourCORE,
			want:    "not supported on flavour",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := backendWith(tc.dp)
			if tc.flavour == FlavourCORE {
				b.Flavour = FlavourCORE
				b.Endpoint = "https://192.168.10.253"
			}
			err := b.validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("want valid, got %v", err)
			case tc.want == "":
			case err == nil:
				t.Fatalf("want an error mentioning %q, got nil", tc.want)
			case !strings.Contains(err.Error(), tc.want):
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestDeleteProtectionRequiresARestart. The reaper is started once, from the
// configuration the controller booted with, so adopting a shortened grace
// period live would make datasets reapable that the earlier configuration had
// promised to keep.
func TestDeleteProtectionRequiresARestart(t *testing.T) {
	cur := &Config{Backends: map[string]Backend{
		"nas1": backendWith(DeleteProtection{Enabled: true, GracePeriod: "168h"})}}
	next := &Config{Backends: map[string]Backend{
		"nas1": backendWith(DeleteProtection{Enabled: true, GracePeriod: "1h"})}}

	err := CheckReloadable(cur, next)
	if err == nil {
		t.Fatal("a shortened grace period was accepted at runtime")
	}
	if !strings.Contains(err.Error(), "deleteProtection") {
		t.Errorf("the refusal does not name the field: %v", err)
	}
	if err := CheckReloadable(cur, cur); err != nil {
		t.Errorf("an unchanged configuration was refused: %v", err)
	}
}
