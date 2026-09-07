package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/piwi3910/truenas-csi/internal/replication"
	v1alpha1 "github.com/piwi3910/truenas-csi/internal/replication/v1alpha1"
)

func group(handles ...string) *v1alpha1.StorageProtectionGroup {
	return &v1alpha1.StorageProtectionGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "prod"},
		Spec: v1alpha1.StorageProtectionGroupSpec{
			SourceBackend: "nas1", TargetBackend: "nas2",
			VolumeHandles: handles, SSHCredentialID: 3,
		},
	}
}

// TestGroupFromSpecRefusesSelfReplication catches a group whose source and
// target are the same appliance: the replication task would then push a dataset
// over itself, which destroys the very data the group exists to protect.
func TestGroupFromSpecRefusesSelfReplication(t *testing.T) {
	spg := group("nas1/nfs/Pool0/k8s/pvc-1")
	spg.Spec.TargetBackend = "nas1"
	if _, err := GroupFromSpec(spg); err == nil ||
		!strings.Contains(err.Error(), "overwrite the source") {
		t.Fatalf("GroupFromSpec error = %v, want a refusal naming the hazard", err)
	}
}

// TestGroupFromSpecRejectsMalformedHandles keeps an unparseable handle from
// ever reaching a middleware argument.
func TestGroupFromSpecRejectsMalformedHandles(t *testing.T) {
	if _, err := GroupFromSpec(group("not-a-handle")); err == nil {
		t.Fatal("GroupFromSpec accepted a malformed volume handle")
	}
}

// TestStatusStoreRoundTripsFailoverRecord is the durability half of failover
// idempotency: the phase and the per-volume failover snapshot must survive a
// controller restart, because they are what a second failover consults before
// deciding it has nothing to do.
func TestStatusStoreRoundTripsFailoverRecord(t *testing.T) {
	spg := group("nas1/nfs/Pool0/k8s/pvc-1")
	store := &statusStore{group: spg}

	want := &replication.State{
		Phase:             replication.PhaseFailedOver,
		TaskID:            12,
		FailoverSnapshots: map[string]string{"pvc-1": "csi-g1-2026-09-08_10-00"},
	}
	if err := store.Save(context.Background(), "g1", want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if spg.Status.Phase != string(replication.PhaseFailedOver) {
		t.Fatalf("status.phase = %q, want FailedOver", spg.Status.Phase)
	}

	// A fresh store over the same object — as after a restart.
	got, err := (&statusStore{group: spg}).Load(context.Background(), "g1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Phase != replication.PhaseFailedOver || got.TaskID != 12 {
		t.Fatalf("loaded state = %+v, want the failover record back", got)
	}
	if got.FailoverSnapshots["pvc-1"] != "csi-g1-2026-09-08_10-00" {
		t.Fatalf("failover snapshot lost across a restart: %+v", got.FailoverSnapshots)
	}
}

// TestRefusedErrorsAreNotRetried checks the classification the reconciler uses:
// a safety refusal must be reported and left alone, never retried in a backoff
// loop as though the cluster could fix it.
func TestRefusedErrorsAreNotRetried(t *testing.T) {
	for _, err := range []error{
		replication.ErrDiverged, replication.ErrFailoverIncomplete,
		replication.ErrTestFailoverActive, replication.ErrWrongPhase,
		replication.ErrForbiddenCall,
	} {
		if !refused(err) {
			t.Fatalf("%v is not classified as a refusal and would be retried", err)
		}
	}
	if refused(context.DeadlineExceeded) {
		t.Fatal("a timeout was classified as a refusal and would never be retried")
	}
}
