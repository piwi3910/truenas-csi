package nvme

import (
	"context"
	"strings"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/volume"
)

// identityParams is what csi-provisioner adds to CreateVolume parameters when
// the deployment passes --extra-create-metadata, which this driver's chart does.
func identityParams(pvc, ns, pv string) map[string]string {
	return map[string]string{
		volume.ParamPVCName:      pvc,
		volume.ParamPVCNamespace: ns,
		volume.ParamPVName:       pv,
	}
}

// userProp reads one user property off the fake's dataset representation.
func userProp(ds map[string]any, key string) (string, bool) {
	props, _ := ds["user_properties"].(map[string]any)
	p, ok := props[key].(map[string]any)
	if !ok {
		return "", false
	}
	v, _ := p["value"].(string)
	return v, true
}

func comments(ds map[string]any) string {
	c, _ := ds["comments"].(map[string]any)
	v, _ := c["value"].(string)
	return v
}

// TestNVMeCreateRecordsPVCIdentity: an NVMe namespace backed by a pvc-<uuid>
// zvol says nothing about which workload owns it.
func TestNVMeCreateRecordsPVCIdentity(t *testing.T) {
	n := newNAS(t)
	b := n.backend()

	req := createReq("pvc-1", 1<<30, identityParams("cache", "prod", "pv-1"))
	if _, err := b.Create(context.Background(), req); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ds := n.dataset("Pool0/k8s/pvc-1")
	if ds == nil {
		t.Fatal("zvol was not created")
	}
	for key, want := range map[string]string{
		volume.PVCNameProperty:      "cache",
		volume.PVCNamespaceProperty: "prod",
		volume.PVNameProperty:       "pv-1",
	} {
		got, ok := userProp(ds, key)
		if !ok || got != want {
			t.Fatalf("%s = %q (present=%v), want %q", key, got, ok, want)
		}
	}
	if want := "Kubernetes PVC prod/cache (pv-1)"; comments(ds) != want {
		t.Fatalf("comments = %q, want %q", comments(ds), want)
	}
}

// TestNVMeCreateWithoutPVCMetadataSucceeds pins the csi-sanity path: absent
// metadata must never fail provisioning or invent appliance-side text.
func TestNVMeCreateWithoutPVCMetadataSucceeds(t *testing.T) {
	n := newNAS(t)
	b := n.backend()

	if _, err := b.Create(context.Background(), createReq("pvc-1", 1<<30, nil)); err != nil {
		t.Fatalf("Create without PVC metadata: %v", err)
	}

	ds := n.dataset("Pool0/k8s/pvc-1")
	for _, key := range []string{
		volume.PVCNameProperty, volume.PVCNamespaceProperty, volume.PVNameProperty,
	} {
		if got, ok := userProp(ds, key); ok {
			t.Fatalf("%s = %q, want the property to be absent entirely", key, got)
		}
	}
	if comments(ds) != "" {
		t.Fatalf("comments = %q — an unlabelled volume must not get invented text", comments(ds))
	}
}

// TestNVMeCloneDoesNotInheritSourcePVCIdentity: a restored volume belongs to
// the NEW claim, and a clone wearing its origin's PVC name points an operator
// at the wrong workload.
func TestNVMeCloneDoesNotInheritSourcePVCIdentity(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	if _, err := b.Create(ctx, createReq("pvc-src", 1<<30,
		identityParams("source-claim", "source-ns", "pv-source"))); err != nil {
		t.Fatalf("Create source: %v", err)
	}

	req := createReq("pvc-restored", 2<<30, identityParams("restored-claim", "restored-ns", "pv-restored"))
	req.SourceSnapshot = "Pool0/k8s/pvc-src@snap1"
	if _, err := b.Create(ctx, req); err != nil {
		t.Fatalf("restore: %v", err)
	}

	ds := n.dataset("Pool0/k8s/pvc-restored")
	if ds == nil {
		t.Fatal("the clone was not created")
	}
	if got, _ := userProp(ds, volume.PVCNameProperty); got != "restored-claim" {
		t.Fatalf("%s = %q, want %q", volume.PVCNameProperty, got, "restored-claim")
	}
	props, _ := ds["user_properties"].(map[string]any)
	for key, raw := range props {
		p, _ := raw.(map[string]any)
		if v, _ := p["value"].(string); strings.Contains(v, "source") {
			t.Fatalf("%s = %q — the clone inherited its origin's identity", key, v)
		}
	}
	if strings.Contains(comments(ds), "source") {
		t.Fatalf("comments = %q — the clone inherited its origin's description", comments(ds))
	}
}

// TestRestoreStampsProtocol pins the protocol marker onto the clone path. Only
// the create path stamped it, so a cloned volume reached the appliance without
// one, and both readers that consult it -- the orphan reconciler and
// per-protocol capacity accounting -- fall back to guessing from the dataset
// type. A VOLUME guesses "iscsi", so every cloned nvme volume was reported
// under an iscsi handle that names nothing.
func TestRestoreStampsProtocol(t *testing.T) {
	ctx := context.Background()
	n := newNAS(t)
	b := n.backend()

	if _, err := b.Create(ctx, createReq("pvc-src", 1<<30, nil)); err != nil {
		t.Fatalf("Create source: %v", err)
	}
	req := createReq("pvc-restored", 1<<30, nil)
	req.SourceSnapshot = "Pool0/k8s/pvc-src@snap1"
	if _, err := b.Create(ctx, req); err != nil {
		t.Fatalf("restore: %v", err)
	}
	ds := n.dataset("Pool0/k8s/pvc-restored")
	if ds == nil {
		t.Fatal("the clone was not created")
	}
	if got, _ := userProp(ds, volume.ProtocolProperty); got != Protocol {
		t.Fatalf("clone protocol property = %q, want %q", got, Protocol)
	}
}
