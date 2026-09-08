package iscsi

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
func userProp(t *testing.T, ds map[string]any, key string) (string, bool) {
	t.Helper()
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

// TestISCSICreateRecordsPVCIdentity: a zvol named pvc-<uuid> says nothing about
// which workload owns it, and the TrueNAS UI is where an operator goes looking.
func TestISCSICreateRecordsPVCIdentity(t *testing.T) {
	n := newNAS(t)
	b := n.backend()

	req := createReq("pvc-1", 1<<30, identityParams("postgres-data", "prod", "pv-1"))
	if _, err := b.Create(context.Background(), req); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ds := n.dataset("Pool0/k8s/pvc-1")
	if ds == nil {
		t.Fatal("zvol was not created")
	}
	for key, want := range map[string]string{
		volume.PVCNameProperty:      "postgres-data",
		volume.PVCNamespaceProperty: "prod",
		volume.PVNameProperty:       "pv-1",
	} {
		got, ok := userProp(t, ds, key)
		if !ok || got != want {
			t.Fatalf("%s = %q (present=%v), want %q", key, got, ok, want)
		}
	}
	if want := "Kubernetes PVC prod/postgres-data (pv-1)"; comments(ds) != want {
		t.Fatalf("comments = %q, want %q", comments(ds), want)
	}
}

// TestISCSICreateWithoutPVCMetadataSucceeds pins the csi-sanity path: no
// metadata arrives, provisioning must still succeed, and nothing must be
// invented on the appliance.
func TestISCSICreateWithoutPVCMetadataSucceeds(t *testing.T) {
	n := newNAS(t)
	b := n.backend()

	if _, err := b.Create(context.Background(), createReq("pvc-1", 1<<30, nil)); err != nil {
		t.Fatalf("Create without PVC metadata: %v", err)
	}

	ds := n.dataset("Pool0/k8s/pvc-1")
	for _, key := range []string{
		volume.PVCNameProperty, volume.PVCNamespaceProperty, volume.PVNameProperty,
	} {
		if got, ok := userProp(t, ds, key); ok {
			t.Fatalf("%s = %q, want the property to be absent entirely", key, got)
		}
	}
	if comments(ds) != "" {
		t.Fatalf("comments = %q — an unlabelled volume must not get invented text", comments(ds))
	}
}

// TestISCSICloneDoesNotInheritSourcePVCIdentity: a restored volume belongs to
// the NEW claim. A clone wearing its origin's PVC name points an operator at
// the wrong workload — the failure this whole feature exists to prevent.
func TestISCSICloneDoesNotInheritSourcePVCIdentity(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	src := createReq("pvc-src", 1<<30, identityParams("source-claim", "source-ns", "pv-source"))
	if _, err := b.Create(ctx, src); err != nil {
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
	if got, _ := userProp(t, ds, volume.PVCNameProperty); got != "restored-claim" {
		t.Fatalf("%s = %q, want %q", volume.PVCNameProperty, got, "restored-claim")
	}
	if got, _ := userProp(t, ds, volume.PVCNamespaceProperty); got != "restored-ns" {
		t.Fatalf("%s = %q, want %q", volume.PVCNamespaceProperty, got, "restored-ns")
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
