package volume

import (
	"errors"
	"strings"
	"testing"
)

func TestVolumeIDRoundTrip(t *testing.T) {
	const s = "nas1/iscsi/Pool0/k8s/pvc-abc"

	id, err := ParseID(s)
	if err != nil {
		t.Fatalf("ParseID(%q) returned error: %v", s, err)
	}
	if got := id.String(); got != s {
		t.Errorf("String() = %q, want %q", got, s)
	}
	if got, want := id.DatasetPath(), "Pool0/k8s/pvc-abc"; got != want {
		t.Errorf("DatasetPath() = %q, want %q", got, want)
	}
	if id.Backend != "nas1" || id.Protocol != "iscsi" || id.Pool != "Pool0" ||
		id.Parent != "k8s" || id.Name != "pvc-abc" {
		t.Errorf("ParseID(%q) = %+v, components do not match the input", s, id)
	}
}

// splitRaw builds an ID from a raw volume-handle string without validating it, so
// Confine is exercised directly on hostile input that ParseID would already reject.
// The final component absorbs the remainder, which is how a traversal payload would
// reach Confine if any caller ever skipped ParseID.
func splitRaw(t *testing.T, s string) ID {
	t.Helper()
	parts := strings.SplitN(s, "/", 5)
	if len(parts) != 5 {
		t.Fatalf("test input %q does not have at least five components", s)
	}
	return ID{Backend: parts[0], Protocol: parts[1], Pool: parts[2], Parent: parts[3], Name: parts[4]}
}

func TestVolumeIDConfinement(t *testing.T) {
	const (
		allowedPool   = "Pool0"
		allowedParent = "k8s"
	)

	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "parent escape via dotdot in name", raw: "nas1/nfs/Pool0/k8s/../../Home", wantErr: true},
		{name: "dotdot as parent", raw: "nas1/nfs/Pool0/../Home/x", wantErr: true},
		{name: "dot and dotdot mixed", raw: "nas1/nfs/Pool0/k8s/./../../old_homes", wantErr: true},
		{name: "wrong pool", raw: "nas1/nfs/OtherPool/k8s/x", wantErr: true},
		{name: "wrong parent", raw: "nas1/nfs/Pool0/notk8s/x", wantErr: true},
		{name: "separator inside name", raw: "nas1/nfs/Pool0/k8s/pvc/nested", wantErr: true},
		{name: "prefix sibling is not a match", raw: "nas1/nfs/Pool0/k8s-other/x", wantErr: true},
		{name: "valid", raw: "nas1/nfs/Pool0/k8s/pvc-1", wantErr: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := splitRaw(t, tc.raw)
			err := Confine(id, allowedPool, allowedParent)
			if tc.wantErr {
				if !errors.Is(err, ErrOutsideParent) {
					t.Fatalf("Confine(%q) = %v, want ErrOutsideParent", tc.raw, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Confine(%q) returned error: %v", tc.raw, err)
			}
		})
	}
}

func TestParseIDRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "empty string", raw: ""},
		{name: "three segments", raw: "nas1/nfs/Pool0"},
		{name: "six segments", raw: "nas1/nfs/Pool0/k8s/a/b"},
		{name: "empty component between separators", raw: "nas1/nfs//k8s/pvc-1"},
		{name: "empty trailing component", raw: "nas1/nfs/Pool0/k8s/"},
		{name: "dot component", raw: "nas1/nfs/Pool0/./pvc-1"},
		{name: "dotdot component", raw: "nas1/nfs/Pool0/../pvc-1"},
		{name: "backslash separator in component", raw: `nas1/nfs/Pool0/k8s/pvc\1`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if id, err := ParseID(tc.raw); err == nil {
				t.Fatalf("ParseID(%q) = %+v, want error", tc.raw, id)
			}
		})
	}
}
