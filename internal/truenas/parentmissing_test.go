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
