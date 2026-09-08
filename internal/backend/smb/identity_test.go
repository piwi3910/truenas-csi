package smb

import (
	"context"
	"strings"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// identityRequest is a create request carrying what csi-provisioner sends when
// the deployment passes --extra-create-metadata, which this driver's chart does.
func identityRequest(name string, bytes int64, pvc, ns string) backend.CreateRequest {
	r := testRequest(name, bytes)
	r.Params[volume.ParamPVCName] = pvc
	r.Params[volume.ParamPVCNamespace] = ns
	r.Params[volume.ParamPVName] = "pv-" + name
	return r
}

// TestSMBCreateRecordsPVCIdentity: an SMB share named after a pvc-<uuid>
// dataset says nothing about which workload owns it.
func TestSMBCreateRecordsPVCIdentity(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)

	if _, err := b.Create(context.Background(), identityRequest("pvc-1", 10*gib, "fileshare", "prod")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ds := n.dataset("Pool0/k8s/pvc-1")
	if ds == nil {
		t.Fatal("dataset was not created")
	}
	if got := ds.props[volume.PVCNameProperty]; got != "fileshare" {
		t.Fatalf("%s = %q, want %q", volume.PVCNameProperty, got, "fileshare")
	}
	if got := ds.props[volume.PVCNamespaceProperty]; got != "prod" {
		t.Fatalf("%s = %q, want %q", volume.PVCNamespaceProperty, got, "prod")
	}
	if want := "Kubernetes PVC prod/fileshare (pv-pvc-1)"; ds.comments != want {
		t.Fatalf("comments = %q, want %q", ds.comments, want)
	}
}

// TestSMBCreateWithoutPVCMetadataSucceeds pins the csi-sanity path: absent
// metadata must never fail provisioning or invent appliance-side text.
func TestSMBCreateWithoutPVCMetadataSucceeds(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)

	if _, err := b.Create(context.Background(), testRequest("pvc-1", 10*gib)); err != nil {
		t.Fatalf("Create without PVC metadata: %v", err)
	}

	ds := n.dataset("Pool0/k8s/pvc-1")
	if got, ok := ds.props[volume.PVCNameProperty]; ok {
		t.Fatalf("%s = %q, want the property to be absent entirely", volume.PVCNameProperty, got)
	}
	if ds.comments != "" {
		t.Fatalf("comments = %q — an unlabelled volume must not get invented text", ds.comments)
	}
}

// TestSMBCloneDoesNotInheritSourcePVCIdentity: a restored share belongs to the
// new claim, and a clone wearing its origin's PVC name points an operator at
// the wrong workload.
func TestSMBCloneDoesNotInheritSourcePVCIdentity(t *testing.T) {
	n := newNAS(t)
	n.put("Pool0/k8s/pvc-src", &fakeDataset{
		refquota: gib, marker: volume.OwnerValue, source: "LOCAL",
		props: map[string]string{
			volume.PVCNameProperty:      "source-claim",
			volume.PVCNamespaceProperty: "source-ns",
		},
		comments: "Kubernetes PVC source-ns/source-claim",
	})
	b := newBackend(t, n)

	r := identityRequest("pvc-restored", 4*gib, "restored-claim", "restored-ns")
	r.SourceSnapshot = "Pool0/k8s/pvc-src@snap-1"
	if _, err := b.Create(context.Background(), r); err != nil {
		t.Fatalf("restore: %v", err)
	}

	ds := n.dataset("Pool0/k8s/pvc-restored")
	if ds == nil {
		t.Fatal("clone was not created")
	}
	if got := ds.props[volume.PVCNameProperty]; got != "restored-claim" {
		t.Fatalf("%s = %q, want %q", volume.PVCNameProperty, got, "restored-claim")
	}
	for prop, got := range ds.props {
		if strings.Contains(got, "source-") {
			t.Fatalf("%s = %q — the clone inherited its origin's identity", prop, got)
		}
	}
	if strings.Contains(ds.comments, "source-") {
		t.Fatalf("comments = %q — the clone inherited its origin's description", ds.comments)
	}
}
