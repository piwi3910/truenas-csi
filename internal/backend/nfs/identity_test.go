package nfs

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

// TestNFSCreateRecordsPVCIdentity: without this an operator sees a pile of
// pvc-<uuid> datasets in the UI with nothing saying which workload owns any of
// them, which is the entire point of the properties.
func TestNFSCreateRecordsPVCIdentity(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)

	req := identityRequest("pvc-1", 10*gib, "postgres-data", "prod")
	if _, err := b.Create(context.Background(), req); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ds := n.dataset("Pool0/k8s/pvc-1")
	if ds == nil {
		t.Fatal("dataset was not created")
	}
	for prop, want := range map[string]string{
		volume.PVCNameProperty:      "postgres-data",
		volume.PVCNamespaceProperty: "prod",
		volume.PVNameProperty:       "pv-pvc-1",
	} {
		if got := ds.props[prop]; got != want {
			t.Fatalf("%s = %q, want %q", prop, got, want)
		}
	}
	// comments is the only free-text field the TrueNAS UI shows for a dataset,
	// so it is where an operator actually looks.
	if want := "Kubernetes PVC prod/postgres-data (pv-pvc-1)"; ds.comments != want {
		t.Fatalf("comments = %q, want %q", ds.comments, want)
	}
}

// TestNFSCreateWithoutPVCMetadataSucceeds: csi-sanity, static provisioning and
// any deployment without --extra-create-metadata send no metadata at all.
// Missing identity must be ordinary, not a provisioning failure, and must not
// cost a middleware call.
func TestNFSCreateWithoutPVCMetadataSucceeds(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)

	if _, err := b.Create(context.Background(), testRequest("pvc-1", 10*gib)); err != nil {
		t.Fatalf("Create without PVC metadata: %v", err)
	}

	ds := n.dataset("Pool0/k8s/pvc-1")
	if ds == nil {
		t.Fatal("dataset was not created")
	}
	for _, prop := range []string{
		volume.PVCNameProperty, volume.PVCNamespaceProperty, volume.PVNameProperty,
	} {
		if got, ok := ds.props[prop]; ok {
			t.Fatalf("%s = %q, want the property to be absent entirely", prop, got)
		}
	}
	if ds.comments != "" {
		t.Fatalf("comments = %q — an unlabelled volume must not get invented text", ds.comments)
	}
	if got := n.CallsTo("pool.dataset.update"); got != 0 {
		t.Fatalf("pool.dataset.update called %d times with nothing to record, want 0", got)
	}
}

// TestNFSCreateIgnoresIdentityOnRepeat: CreateVolume is retried, and a repeat
// naming a different claim means the same handle is being claimed twice. The
// first writer's record is exactly what an operator needs at that moment, so it
// is neither overwritten nor turned into a refusal.
func TestNFSCreateIgnoresIdentityOnRepeat(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)
	ctx := context.Background()

	if _, err := b.Create(ctx, identityRequest("pvc-1", 10*gib, "first", "prod")); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := b.Create(ctx, identityRequest("pvc-1", 10*gib, "second", "staging")); err != nil {
		t.Fatalf("a repeat with different metadata must still succeed: %v", err)
	}

	ds := n.dataset("Pool0/k8s/pvc-1")
	if got := ds.props[volume.PVCNameProperty]; got != "first" {
		t.Fatalf("%s = %q, want %q — the birth record must survive a repeat",
			volume.PVCNameProperty, got, "first")
	}
	if got := ds.props[volume.PVCNamespaceProperty]; got != "prod" {
		t.Fatalf("%s = %q, want %q", volume.PVCNamespaceProperty, got, "prod")
	}
}

// TestNFSCloneDoesNotInheritSourcePVCIdentity is the regression this feature is
// most likely to grow: a restored volume belongs to the NEW claim, and a clone
// carrying its origin's PVC name sends an operator to the wrong workload.
func TestNFSCloneDoesNotInheritSourcePVCIdentity(t *testing.T) {
	n := newNAS(t)
	n.put("Pool0/k8s/pvc-src", &fakeDataset{
		refquota: gib, marker: volume.OwnerValue, source: "LOCAL",
		props: map[string]string{
			volume.PVCNameProperty:      "source-claim",
			volume.PVCNamespaceProperty: "source-ns",
			volume.PVNameProperty:       "pv-source",
		},
		comments: "Kubernetes PVC source-ns/source-claim (pv-source)",
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
	if got := ds.props[volume.PVCNamespaceProperty]; got != "restored-ns" {
		t.Fatalf("%s = %q, want %q", volume.PVCNamespaceProperty, got, "restored-ns")
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

// TestNFSCloneWithoutPVCMetadataCarriesNoIdentity: a restore that arrives with
// no metadata must end up unlabelled rather than wearing its origin's label.
func TestNFSCloneWithoutPVCMetadataCarriesNoIdentity(t *testing.T) {
	n := newNAS(t)
	n.put("Pool0/k8s/pvc-src", &fakeDataset{
		refquota: gib, marker: volume.OwnerValue, source: "LOCAL",
		props:    map[string]string{volume.PVCNameProperty: "source-claim"},
		comments: "Kubernetes PVC source-ns/source-claim",
	})
	b := newBackend(t, n)

	r := testRequest("pvc-restored", 4*gib)
	r.SourceSnapshot = "Pool0/k8s/pvc-src@snap-1"
	if _, err := b.Create(context.Background(), r); err != nil {
		t.Fatalf("restore without PVC metadata: %v", err)
	}

	ds := n.dataset("Pool0/k8s/pvc-restored")
	if got, ok := ds.props[volume.PVCNameProperty]; ok {
		t.Fatalf("%s = %q, want the property to be absent", volume.PVCNameProperty, got)
	}
	if ds.comments != "" {
		t.Fatalf("comments = %q, want empty", ds.comments)
	}
}
