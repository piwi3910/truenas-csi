// Package replication implements StorageProtectionGroup — a named set of
// driver-owned volumes replicated from one TrueNAS appliance to another — on
// top of TrueNAS's own dataset replication (replication.* plus periodic
// snapshot tasks).
//
// The operations here move production data between appliances, so every one of
// them is written to be refusable and repeatable rather than convenient:
//
//   - a group may only contain datasets this driver created (volume.VerifyOwned),
//     because a replication task overwrites its target;
//   - failover is explicit and idempotent — a second failover never promotes twice;
//   - a test failover clones the last replicated snapshot into a scratch dataset
//     and physically cannot reach the production target or its replication task;
//   - failback refuses a diverged source unless forced, and says what diverged;
//   - suspend and resume are level-triggered, so repeating them is free;
//   - nothing here deletes a dataset it did not create.
package replication

import (
	"context"
	"sync"
)

// Phase is the group's position in the failover lifecycle. It is the record
// that makes failover idempotent: without it a repeated Failover call would
// promote the target a second time, and two promotions is how a split brain
// starts.
type Phase string

const (
	// PhaseUnknown is a group that has never been reconciled.
	PhaseUnknown Phase = ""
	// PhaseReady means the source is primary and replication is running.
	PhaseReady Phase = "Ready"
	// PhaseSuspended means the source is still primary but transfers are off.
	PhaseSuspended Phase = "Suspended"
	// PhaseFailingOver means a failover started and has not finished. A second
	// failover is REFUSED in this phase unless forced: the first one may still
	// be running, and running two concurrently is what produces two primaries.
	PhaseFailingOver Phase = "FailingOver"
	// PhaseFailedOver means the target has been promoted and is now primary.
	PhaseFailedOver Phase = "FailedOver"
	// PhaseFailingBack means a failback started and has not finished.
	PhaseFailingBack Phase = "FailingBack"
)

// State is everything the driver must remember about a group between calls.
//
// It is deliberately serialisable and free of live handles: the controller
// persists it in the StorageProtectionGroup's status and the tests keep it in
// memory, but the safety decisions read exactly the same fields either way.
type State struct {
	Phase Phase `json:"phase,omitempty"`

	// TaskID is the TrueNAS replication task id on the source appliance.
	TaskID int `json:"taskID,omitempty"`
	// SnapshotTaskIDs maps a volume name to its periodic snapshot task id on
	// the source appliance.
	SnapshotTaskIDs map[string]int `json:"snapshotTaskIDs,omitempty"`

	// Suspended mirrors the replication task's enabled flag.
	Suspended bool `json:"suspended,omitempty"`

	// FailoverSnapshots maps a volume name to the last snapshot that existed on
	// the TARGET when failover promoted it. Failback compares the source
	// against this mark to decide whether the old source has diverged.
	FailoverSnapshots map[string]string `json:"failoverSnapshots,omitempty"`

	// TestFailover, when non-nil, describes the scratch clones a test failover
	// created on the target appliance. It never refers to production datasets.
	TestFailover *TestFailoverState `json:"testFailover,omitempty"`

	// Message explains the current phase to an operator.
	Message string `json:"message,omitempty"`
}

// TestFailoverState records the scratch datasets a test failover exposed.
type TestFailoverState struct {
	// Root is the scratch parent dataset on the target appliance.
	Root string `json:"root,omitempty"`
	// Datasets maps a volume name to the scratch clone serving it.
	Datasets map[string]string `json:"datasets,omitempty"`
	// Snapshots maps a volume name to the replicated snapshot it was cloned from.
	Snapshots map[string]string `json:"snapshots,omitempty"`
}

// DeepCopy returns an independent copy of s.
func (s *State) DeepCopy() *State {
	if s == nil {
		return nil
	}
	out := *s
	if s.SnapshotTaskIDs != nil {
		out.SnapshotTaskIDs = make(map[string]int, len(s.SnapshotTaskIDs))
		for k, v := range s.SnapshotTaskIDs {
			out.SnapshotTaskIDs[k] = v
		}
	}
	out.FailoverSnapshots = copyStringMap(s.FailoverSnapshots)
	if s.TestFailover != nil {
		tf := *s.TestFailover
		tf.Datasets = copyStringMap(s.TestFailover.Datasets)
		tf.Snapshots = copyStringMap(s.TestFailover.Snapshots)
		out.TestFailover = &tf
	}
	return &out
}

func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Store persists group state. The controller backs it with the custom
// resource's status; tests use MemoryStore.
type Store interface {
	Load(ctx context.Context, group string) (*State, error)
	Save(ctx context.Context, group string, s *State) error
}

// MemoryStore is an in-process Store.
type MemoryStore struct {
	mu     sync.Mutex
	states map[string]*State
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore { return &MemoryStore{states: map[string]*State{}} }

// Load returns the stored state, or a zero state when the group is new.
func (m *MemoryStore) Load(_ context.Context, group string) (*State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.states[group]; ok {
		return s.DeepCopy(), nil
	}
	return &State{}, nil
}

// Save records the state for a group.
func (m *MemoryStore) Save(_ context.Context, group string, s *State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[group] = s.DeepCopy()
	return nil
}
