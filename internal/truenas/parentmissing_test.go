package truenas

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestDatasetCreateNamesTheMissingParent pins the translation of the one
// failure a mis-typed parentDataset produces.
//
// The reason string is copied verbatim from a live 25.10.6 appliance. The
// middleware calls it EINVAL and puts the real cause only in the text, so a
// driver that forwards the error hands the operator a raw middleware string
// under a bare Internal code — for a mistake made in a values file one deploy
// earlier, on every PersistentVolumeClaim at once.
func TestDatasetCreateNamesTheMissingParent(t *testing.T) {
	err := &CallError{
		Method: "pool.dataset.create", Code: -32602, ErrName: "EINVAL",
		Reason: "[EINVAL] pool_dataset_create.name: Parent dataset (Pool0/k8s) does not exist.",
	}
	if !IsParentMissing(err) {
		t.Fatal("IsParentMissing did not recognise the appliance's own wording")
	}
	if IsParentMissing(errors.New("some other failure")) {
		t.Error("IsParentMissing matched an unrelated error")
	}
	// A dataset that is merely absent is NOT a missing parent: that one is
	// idempotent success on delete, and must never become FailedPrecondition.
	notFound := &CallError{Method: "pool.dataset.query", ErrName: "EINVAL",
		Reason: "[ENOENT] does not exist"}
	if IsParentMissing(notFound) {
		t.Error("IsParentMissing matched a plain ENOENT")
	}
}

// TestDatasetCreateParentMissingIsFailedPrecondition checks the code and the
// message the operator actually sees in `kubectl describe pvc`.
func TestDatasetCreateParentMissingIsFailedPrecondition(t *testing.T) {
	ops := &Ops{Transport: parentMissingTransport{}}
	_, err := ops.DatasetCreate(context.Background(), DatasetSpec{
		Name: "Pool0/k8s/pvc-1", Type: "FILESYSTEM",
	})
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("code = %s, want FailedPrecondition (got %v)", got, err)
	}
	if !strings.Contains(err.Error(), "Pool0/k8s") {
		t.Errorf("the message must name the dataset to create, got: %v", err)
	}
}

// parentMissingTransport fails every call the way the appliance fails a create
// whose parent is absent.
type parentMissingTransport struct{}

func (parentMissingTransport) CallJSON(context.Context, any, string, ...any) error {
	return parentMissingErr()
}

func (parentMissingTransport) Call(context.Context, string, ...any) (json.RawMessage, error) {
	return nil, parentMissingErr()
}

func (parentMissingTransport) Host() string { return "appliance.invalid" }
func (parentMissingTransport) Close() error { return nil }

func parentMissingErr() error {
	return &CallError{
		Method: "pool.dataset.create", Code: -32602, ErrName: "EINVAL",
		Reason: "[EINVAL] pool_dataset_create.name: Parent dataset (Pool0/k8s) does not exist.",
	}
}

// TestDatasetDeleteNamesTheDependentClones pins the translation of the refusal
// that ends the most ordinary snapshot workflow there is: restore a snapshot,
// check the copy, delete the original.
//
// The reason string is verbatim from a live 25.10.6 appliance. Forwarded raw it
// reaches Kubernetes as codes.Internal, which external-provisioner retries for
// ever — the claim sits in Terminating with no indication of what is holding
// it, and the message quotes ZFS suggesting `-R`, which would destroy the
// restored volume the operator had just made.
func TestDatasetDeleteNamesTheDependentClones(t *testing.T) {
	err := &CallError{
		Method: "pool.dataset.delete", Code: -32001, ErrName: "EFAULT",
		Reason: "[EFAULT] Failed to delete dataset: cannot destroy 'Pool0/k8s/pvc-1': " +
			"filesystem has dependent clones\nuse '-R' to destroy the following datasets:\n" +
			"Pool0/k8s/pvc-restored",
	}
	if !IsHasDependentClones(err) {
		t.Fatal("IsHasDependentClones did not recognise the appliance's own wording")
	}
	if IsHasDependentClones(errors.New("some other failure")) {
		t.Error("IsHasDependentClones matched an unrelated error")
	}

	ops := &Ops{Transport: dependentClonesTransport{}}
	derr := ops.DatasetDelete(context.Background(), "Pool0/k8s/pvc-1", true, false)
	if got := status.Code(derr); got != codes.FailedPrecondition {
		t.Fatalf("code = %s, want FailedPrecondition (got %v)", got, derr)
	}
	if !strings.Contains(derr.Error(), "Pool0/k8s/pvc-1") {
		t.Errorf("the message must name the volume being deleted, got: %v", derr)
	}
	if strings.Contains(derr.Error(), "-R") {
		t.Errorf("the message repeats ZFS's -R suggestion, which destroys the "+
			"dependent volumes: %v", derr)
	}
}

// dependentClonesTransport refuses the delete the way the appliance does, and
// answers the follow-up listing that finds what is holding the dataset.
type dependentClonesTransport struct{}

func (dependentClonesTransport) CallJSON(_ context.Context, out any, method string, _ ...any) error {
	if method == "pool.dataset.query" {
		return json.Unmarshal([]byte(`[{"id":"Pool0/k8s/pvc-restored",
		  "origin":{"parsed":"Pool0/k8s/pvc-1@snap1","value":"Pool0/k8s/pvc-1@snap1","source":"NONE"}}]`), out)
	}
	return &CallError{
		Method: "pool.dataset.delete", Code: -32001, ErrName: "EFAULT",
		Reason: "[EFAULT] Failed to delete dataset: cannot destroy 'Pool0/k8s/pvc-1': " +
			"filesystem has dependent clones",
	}
}

func (dependentClonesTransport) Call(context.Context, string, ...any) (json.RawMessage, error) {
	return nil, errors.New("not used")
}
func (dependentClonesTransport) Host() string { return "appliance.invalid" }
func (dependentClonesTransport) Close() error { return nil }
