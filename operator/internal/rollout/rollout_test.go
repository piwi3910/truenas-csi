package rollout_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/piwi3910/truenas-csi/operator/internal/rollout"
)

// TestNodeRolloutIsDrainAware is the test that catches the worst failure this
// operator can cause: replacing a node's CSI plugin while that node is halfway
// through staging or unstaging a volume.
//
// It catches three distinct breaks:
//
//  1. rolling a node that is busy — the plugin disappears mid-stage, the CSI
//     call fails, and on a terminating pod the iSCSI session and mount are
//     stranded with nothing left that knows how to clean them up;
//  2. rolling more than one node at a time — "one at a time" is what bounds the
//     blast radius of a bad image to a single node's workloads;
//  3. starting the next node before the previous node's plugin is ready —
//     which is the same thing as rolling two nodes at once, just spread out.
func TestNodeRolloutIsDrainAware(t *testing.T) {
	t.Run("a busy node is skipped in favour of a clear one", func(t *testing.T) {
		plan := rollout.Next([]rollout.NodeState{
			{Name: "node-a", PodName: "plugin-a", UpToDate: false, Ready: true, Busy: true,
				BusyReason: "pod default/db is terminating with volume default/data still staged"},
			{Name: "node-b", PodName: "plugin-b", UpToDate: false, Ready: true, Busy: false},
		})
		if len(plan.Roll) != 1 {
			t.Fatalf("plan rolls %d pods, want exactly 1: nodes must be rolled one at a time", len(plan.Roll))
		}
		if plan.Roll[0] != "plugin-b" {
			t.Fatalf("plan rolls %q; node-a has a volume mid-stage and must not be touched", plan.Roll[0])
		}
	})

	t.Run("every out-of-date node busy means nothing is rolled", func(t *testing.T) {
		plan := rollout.Next([]rollout.NodeState{
			{Name: "node-a", PodName: "plugin-a", Ready: true, Busy: true, BusyReason: "pod default/db is still acquiring volume default/data"},
			{Name: "node-b", PodName: "plugin-b", Ready: true, Busy: true, BusyReason: "pod default/web is terminating with volume default/web-data still staged"},
		})
		if len(plan.Roll) != 0 {
			t.Fatalf("plan rolls %v while every node has a volume mid-stage", plan.Roll)
		}
		if plan.WaitingFor == "" {
			t.Error("a stalled rollout must say why: an administrator watching it sit still needs to know it is waiting on a workload, not stuck")
		}
		if plan.Done {
			t.Error("plan reports Done while nodes are still out of date")
		}
	})

	t.Run("the next node waits for the previous plugin to become ready", func(t *testing.T) {
		plan := rollout.Next([]rollout.NodeState{
			{Name: "node-a", PodName: "plugin-a2", UpToDate: true, Ready: false},
			{Name: "node-b", PodName: "plugin-b", UpToDate: false, Ready: true},
		})
		if len(plan.Roll) != 0 {
			t.Fatalf("plan rolls %v while node-a's new plugin is not ready yet: that is two nodes without a plugin", plan.Roll)
		}
	})

	t.Run("a finished rollout reports done", func(t *testing.T) {
		plan := rollout.Next([]rollout.NodeState{
			{Name: "node-a", PodName: "plugin-a", UpToDate: true, Ready: true},
			{Name: "node-b", PodName: "plugin-b", UpToDate: true, Ready: true},
		})
		if len(plan.Roll) != 0 || !plan.Done {
			t.Fatalf("plan = %+v, want done with nothing to roll", plan)
		}
		if plan.Updated != 2 || plan.Total != 2 {
			t.Errorf("progress = %d/%d, want 2/2", plan.Updated, plan.Total)
		}
	})

	t.Run("selection is deterministic across restarts", func(t *testing.T) {
		nodes := []rollout.NodeState{
			{Name: "node-c", PodName: "plugin-c", Ready: true},
			{Name: "node-a", PodName: "plugin-a", Ready: true},
			{Name: "node-b", PodName: "plugin-b", Ready: true},
		}
		first := rollout.Next(nodes)
		reversed := []rollout.NodeState{nodes[2], nodes[0], nodes[1]}
		second := rollout.Next(reversed)
		if len(first.Roll) != 1 || len(second.Roll) != 1 || first.Roll[0] != second.Roll[0] {
			t.Fatalf("rollout picked %v then %v: an operator restart must resume, not pick a new victim", first.Roll, second.Roll)
		}
	})
}

// TestBusyNodesRecognisesMidStageVolumes checks the input the rollout depends
// on: which node has a volume in flight right now.
func TestBusyNodesRecognisesMidStageVolumes(t *testing.T) {
	ours := map[string]bool{
		rollout.DriverPVCKey("default", "data"): true,
	}
	now := metav1.Now()

	pods := []corev1.Pod{
		// Terminating with one of our volumes: an unstage is imminent.
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "db", DeletionTimestamp: &now, Finalizers: []string{"x"}},
			Spec: corev1.PodSpec{NodeName: "node-a", Volumes: []corev1.Volume{{
				Name:         "d",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}},
			}}},
		},
		// Pending with one of our volumes: a stage is in flight.
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"},
			Spec: corev1.PodSpec{NodeName: "node-b", Volumes: []corev1.Volume{{
				Name:         "d",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}},
			}}},
			Status: corev1.PodStatus{Phase: corev1.PodPending},
		},
		// Running with one of our volumes: staged and settled, not busy. If this
		// counted as busy the rollout would never finish on a cluster that is
		// actually using its storage.
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "cache"},
			Spec: corev1.PodSpec{NodeName: "node-c", Volumes: []corev1.Volume{{
				Name:         "d",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}},
			}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		// Pending, but with somebody else's volume.
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "other"},
			Spec: corev1.PodSpec{NodeName: "node-d", Volumes: []corev1.Volume{{
				Name:         "d",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "longhorn-data"}},
			}}},
			Status: corev1.PodStatus{Phase: corev1.PodPending},
		},
	}

	busy := rollout.BusyNodes(pods, ours)
	for _, node := range []string{"node-a", "node-b"} {
		if _, ok := busy[node]; !ok {
			t.Errorf("%s has one of our volumes in flight but is not reported busy", node)
		}
	}
	for _, node := range []string{"node-c", "node-d"} {
		if reason, ok := busy[node]; ok {
			t.Errorf("%s is reported busy (%q) but has nothing of ours in flight", node, reason)
		}
	}
}
