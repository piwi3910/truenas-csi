package nfs

import (
	"context"
	"strings"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/retention"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// protectedBackend is the NFS backend with a week's delete protection, built
// the way the registry builds it, so the test exercises the wiring an operator
// would actually get rather than a policy poked in by hand.
func protectedBackend(t *testing.T, n *nas) (backend.Backend, retention.Policy) {
	t.Helper()
	c, err := truenas.Dial(context.Background(), config.Backend{
		Name: "nas1", Endpoint: n.URL(), Username: "truenas_admin", APIKey: "1-secret",
		Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	p := retention.PolicyFor(config.Backend{
		Pool: "Pool0", ParentDataset: "k8s",
		DeleteProtection: config.DeleteProtection{Enabled: true, GracePeriod: "168h"},
	})
	return New(c, backend.Options{Pool: "Pool0", Parent: "k8s", Retention: p}), p
}

// graveyardEntries returns the datasets currently sitting in the graveyard.
func graveyardEntries(n *nas, p retention.Policy) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for id := range n.datasets {
		if p.ConfineToGraveyard(id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// TestDeleteRetiresInsteadOfDestroying is the feature in one test: the share
// goes, the data does not, and the dataset is no longer where the volume handle
// says it is.
func TestDeleteRetiresInsteadOfDestroying(t *testing.T) {
	n := newNAS(t)
	b, p := protectedBackend(t, n)
	ctx := context.Background()

	if _, err := b.Create(ctx, testRequest("pvc-1", gib)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Delete(ctx, testID("pvc-1")); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if n.dataset("Pool0/k8s/pvc-1") != nil {
		t.Error("the dataset is still where the volume handle names it")
	}
	entries := graveyardEntries(n, p)
	if len(entries) != 1 {
		t.Fatalf("graveyard holds %v, want exactly one retired volume", entries)
	}
	retired := n.dataset(entries[0])
	if retired.marker != volume.OwnerValue {
		t.Errorf("the retired dataset lost its ownership marker (%q); the reaper would refuse it for ever",
			retired.marker)
	}
	if retired.props[volume.DeletedAtProperty] == "" {
		t.Error("the retired dataset has no deletion timestamp; its grace period could never expire")
	}
	if got := retired.props[volume.RetiredFromProperty]; got != "nas1/nfs/Pool0/k8s/pvc-1" {
		t.Errorf("%s = %q, want the original volume handle", volume.RetiredFromProperty, got)
	}

	// The export is gone, which is the part that must happen BEFORE the rename:
	// pool.dataset.rename performs no safety checks and would otherwise leave a
	// live export pointing at a path that no longer exists.
	if n.export("/mnt/Pool0/k8s/pvc-1") != nil {
		t.Error("the NFS export survived the retire")
	}
	if !renameFollowedShareDelete(n.Calls()) {
		t.Errorf("the rename did not follow the share teardown; calls were %v", n.Calls())
	}
}

func renameFollowedShareDelete(calls []string) bool {
	share, rename := -1, -1
	for i, c := range calls {
		switch c {
		case "sharing.nfs.delete":
			share = i
		case "pool.dataset.rename":
			rename = i
		}
	}
	return share >= 0 && rename > share
}

// TestRetiredVolumeSatisfiesCSIDeleteSemantics covers the three promises
// DeleteVolume makes to the CO, all of which delete protection could plausibly
// have broken:
//
//  1. after success the volume id no longer resolves;
//  2. a retried DeleteVolume still succeeds;
//  3. a Create with the same name afterwards makes a FRESH volume rather than
//     resurrecting the retired one.
func TestRetiredVolumeSatisfiesCSIDeleteSemantics(t *testing.T) {
	n := newNAS(t)
	b, p := protectedBackend(t, n)
	ctx := context.Background()

	if _, err := b.Create(ctx, testRequest("pvc-1", gib)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Delete(ctx, testID("pvc-1")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	first := graveyardEntries(n, p)
	if len(first) != 1 {
		t.Fatalf("graveyard holds %v after one delete", first)
	}

	// 1. The id no longer resolves.
	if _, err := b.Expand(ctx, testID("pvc-1"), 2*gib); err == nil {
		t.Error("a retired volume still resolves: Expand succeeded on it")
	}

	// 2. A retried delete is still a success.
	if err := b.Delete(ctx, testID("pvc-1")); err != nil {
		t.Errorf("a retried DeleteVolume failed: %v", err)
	}
	if got := graveyardEntries(n, p); len(got) != 1 {
		t.Errorf("the retry produced a second graveyard entry: %v", got)
	}

	// 3. Creating the same name again makes a fresh volume, at a different size
	//    so a resurrected one would be caught: Create returns AlreadyExists when
	//    an existing dataset's quota differs from the request.
	if _, err := b.Create(ctx, testRequest("pvc-1", 4*gib)); err != nil {
		t.Fatalf("re-creating a retired volume's name: %v", err)
	}
	fresh := n.dataset("Pool0/k8s/pvc-1")
	if fresh == nil {
		t.Fatal("the re-created volume has no dataset")
	}
	if fresh.refquota != 4*gib {
		t.Errorf("the re-created volume has refquota %d, want %d — it resurrected the retired dataset",
			fresh.refquota, 4*gib)
	}
	if fresh.props[volume.DeletedAtProperty] != "" {
		t.Error("the re-created volume carries a deletion timestamp: it IS the retired dataset")
	}
	if got := graveyardEntries(n, p); len(got) != 1 {
		t.Errorf("graveyard holds %v after the re-create; the retired copy must stay put", got)
	}
}

// TestDeleteStillDestroysWithoutProtection pins the default path: nothing about
// the delete changes when an operator has not asked for protection.
func TestDeleteStillDestroysWithoutProtection(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n) // no Retention in its Options
	ctx := context.Background()

	if _, err := b.Create(ctx, testRequest("pvc-1", gib)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Delete(ctx, testID("pvc-1")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n.dataset("Pool0/k8s/pvc-1") != nil {
		t.Error("the dataset survived a delete with protection off")
	}
	for _, unwanted := range []string{"pool.dataset.rename"} {
		if n.CallsTo(unwanted) != 0 {
			t.Errorf("protection is off but %s was called", unwanted)
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for id := range n.datasets {
		if strings.Contains(id, config.DefaultGraveyardDataset) {
			t.Errorf("protection is off but a graveyard dataset %q was created", id)
		}
	}
}

// TestDeleteRefusesToRetireADatasetItDoesNotOwn: the ownership guard runs
// BEFORE disposal, so an unowned dataset is neither destroyed nor moved. Moving
// it would be almost as bad as destroying it — the operator's data would vanish
// from where they put it.
func TestDeleteRefusesToRetireADatasetItDoesNotOwn(t *testing.T) {
	n := newNAS(t)
	b, p := protectedBackend(t, n)
	n.put("Pool0/k8s/pvc-1", &fakeDataset{refquota: gib}) // no ownership marker

	err := b.Delete(context.Background(), testID("pvc-1"))
	if err == nil {
		t.Fatal("Delete accepted a dataset this driver never created")
	}
	if n.dataset("Pool0/k8s/pvc-1") == nil {
		t.Error("an unowned dataset was moved out from under its owner")
	}
	if got := graveyardEntries(n, p); len(got) != 0 {
		t.Errorf("an unowned dataset was retired into the graveyard: %v", got)
	}
}
