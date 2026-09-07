package replication

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/volume"
)

// setup creates a group whose members exist and are driver-owned on both
// appliances, and returns the created group.
func setup(t *testing.T, m *Manager, src, dst *nas, vols ...string) Group {
	t.Helper()
	g := testGroup(vols...)
	for _, v := range vols {
		src.putOwned("Pool0/k8s/" + v)
		dst.putOwned("Pool1/k8s/" + v)
	}
	if _, err := m.Create(context.Background(), g); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return g
}

// TestGroupRefusesUnownedVolumes catches the worst mistake this package could
// make: accepting a dataset the driver did not create. A replication task
// OVERWRITES its target, so a group that names someone's real dataset would
// destroy it on the first run — and would do so on a schedule.
func TestGroupRefusesUnownedVolumes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		place func(n *nas, id string)
		want  string
	}{
		{"foreign dataset", (*nas).putForeign, "has no io.truenas.csi:managed"},
		{"inherited marker", (*nas).putInherited, "inherited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, _, m := pair(t)
			src.putOwned("Pool0/k8s/pvc-1")
			tc.place(src, "Pool0/k8s/pvc-2")

			g := testGroup("pvc-1", "pvc-2")
			_, err := m.Create(context.Background(), g)
			if !errors.Is(err, volume.ErrNotManaged) {
				t.Fatalf("Create error = %v, want volume.ErrNotManaged", err)
			}
			if !strings.Contains(err.Error(), "pvc-2") {
				t.Fatalf("error %q does not name the offending dataset", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not explain why (%s)", err, tc.want)
			}
			if _, _, replCreates, _, _ := src.counts(); replCreates != 0 {
				t.Fatalf("a replication task was created for a group with %d unowned member(s)", replCreates)
			}
		})
	}
}

// TestFailoverIsIdempotent catches a repeated failover promoting the target a
// second time. Two promotions of one group is a split brain: two appliances
// both believing they are primary, with writes landing on each.
func TestFailoverIsIdempotent(t *testing.T) {
	src, dst, m := pair(t)
	g := setup(t, m, src, dst, "pvc-1")
	dst.addSnapshot("Pool1/k8s/pvc-1", "csi-group1-2026-09-07_10-00")

	first, err := m.Failover(context.Background(), g, ActionOptions{})
	if err != nil {
		t.Fatalf("first Failover: %v", err)
	}
	if first.Phase != PhaseFailedOver {
		t.Fatalf("phase after failover = %q, want %q", first.Phase, PhaseFailedOver)
	}

	second, err := m.Failover(context.Background(), g, ActionOptions{})
	if err != nil {
		t.Fatalf("second Failover must be a no-op, got %v", err)
	}
	if second.Phase != PhaseFailedOver {
		t.Fatalf("phase after second failover = %q, want %q", second.Phase, PhaseFailedOver)
	}

	if got := dst.readonlyWrites("Pool1/k8s/pvc-1"); got != 1 {
		t.Fatalf("target promoted %d times, want exactly 1 — a second promotion is a split brain", got)
	}
	if _, promotions, _, _, _ := dst.counts(); promotions != 0 {
		t.Fatalf("pool.dataset.promote called %d times on a replication target that is not a clone", promotions)
	}
	disables := 0
	src.mu.Lock()
	for _, u := range src.replUpdates {
		if e, ok := u.Patch["enabled"].(bool); ok && !e {
			disables++
		}
	}
	src.mu.Unlock()
	if disables != 1 {
		t.Fatalf("source replication task disabled %d times, want exactly 1", disables)
	}
}

// TestFailoverRefusesAfterAnIncompleteOne catches a failover restarted while an
// earlier one is unfinished — the other route to two primaries.
func TestFailoverRefusesAfterAnIncompleteOne(t *testing.T) {
	src, dst, m := pair(t)
	g := setup(t, m, src, dst, "pvc-1")
	dst.addSnapshot("Pool1/k8s/pvc-1", "snap-1")

	if err := m.store.Save(context.Background(), g.Name, &State{Phase: PhaseFailingOver, TaskID: 1}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := m.Failover(context.Background(), g, ActionOptions{}); !errors.Is(err, ErrFailoverIncomplete) {
		t.Fatalf("Failover error = %v, want ErrFailoverIncomplete", err)
	}
	if got := dst.readonlyWrites("Pool1/k8s/pvc-1"); got != 0 {
		t.Fatalf("target was promoted %d times despite the refusal", got)
	}
	if _, err := m.Failover(context.Background(), g, ActionOptions{Force: true}); err != nil {
		t.Fatalf("forced Failover: %v", err)
	}
}

// TestTestFailoverDoesNotPromoteTarget is the property that makes a rehearsal
// safe to run on a Tuesday afternoon: it must clone the last replicated
// snapshot into scratch space and leave production — the target datasets and
// the replication task feeding them — exactly as it found them.
func TestTestFailoverDoesNotPromoteTarget(t *testing.T) {
	src, dst, m := pair(t)
	g := setup(t, m, src, dst, "pvc-1")
	dst.addSnapshot("Pool1/k8s/pvc-1", "csi-group1-old")
	dst.addSnapshot("Pool1/k8s/pvc-1", "csi-group1-latest")

	srcDeletes, srcPromotions, srcCreates, srcUpdates, srcRuns := src.counts()

	st, err := m.TestFailover(context.Background(), g)
	if err != nil {
		t.Fatalf("TestFailover: %v", err)
	}

	// The rehearsal must have produced something usable.
	if st.TestFailover == nil || st.TestFailover.Datasets["pvc-1"] == "" {
		t.Fatalf("TestFailover exposed no scratch dataset: %+v", st.TestFailover)
	}
	scratch := st.TestFailover.Datasets["pvc-1"]
	if ds := dst.dataset(scratch); ds == nil {
		t.Fatalf("scratch dataset %q was not created", scratch)
	} else if ds.marker != volume.OwnerValue || ds.source != "LOCAL" {
		t.Fatalf("scratch dataset %q is not stamped as driver-owned (%+v) and could never be cleaned up", scratch, ds)
	}
	if got := st.TestFailover.Snapshots["pvc-1"]; got != "csi-group1-latest" {
		t.Fatalf("cloned snapshot = %q, want the latest replicated one", got)
	}

	// Production on the target must be untouched.
	if _, promotions, _, _, _ := dst.counts(); promotions != 0 {
		t.Fatalf("a test failover called pool.dataset.promote %d times — it promoted the real target", promotions)
	}
	if got := dst.readonlyWrites("Pool1/k8s/pvc-1"); got != 0 {
		t.Fatalf("a test failover cleared readonly on the production target %d times", got)
	}
	if got := dst.dataset("Pool1/k8s/pvc-1").readonly; got != "ON" {
		t.Fatalf("production target readonly = %q after a test failover, want ON", got)
	}
	for _, id := range dst.datasetDeletes {
		if !strings.HasPrefix(id, scratchRoot(g, "Pool1/k8s")) {
			t.Fatalf("a test failover deleted production dataset %q", id)
		}
	}

	// And the production replication stream must be untouched.
	deletes, promotions, creates, updates, runs := src.counts()
	if deletes != srcDeletes || promotions != srcPromotions || creates != srcCreates ||
		updates != srcUpdates || runs != srcRuns {
		t.Fatalf("a test failover changed the source appliance: deletes %d->%d promotions %d->%d "+
			"replCreates %d->%d replUpdates %d->%d replRuns %d->%d",
			srcDeletes, deletes, srcPromotions, promotions, srcCreates, creates,
			srcUpdates, updates, srcRuns, runs)
	}
	if task := src.replTaskByName(taskName(g)); task == nil || !task.enabled {
		t.Fatalf("the production replication task is gone or disabled after a test failover: %+v", task)
	}
	if st.Phase != PhaseReady {
		t.Fatalf("phase after a test failover = %q, want it unchanged at %q", st.Phase, PhaseReady)
	}
}

// TestFailbackRefusesOnDivergence catches a failback that would silently
// discard writes made on the old source after failover. Reverse replication
// rolls the source back to the target's snapshot stream; anything written in
// between is gone, so the operator has to be told what would be lost.
func TestFailbackRefusesOnDivergence(t *testing.T) {
	src, dst, m := pair(t)
	g := setup(t, m, src, dst, "pvc-1")
	src.addSnapshot("Pool0/k8s/pvc-1", "csi-group1-common")
	dst.addSnapshot("Pool1/k8s/pvc-1", "csi-group1-common")

	if _, err := m.Failover(context.Background(), g, ActionOptions{}); err != nil {
		t.Fatalf("Failover: %v", err)
	}

	// Someone wrote to the old source and snapshotted it after failover.
	src.addSnapshot("Pool0/k8s/pvc-1", "rogue-2026-09-08")

	_, err := m.Failback(context.Background(), g, ActionOptions{})
	if !errors.Is(err, ErrDiverged) {
		t.Fatalf("Failback error = %v, want ErrDiverged", err)
	}
	if !strings.Contains(err.Error(), "pvc-1") || !strings.Contains(err.Error(), "rogue-2026-09-08") {
		t.Fatalf("error %q does not say what diverged", err)
	}
	if _, _, replCreates, _, _ := dst.counts(); replCreates != 0 {
		t.Fatalf("a reverse replication task was created despite the refusal")
	}
	st, err := m.store.Load(context.Background(), g.Name)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.Phase != PhaseFailedOver {
		t.Fatalf("phase after a refused failback = %q, want %q", st.Phase, PhaseFailedOver)
	}

	forced, err := m.Failback(context.Background(), g, ActionOptions{Force: true})
	if err != nil {
		t.Fatalf("forced Failback: %v", err)
	}
	if forced.Phase != PhaseReady {
		t.Fatalf("phase after a forced failback = %q, want %q", forced.Phase, PhaseReady)
	}
	if _, _, replCreates, _, _ := dst.counts(); replCreates != 1 {
		t.Fatalf("forced failback created %d reverse replication tasks, want 1", replCreates)
	}
}

// TestSuspendResumeAreIdempotent catches suspend/resume that issue a write
// every time they are called. The controller re-reconciles on every watch
// event, so a non-level-triggered suspend would hammer the middleware forever.
func TestSuspendResumeAreIdempotent(t *testing.T) {
	src, dst, m := pair(t)
	g := setup(t, m, src, dst, "pvc-1")

	for i := 0; i < 3; i++ {
		st, err := m.Suspend(context.Background(), g)
		if err != nil {
			t.Fatalf("Suspend %d: %v", i, err)
		}
		if st.Phase != PhaseSuspended || !st.Suspended {
			t.Fatalf("Suspend %d left phase %q suspended=%v", i, st.Phase, st.Suspended)
		}
	}
	if got := countEnabledWrites(src, false); got != 1 {
		t.Fatalf("three Suspend calls issued %d disabling replication.update calls, want 1", got)
	}

	for i := 0; i < 3; i++ {
		st, err := m.Resume(context.Background(), g)
		if err != nil {
			t.Fatalf("Resume %d: %v", i, err)
		}
		if st.Phase != PhaseReady || st.Suspended {
			t.Fatalf("Resume %d left phase %q suspended=%v", i, st.Phase, st.Suspended)
		}
	}
	if got := countEnabledWrites(src, true); got != 1 {
		t.Fatalf("three Resume calls issued %d enabling replication.update calls, want 1", got)
	}
}

func countEnabledWrites(n *nas, want bool) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	count := 0
	for _, u := range n.replUpdates {
		if e, ok := u.Patch["enabled"].(bool); ok && e == want {
			count++
		}
	}
	return count
}

// TestReplicationNeverDeletesForeignDatasets is the same guarantee the volume
// backends give, extended to the target appliance: replication tears down only
// what it created. A group teardown that destroyed the operator's data on the
// remote appliance would be unrecoverable.
func TestReplicationNeverDeletesForeignDatasets(t *testing.T) {
	t.Run("group teardown", func(t *testing.T) {
		src, dst, m := pair(t)
		g := setup(t, m, src, dst, "pvc-1")
		// The remote path now holds something the driver did not create.
		dst.putForeign("Pool1/k8s/pvc-1")

		err := m.Delete(context.Background(), g, DeleteOptions{RemoveTargetDatasets: true})
		if !errors.Is(err, volume.ErrNotManaged) {
			t.Fatalf("Delete error = %v, want volume.ErrNotManaged", err)
		}
		if deletes, _, _, _, _ := dst.counts(); deletes != 0 {
			t.Fatalf("%d dataset(s) were destroyed on the target despite the refusal", deletes)
		}
	})

	t.Run("test failover teardown", func(t *testing.T) {
		src, dst, m := pair(t)
		g := setup(t, m, src, dst, "pvc-1")
		dst.addSnapshot("Pool1/k8s/pvc-1", "csi-group1-latest")

		st, err := m.TestFailover(context.Background(), g)
		if err != nil {
			t.Fatalf("TestFailover: %v", err)
		}
		// An operator replaced the scratch clone with a real dataset.
		dst.putForeign(st.TestFailover.Datasets["pvc-1"])

		if _, err := m.StopTestFailover(context.Background(), g); !errors.Is(err, volume.ErrNotManaged) {
			t.Fatalf("StopTestFailover error = %v, want volume.ErrNotManaged", err)
		}
		if deletes, _, _, _, _ := dst.counts(); deletes != 0 {
			t.Fatalf("%d dataset(s) were destroyed despite the refusal", deletes)
		}
	})
}

// TestCreateRemoteVolumeStampsOwnership catches a remote dataset created
// without the ownership marker. ZFS clones and remote datasets do not inherit
// it, and an unmarked dataset can never be cleaned up by the guard above — it
// would leak on the target appliance forever.
func TestCreateRemoteVolumeStampsOwnership(t *testing.T) {
	src, dst, m := pair(t)
	g := testGroup("pvc-1")
	src.putOwned("Pool0/k8s/pvc-1")
	src.dataset("Pool0/k8s/pvc-1").refquota = 1 << 30

	remote, err := m.CreateRemoteVolume(context.Background(), g, testVolume("pvc-1"))
	if err != nil {
		t.Fatalf("CreateRemoteVolume: %v", err)
	}
	if remote != "Pool1/k8s/pvc-1" {
		t.Fatalf("remote dataset = %q, want Pool1/k8s/pvc-1", remote)
	}

	dst.mu.Lock()
	creates := append([]map[string]any(nil), dst.datasetCreates...)
	dst.mu.Unlock()
	if len(creates) != 1 {
		t.Fatalf("want exactly one pool.dataset.create on the target, got %d", len(creates))
	}
	props, _ := creates[0]["user_properties"].([]any)
	found := false
	for _, raw := range props {
		p, _ := raw.(map[string]any)
		if p["key"] == volume.OwnerProperty && p["value"] == volume.OwnerValue {
			found = true
		}
	}
	if !found {
		t.Fatalf("remote dataset created without %s — it could never be deleted: %v",
			volume.OwnerProperty, creates[0])
	}
	if ds := dst.dataset("Pool1/k8s/pvc-1"); ds == nil || ds.source != "LOCAL" {
		t.Fatalf("remote dataset is not owned with source LOCAL: %+v", ds)
	}

	// Calling again must not create a second dataset.
	if _, err := m.CreateRemoteVolume(context.Background(), g, testVolume("pvc-1")); err != nil {
		t.Fatalf("second CreateRemoteVolume: %v", err)
	}
	dst.mu.Lock()
	defer dst.mu.Unlock()
	if len(dst.datasetCreates) != 1 {
		t.Fatalf("CreateRemoteVolume is not idempotent: %d creates", len(dst.datasetCreates))
	}
}
