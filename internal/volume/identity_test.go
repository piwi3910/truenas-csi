package volume

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// TestIdentityFrom covers what arrives in CreateVolume parameters, which is a
// map anyone with StorageClass edit rights can write into and the provisioner
// fills in only when --extra-create-metadata is set. Every row is a shape the
// driver has to survive without failing provisioning.
func TestIdentityFrom(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]string
		want   Identity
	}{
		{
			name:   "no parameters at all",
			params: nil,
			want:   Identity{},
		},
		{
			// csi-sanity and static provisioning both look like this.
			name:   "storage class parameters but no metadata",
			params: map[string]string{"protocol": "nfs", "server": "192.168.10.253"},
			want:   Identity{},
		},
		{
			name: "the usual provisioner payload",
			params: map[string]string{
				ParamPVCName:      "postgres-data",
				ParamPVCNamespace: "prod",
				ParamPVName:       "pvc-2f1c9a3e-1b6d-4f9e-9c11-8d0f7a2b5c44",
			},
			want: Identity{
				PVCName:      "postgres-data",
				PVCNamespace: "prod",
				PVName:       "pvc-2f1c9a3e-1b6d-4f9e-9c11-8d0f7a2b5c44",
			},
		},
		{
			// A claim name is a DNS-1123 subdomain, so dots are legal and must
			// survive: mangling them would record a name matching no PVC.
			name: "legal but unusual kubernetes names",
			params: map[string]string{
				ParamPVCName:      "my.app.data-0",
				ParamPVCNamespace: "team-a-1",
			},
			want: Identity{PVCName: "my.app.data-0", PVCNamespace: "team-a-1"},
		},
		{
			name:   "namespace only",
			params: map[string]string{ParamPVCNamespace: "prod"},
			want:   Identity{PVCNamespace: "prod"},
		},
		{
			name:   "surrounding whitespace is not part of the name",
			params: map[string]string{ParamPVCName: "  data  "},
			want:   Identity{PVCName: "data"},
		},
		{
			name:   "empty values are the same as absent",
			params: map[string]string{ParamPVCName: "", ParamPVCNamespace: "   "},
			want:   Identity{},
		},
		{
			// The value reaches the appliance as a ZFS property value. A newline
			// would be written verbatim and split the record in half for every
			// tool that reads `zfs get` output a line at a time.
			name:   "newline injected into the name",
			params: map[string]string{ParamPVCName: "data\nio.truenas.csi:managed\ttruenas-csi"},
			want:   Identity{PVCName: "data_io.truenas.csi_managed_truenas-csi"},
		},
		{
			name:   "quotes and shell metacharacters",
			params: map[string]string{ParamPVCName: `a"b'c;$(rm -rf /)`},
			want:   Identity{PVCName: "a_b_c___rm_-rf___"},
		},
		{
			name:   "non-ascii is folded rather than passed through",
			params: map[string]string{ParamPVCNamespace: "prodüction"},
			want:   Identity{PVCNamespace: "prod_ction"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IdentityFrom(tc.params); got != tc.want {
				t.Fatalf("IdentityFrom = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestIdentityFromTruncatesOversizedValues: a legal Kubernetes name fits, but
// nothing validates the parameter map, and an unbounded string handed to the
// appliance is a dataset write the driver cannot predict the outcome of.
func TestIdentityFromTruncatesOversizedValues(t *testing.T) {
	id := IdentityFrom(map[string]string{
		ParamPVCName:      strings.Repeat("a", 4096),
		ParamPVCNamespace: strings.Repeat("b", 253),
	})
	if len(id.PVCName) != maxIdentityValue {
		t.Fatalf("PVCName length = %d, want %d", len(id.PVCName), maxIdentityValue)
	}
	if len(id.PVCNamespace) != 253 {
		t.Fatalf("a 253-character namespace must survive intact, got %d", len(id.PVCNamespace))
	}
	if len(id.Description()) > maxDescription {
		t.Fatalf("description length = %d, want at most %d", len(id.Description()), maxDescription)
	}
}

// TestIdentityProperties: an unrecorded field must be absent rather than
// present and empty, so "not recorded" and "recorded as nothing" stay
// distinguishable on the appliance.
func TestIdentityProperties(t *testing.T) {
	cases := []struct {
		name  string
		id    Identity
		want  map[string]string
		empty bool
	}{
		{
			name:  "nothing known",
			id:    Identity{},
			want:  map[string]string{},
			empty: true,
		},
		{
			name: "everything known",
			id:   Identity{PVCName: "data", PVCNamespace: "prod", PVName: "pvc-1"},
			want: map[string]string{
				PVCNameProperty:      "data",
				PVCNamespaceProperty: "prod",
				PVNameProperty:       "pvc-1",
			},
		},
		{
			name: "namespace missing",
			id:   Identity{PVCName: "data", PVName: "pvc-1"},
			want: map[string]string{PVCNameProperty: "data", PVNameProperty: "pvc-1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.Empty(); got != tc.empty {
				t.Fatalf("Empty = %v, want %v", got, tc.empty)
			}
			got := tc.id.Properties()
			if !maps.Equal(got, tc.want) {
				t.Fatalf("Properties = %v, want %v", got, tc.want)
			}
			// The payload order must not depend on map iteration, or two
			// identical requests would send different bytes to the appliance.
			keys := tc.id.PropertyKeys()
			if !slices.IsSorted(keys) {
				t.Fatalf("PropertyKeys = %v, want sorted", keys)
			}
			if len(keys) != len(tc.want) {
				t.Fatalf("PropertyKeys = %v, want %d keys", keys, len(tc.want))
			}
		})
	}
}

// TestIdentityDescription pins the text an operator reads in the TrueNAS UI's
// Edit Dataset pane, including the partial cases: a description that silently
// renders "prod/" reads as a claim with no name.
func TestIdentityDescription(t *testing.T) {
	cases := []struct {
		name string
		id   Identity
		want string
	}{
		{name: "nothing known", id: Identity{}, want: ""},
		{
			name: "everything known",
			id:   Identity{PVCName: "data", PVCNamespace: "prod", PVName: "pvc-1"},
			want: "Kubernetes PVC prod/data (pvc-1)",
		},
		{
			name: "no pv name",
			id:   Identity{PVCName: "data", PVCNamespace: "prod"},
			want: "Kubernetes PVC prod/data",
		},
		{
			name: "no namespace",
			id:   Identity{PVCName: "data"},
			want: "Kubernetes PVC <unknown>/data",
		},
		{
			name: "no claim name",
			id:   Identity{PVCNamespace: "prod"},
			want: "Kubernetes PVC prod/<unknown>",
		},
		{
			name: "pv name only",
			id:   Identity{PVName: "pvc-1"},
			want: "Kubernetes PVC <unknown>/<unknown> (pvc-1)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.Description(); got != tc.want {
				t.Fatalf("Description = %q, want %q", got, tc.want)
			}
		})
	}
}
