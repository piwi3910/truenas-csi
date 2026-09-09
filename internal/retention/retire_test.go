package retention

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

func vol(name string) volume.ID {
	return volume.ID{Backend: "nas1", Protocol: "nfs", Pool: "Pool0", Parent: "k8s", Name: name}
}

func nsVol(ns, name string) volume.ID {
	id := vol(name)
	id.Namespace = ns
	return id
}

// TestDisposeDestroysWhenProtectionIsOff pins the default. A grace period of
// zero and the feature being off must both take the ORIGINAL code path — the
// caller's destroy callback and nothing else — because silently changing what
// `kubectl delete pvc` means is the surprise this feature must not spring.
func TestDisposeDestroysWhenProtectionIsOff(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy Policy
	}{
		{"the zero policy", Policy{}},
		{"enabled with a zero grace period", testPolicy("0s")},
		{"enabled with an unparsable grace period", testPolicy("a fortnight")},
		{"a grace period but no pool", Policy{Parent: "k8s", Graveyard: ".trash", Grace: time.Hour}},
		{"a grace period but no graveyard name", Policy{Pool: "Pool0", Parent: "k8s", Grace: time.Hour}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newNAS(t)
			n.put("Pool0/k8s/pvc-1", map[string]string{volume.OwnerProperty: volume.OwnerValue})

			called := 0
			err := Dispose(context.Background(), n.client(t), tc.policy, vol("pvc-1"),
				func(context.Context) error { called++; return nil })
			if err != nil {
				t.Fatalf("Dispose: %v", err)
			}
			if called != 1 {
				t.Errorf("destroy callback called %d times, want exactly 1", called)
			}
			if got := n.CallsTo("pool.dataset.rename"); got != 0 {
				t.Errorf("delete protection is off but %d rename(s) were issued", got)
			}
			if got := n.CallsTo("pool.dataset.create"); got != 0 {
				t.Errorf("delete protection is off but a graveyard dataset was created")
			}
		})
	}
}

// TestRetireRenamesAndStamps is the happy path: the dataset leaves the location
// its volume handle names, arrives in the graveyard, and carries the evidence
// the reaper will demand of it.
func TestRetireRenamesAndStamps(t *testing.T) {
	n := newNAS(t)
	p := testPolicy(week)
	client := n.client(t)
	deletedAt := time.Date(2026, 9, 8, 10, 15, 0, 0, time.UTC)

	n.put("Pool0/k8s/pvc-1", map[string]string{
		volume.OwnerProperty:    volume.OwnerValue,
		volume.ProtocolProperty: "nfs",
	})

	if err := Retire(context.Background(), client, p, vol("pvc-1"), func() time.Time { return deletedAt }); err != nil {
		t.Fatalf("Retire: %v", err)
	}

	if n.has("Pool0/k8s/pvc-1") {
		t.Fatal("the volume's dataset is still where its handle names it")
	}
	want := p.Root() + "/20260908T101500Z-pvc-1"
	if !n.has(want) {
		t.Fatalf("dataset was not renamed to %s; the box holds %v", want, n.ids())
	}

	ds, err := client.DatasetQuery(context.Background(), want)
	if err != nil || ds == nil {
		t.Fatalf("query retired dataset: %v", err)
	}
	// The ownership marker survives the rename untouched, which is why Retire
	// does not re-stamp it: pool.dataset.rename moves the dataset's properties
	// with it.
	if got := ds.LocalProperty(volume.OwnerProperty); got != volume.OwnerValue {
		t.Errorf("ownership marker is %q after the rename, want %q", got, volume.OwnerValue)
	}
	if got := ds.LocalProperty(volume.DeletedAtProperty); got != "2026-09-08T10:15:00Z" {
		t.Errorf("%s = %q", volume.DeletedAtProperty, got)
	}
	if got := ds.LocalProperty(volume.RetiredFromProperty); got != "nas1/nfs/Pool0/k8s/pvc-1" {
		t.Errorf("%s = %q, want the original volume handle", volume.RetiredFromProperty, got)
	}

	// And the retired dataset is reapable once, and only once, a week has gone.
	if err := Reapable(ds, p, deletedAt.Add(6*24*time.Hour)); err == nil {
		t.Error("a six-day-old retired volume must not be reapable under a week's grace")
	}
	if err := Reapable(ds, p, deletedAt.Add(8*24*time.Hour)); err != nil {
		t.Errorf("an eight-day-old retired volume must be reapable: %v", err)
	}
}

// TestRetireCreatesTheGraveyardOnceAndMarksIt checks the container the reaper
// must never mistake for its contents.
func TestRetireCreatesTheGraveyardOnceAndMarksIt(t *testing.T) {
	n := newNAS(t)
	p := testPolicy(week)
	client := n.client(t)
	at := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)

	for i, name := range []string{"pvc-a", "pvc-b"} {
		n.put("Pool0/k8s/"+name, map[string]string{volume.OwnerProperty: volume.OwnerValue})
		when := at.Add(time.Duration(i) * time.Minute)
		if err := Retire(context.Background(), client, p, vol(name), func() time.Time { return when }); err != nil {
			t.Fatalf("Retire %s: %v", name, err)
		}
	}
	if got := n.CallsTo("pool.dataset.create"); got != 1 {
		t.Errorf("the graveyard was created %d times, want once", got)
	}

	root, err := client.DatasetQuery(context.Background(), p.Root())
	if err != nil || root == nil {
		t.Fatalf("query graveyard: %v", err)
	}
	if !volume.IsGraveyard(root.LocalProperty(volume.GraveyardProperty)) {
		t.Error("the graveyard does not carry its own marker, so nothing can tell it from a volume")
	}
	// The container is driver-owned AND past every conceivable grace period,
	// which is exactly why it needs a check of its own.
	if err := Reapable(root, p, at.Add(365*24*time.Hour)); err == nil {
		t.Fatal("the graveyard itself is reapable — destroying it would take every retired volume with it")
	}
}

// TestRetireRefusesAnAlienGraveyard: adopting a dataset the driver did not
// create would put datasets it later destroys inside somebody else's.
func TestRetireRefusesAnAlienGraveyard(t *testing.T) {
	n := newNAS(t)
	p := testPolicy(week)
	n.put(p.Root(), nil) // an operator's dataset that happens to sit there
	n.put("Pool0/k8s/pvc-1", map[string]string{volume.OwnerProperty: volume.OwnerValue})

	err := Retire(context.Background(), n.client(t), p, vol("pvc-1"), time.Now)
	if err == nil {
		t.Fatal("Retire adopted a dataset it did not create as its graveyard")
	}
	if !strings.Contains(err.Error(), "is not this driver's graveyard") {
		t.Errorf("unhelpful refusal: %v", err)
	}
	if !n.has("Pool0/k8s/pvc-1") {
		t.Error("the volume was moved into a graveyard the driver had just refused")
	}
}

// TestRetireGivesEachVolumeItsOwnGraveyardEntry covers the collision the
// timestamp alone cannot rule out: two namespaced volumes retired in the same
// second whose names sanitise to the same string.
func TestRetireGivesEachVolumeItsOwnGraveyardEntry(t *testing.T) {
	n := newNAS(t)
	p := testPolicy(week)
	client := n.client(t)
	at := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)

	ids := []volume.ID{nsVol("team", "a-pvc-1"), nsVol("team-a", "pvc-1")}
	for _, id := range ids {
		n.put(id.DatasetPath(), map[string]string{volume.OwnerProperty: volume.OwnerValue})
		if err := Retire(context.Background(), client, p, id, func() time.Time { return at }); err != nil {
			t.Fatalf("Retire %s: %v", id, err)
		}
	}

	entries := map[string]string{}
	for _, dsID := range n.ids() {
		if p.ConfineToGraveyard(dsID) != nil {
			continue
		}
		ds, err := client.DatasetQuery(context.Background(), dsID)
		if err != nil || ds == nil {
			t.Fatalf("query %s: %v", dsID, err)
		}
		entries[dsID] = ds.LocalProperty(volume.RetiredFromProperty)
	}
	if len(entries) != 2 {
		t.Fatalf("two retired volumes produced %d graveyard entries: %v", len(entries), entries)
	}
	seen := map[string]bool{}
	for _, from := range entries {
		if from == "" {
			t.Error("a graveyard entry has no record of the volume it was")
		}
		if seen[from] {
			t.Errorf("two graveyard entries claim to be %s", from)
		}
		seen[from] = true
	}
}

// TestRetireRefusesAVolumeOutsideTheConfiguredParent: Retire re-confines rather
// than trusting its caller, because it is the last check before a rename that
// moves data.
func TestRetireRefusesAVolumeOutsideTheConfiguredParent(t *testing.T) {
	n := newNAS(t)
	p := testPolicy(week)
	alien := volume.ID{Backend: "nas1", Protocol: "nfs", Pool: "Pool0", Parent: "Home", Name: "photos"}
	n.put(alien.DatasetPath(), map[string]string{volume.OwnerProperty: volume.OwnerValue})

	if err := Retire(context.Background(), n.client(t), p, alien, time.Now); err == nil {
		t.Fatal("Retire moved a dataset outside the configured parent")
	}
	if got := n.CallsTo("pool.dataset.rename"); got != 0 {
		t.Errorf("%d rename(s) were issued for a dataset outside the parent", got)
	}
}

// TestRetireRetriesWhileTheZvolIsStillBusy mirrors the delete path: removing an
// extent does not release the device immediately, so the first rename after
// teardown can legitimately fail EBUSY.
func TestRetireRetriesWhileTheZvolIsStillBusy(t *testing.T) {
	n := newNAS(t)
	p := testPolicy(week)
	n.put("Pool0/k8s/pvc-1", map[string]string{volume.OwnerProperty: volume.OwnerValue})
	n.mu.Lock()
	n.renameFail = &fake.RPCError{Code: -32001, ErrName: "EBUSY", Reason: "[EBUSY] dataset is busy"}
	n.mu.Unlock()

	at := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	if err := Retire(context.Background(), n.client(t), p, vol("pvc-1"), func() time.Time { return at }); err != nil {
		t.Fatalf("Retire gave up on a transient EBUSY: %v", err)
	}
	if !n.has(p.Root() + "/20260908T100000Z-pvc-1") {
		t.Fatal("the retry did not land the dataset in the graveyard")
	}
}

// TestRetireForcesTheRename pins that the rename passes force, which is the
// opposite of what this test asserted when it was written.
//
// The middleware's warning ("No safety checks are performed... may cause
// disruptions or service failures") reads like a conditional gate that would
// pass on an idle dataset, and the original reasoning followed from that: a
// refusal means teardown did not finish, so never force. Verified against the
// appliance, that is simply wrong. 25.10 refuses EVERY rename without force —
// a freshly retired dataset with no share, no extent and nothing holding it is
// refused with
//
//	[EINVAL] pool.dataset.rename.force: ... please set force and proceed
//
// so passing false does not make the driver careful, it makes delete protection
// fail 100% of the time. The feature was completely non-functional and every
// unit test passed, because the fake accepted what the appliance does not.
//
// Safety comes from ORDER instead: DeleteVolume removes the share, the extent
// and the target mapping before Retire is reached, so there is deliberately
// nothing left for the rename to break.
func TestRetireForcesTheRename(t *testing.T) {
	n := newNAS(t)
	p := testPolicy(week)
	n.put("Pool0/k8s/pvc-1", map[string]string{volume.OwnerProperty: volume.OwnerValue})

	if err := Retire(context.Background(), n.client(t), p, vol("pvc-1"), time.Now); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	forced := n.renameForces()
	if len(forced) != 1 {
		t.Fatalf("expected exactly one rename, saw %d", len(forced))
	}
	if !forced[0] {
		t.Error("the rename was not forced, so the appliance would refuse it and delete " +
			"protection would fail every time: 25.10 requires force on every rename")
	}
}
