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

func TestStampPropertiesCarriesTheMarker(t *testing.T) {
	props := StampProperties()
	if len(props) != 1 {
		t.Fatalf("StampProperties() returned %d entries, want 1", len(props))
	}
	if got, want := props[0]["key"], OwnerProperty; got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
	if got, want := props[0]["value"], OwnerValue; got != want {
		t.Errorf("value = %q, want %q", got, want)
	}

	// The payload must be a fresh map each call: it is handed to the middleware
	// client, and a shared map would let one create mutate another's request.
	props[0]["value"] = "tampered"
	if again := StampProperties(); again[0]["value"] != OwnerValue {
		t.Errorf("StampProperties() returned shared state: value = %q", again[0]["value"])
	}
}
