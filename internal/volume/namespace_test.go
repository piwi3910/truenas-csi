package volume

import (
	"errors"
	"strings"
	"testing"
)

// TestFlatHandlesSurviveTheNamespacedLayout is the test that must never be
// deleted. Per-namespace accounting is opt-in and can be switched on years into
// a cluster's life; every handle already written into a PersistentVolume was
// minted under the flat layout, and if one of them stops parsing — or starts
// resolving to a different dataset — those volumes are stranded with no way
// back, because a volumeHandle is immutable.
func TestFlatHandlesSurviveTheNamespacedLayout(t *testing.T) {
	cases := []struct {
		name        string
		handle      string
		wantDataset string
	}{
		{"nfs volume", "nas1/nfs/Pool0/k8s/pvc-1", "Pool0/k8s/pvc-1"},
		{"iscsi volume", "nas1/iscsi/Pool0/k8s/pvc-abcdef", "Pool0/k8s/pvc-abcdef"},
		{"parent with a dash", "nas2/smb/tank/csi-vols/pvc-9", "tank/csi-vols/pvc-9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := ParseID(tc.handle)
			if err != nil {
				t.Fatalf("ParseID(%q): %v", tc.handle, err)
			}
			if id.Namespace != "" {
				t.Errorf("Namespace = %q, want empty for a flat handle", id.Namespace)
			}
			if got := id.DatasetPath(); got != tc.wantDataset {
				t.Errorf("DatasetPath() = %q, want %q", got, tc.wantDataset)
			}
			if got := id.String(); got != tc.handle {
				t.Errorf("String() = %q, want the handle back unchanged %q", got, tc.handle)
			}
			if err := Confine(id, id.Pool, id.Parent); err != nil {
				t.Errorf("Confine: %v", err)
			}
		})
	}
}

// TestNamespacedHandleRoundTrip checks the new shape, and that it can never
// collide with a flat one: the two produce dataset paths of different depths.
func TestNamespacedHandleRoundTrip(t *testing.T) {
	id := ID{Backend: "nas1", Protocol: "nfs", Pool: "Pool0", Parent: "k8s",
		Namespace: "team-a", Name: "pvc-1"}
	const want = "nas1/nfs/Pool0/k8s/team-a/pvc-1"
	if got := id.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	back, err := ParseID(want)
	if err != nil {
		t.Fatalf("ParseID: %v", err)
	}
	if back != id {
		t.Fatalf("round trip = %+v, want %+v", back, id)
	}
	if got := id.DatasetPath(); got != "Pool0/k8s/team-a/pvc-1" {
		t.Errorf("DatasetPath() = %q", got)
	}
	if got := id.NamespaceDatasetPath(); got != "Pool0/k8s/team-a" {
		t.Errorf("NamespaceDatasetPath() = %q", got)
	}
	flat := ID{Backend: "nas1", Protocol: "nfs", Pool: "Pool0", Parent: "k8s", Name: "pvc-1"}
	if flat.DatasetPath() == id.DatasetPath() {
		t.Fatal("a flat and a namespaced handle resolved to the same dataset")
	}
	if flat.NamespaceDatasetPath() != "" {
		t.Errorf("a flat handle claimed a namespace dataset: %q", flat.NamespaceDatasetPath())
	}
}

func TestIDFromLeaf(t *testing.T) {
	cases := []struct {
		name    string
		leaf    string
		wantNS  string
		wantNm  string
		wantErr bool
	}{
		{name: "flat volume", leaf: "pvc-1", wantNm: "pvc-1"},
		{name: "namespaced volume", leaf: "team-a/pvc-1", wantNS: "team-a", wantNm: "pvc-1"},
		{name: "three levels is not a shape we create", leaf: "a/b/c", wantErr: true},
		{name: "empty leaf", leaf: "", wantErr: true},
		{name: "empty component", leaf: "team-a/", wantErr: true},
		{name: "traversal", leaf: "../pvc-1", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := IDFromLeaf("nas1", "nfs", "Pool0", "k8s", tc.leaf)
			if tc.wantErr {
				if !errors.Is(err, ErrMalformedID) {
					t.Fatalf("err = %v, want ErrMalformedID", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("IDFromLeaf: %v", err)
			}
			if id.Namespace != tc.wantNS || id.Name != tc.wantNm {
				t.Fatalf("got namespace %q name %q, want %q / %q",
					id.Namespace, id.Name, tc.wantNS, tc.wantNm)
			}
		})
	}
}

// TestValidateNamespace guards the one string that reaches a dataset path
// unmodified. Sanitising is wrong here — two namespaces that sanitised alike
// would share a dataset and a quota — so anything that is not already a
// DNS-1123 label must be refused.
func TestValidateNamespace(t *testing.T) {
	cases := []struct {
		name string
		ns   string
		ok   bool
	}{
		{name: "simple", ns: "default", ok: true},
		{name: "hyphenated", ns: "team-a-prod", ok: true},
		{name: "digits", ns: "ns1", ok: true},
		{name: "empty", ns: ""},
		{name: "path separator", ns: "team-a/evil"},
		{name: "traversal", ns: ".."},
		{name: "backslash", ns: `team\a`},
		{name: "uppercase", ns: "TeamA"},
		{name: "leading hyphen", ns: "-team"},
		{name: "trailing hyphen", ns: "team-"},
		{name: "underscore", ns: "team_a"},
		{name: "newline", ns: "team\na"},
		{name: "too long", ns: strings.Repeat("a", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNamespace(tc.ns)
			if tc.ok && err != nil {
				t.Fatalf("ValidateNamespace(%q) = %v, want nil", tc.ns, err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidNamespace) {
				t.Fatalf("ValidateNamespace(%q) = %v, want ErrInvalidNamespace", tc.ns, err)
			}
		})
	}
}

func TestAppliedQuota(t *testing.T) {
	cases := []struct {
		name     string
		recorded string
		want     int64
		wantOK   bool
	}{
		{name: "unlimited", recorded: "0", want: 0, wantOK: true},
		{name: "a quota", recorded: "1073741824", want: 1 << 30, wantOK: true},
		{name: "padded", recorded: " 42 ", want: 42, wantOK: true},
		{name: "never recorded", recorded: ""},
		{name: "garbage reads as unrecorded", recorded: "100Gi"},
		{name: "negative reads as unrecorded", recorded: "-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := AppliedQuota(tc.recorded)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("AppliedQuota(%q) = (%d, %v), want (%d, %v)",
					tc.recorded, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestIsNamespaceDataset(t *testing.T) {
	if IsNamespaceDataset("") {
		t.Error("a dataset with no LOCAL namespace property is not a namespace container")
	}
	if !IsNamespaceDataset("team-a") {
		t.Error("a dataset with a LOCAL namespace property is a namespace container")
	}
}

func TestNamespaceProperties(t *testing.T) {
	props := NamespaceProperties("team-a")
	if props[OwnerProperty] != OwnerValue {
		t.Errorf("namespace dataset must carry the ownership marker, got %q", props[OwnerProperty])
	}
	if props[NamespaceProperty] != "team-a" {
		t.Errorf("namespace marker = %q, want team-a", props[NamespaceProperty])
	}
}
