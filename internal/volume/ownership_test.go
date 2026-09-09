package volume

import (
	"errors"
	"testing"
)

func TestDeleteRefusesUnmarkedDataset(t *testing.T) {
	cases := []struct {
		name    string
		ds      *Dataset
		wantErr error
	}{
		{
			name:    "no user properties at all",
			ds:      &Dataset{ID: "Pool0/k8s/pvc-1"},
			wantErr: ErrNotManaged,
		},
		{
			name: "unrelated properties only",
			ds: &Dataset{ID: "Pool0/k8s/pvc-1", UserProperties: map[string]Property{
				"com.example:owner": {Value: "someone-else", Source: "LOCAL"},
			}},
			wantErr: ErrNotManaged,
		},
		{
			// ZFS user properties are inherited by children: if the operator's
			// parent dataset ever carried the marker, every pre-existing dataset
			// beneath it would look driver-owned. Presence is not ownership.
			name: "marker inherited from the parent dataset",
			ds: &Dataset{ID: "Pool0/k8s/real-data", UserProperties: map[string]Property{
				OwnerProperty: {Value: OwnerValue, Source: "INHERITED"},
			}},
			wantErr: ErrNotManaged,
		},
		{
			name: "marker set locally but by something else",
			ds: &Dataset{ID: "Pool0/k8s/pvc-1", UserProperties: map[string]Property{
				OwnerProperty: {Value: "something-else", Source: "LOCAL"},
			}},
			wantErr: ErrNotManaged,
		},
		{
			name: "marker present with an empty source",
			ds: &Dataset{ID: "Pool0/k8s/pvc-1", UserProperties: map[string]Property{
				OwnerProperty: {Value: OwnerValue},
			}},
			wantErr: ErrNotManaged,
		},
		{
			name: "marked by this driver",
			ds: &Dataset{ID: "Pool0/k8s/pvc-1", UserProperties: map[string]Property{
				OwnerProperty: {Value: "truenas-csi", Source: "LOCAL"},
			}},
			wantErr: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyOwned(tc.ds)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("VerifyOwned() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("VerifyOwned() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestVerifyOwnedRejectsNilDataset(t *testing.T) {
	if err := VerifyOwned(nil); !errors.Is(err, ErrNotManaged) {
		t.Fatalf("VerifyOwned(nil) = %v, want ErrNotManaged", err)
	}
}

func TestStampPropertiesCarriesTheMarkerAndTheID(t *testing.T) {
	const id = "Pool0/k8s/pvc-1"
	props := StampProperties(id)
	if len(props) != 2 {
		t.Fatalf("StampProperties(%q) returned %d entries, want 2", id, len(props))
	}
	got := map[string]string{}
	for _, p := range props {
		got[p["key"]] = p["value"]
	}
	if got[OwnerProperty] != OwnerValue {
		t.Errorf("%s = %q, want %q", OwnerProperty, got[OwnerProperty], OwnerValue)
	}
	// The id is what makes an inherited marker detectable: a child carries its
	// ancestor's id, not its own.
	if got[OwnerIDProperty] != id {
		t.Errorf("%s = %q, want %q", OwnerIDProperty, got[OwnerIDProperty], id)
	}

	// A caller that does not know the path still gets a usable marker, and the
	// dataset falls back to the weaker legacy check.
	if only := StampProperties(""); len(only) != 1 {
		t.Errorf("StampProperties(\"\") returned %d entries, want 1", len(only))
	}

	// The payload must be a fresh map each call: it is handed to the middleware
	// client, and a shared map would let one create mutate another's request.
	props[0]["value"] = "tampered"
	if again := StampProperties(id); again[0]["value"] != OwnerValue {
		t.Errorf("StampProperties() returned shared state: value = %q", again[0]["value"])
	}
}

// TestInheritedMarkerIsRefusedWithoutTrustingSource is the guard for the defect
// the appliance exposed: a dataset an operator creates by hand underneath a
// driver-owned one inherits the marker and, on TrueNAS, reports it as LOCAL.
//
// Verified on 25.10.6 — pool.dataset.query answers EVERY user property with
// source "LOCAL", inherited or not:
//
//	Pool0/p     managed=truenas-csi source=LOCAL   (set here)
//	Pool0/p/c   managed=truenas-csi source=LOCAL   (INHERITED, never set,
//	                                                never created by us)
//
// So the source field cannot carry this check, and every case below deliberately
// claims source LOCAL. What refuses the child is the owner id naming an
// ancestor rather than itself — evidence in the data, which no mock can soften.
func TestInheritedMarkerIsRefusedWithoutTrustingSource(t *testing.T) {
	local := func(v string) Property { return Property{Value: v, Source: "LOCAL"} }

	for _, tc := range []struct {
		name    string
		ds      *Dataset
		wantErr bool
	}{
		{
			name: "a dataset that stamped itself is owned",
			ds: &Dataset{ID: "Pool0/k8s/pvc-1", UserProperties: map[string]Property{
				OwnerProperty:   local(OwnerValue),
				OwnerIDProperty: local("Pool0/k8s/pvc-1"),
			}},
		},
		{
			name: "a child inheriting its parent's marker is NOT owned",
			ds: &Dataset{ID: "Pool0/k8s/pvc-1/hand-made", UserProperties: map[string]Property{
				OwnerProperty:   local(OwnerValue),
				OwnerIDProperty: local("Pool0/k8s/pvc-1"),
			}},
			wantErr: true,
		},
		{
			name: "a dataset under the graveyard inheriting the root's marker is NOT owned",
			ds: &Dataset{ID: "Pool0/k8s/.trash/entry", UserProperties: map[string]Property{
				OwnerProperty:   local(OwnerValue),
				OwnerIDProperty: local("Pool0/k8s/.trash"),
			}},
			wantErr: true,
		},
		{
			name: "a legacy dataset with no owner id still verifies on source alone",
			ds: &Dataset{ID: "Pool0/k8s/pvc-old", UserProperties: map[string]Property{
				OwnerProperty: local(OwnerValue),
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyOwned(tc.ds)
			if tc.wantErr && err == nil {
				t.Errorf("%q was accepted as driver-owned, but its marker names %q. "+
					"On TrueNAS an inherited property reports source LOCAL, so accepting "+
					"this clears a destructive path to act on data the driver never created.",
					tc.ds.ID, tc.ds.UserProperties[OwnerIDProperty].Value)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("VerifyOwned(%q) = %v, want owned", tc.ds.ID, err)
			}
		})
	}
}
