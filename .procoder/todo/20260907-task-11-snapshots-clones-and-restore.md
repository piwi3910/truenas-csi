# Task 11: Snapshots, clones and restore

Status: closed
Created: 2026-09-07

## Description

Implements plan task 11 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 11: Snapshots, clones and restore" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/backend/snapshot.go` — protocol-independent snapshot operations.
- `internal/backend/snapshot_test.go` — tests.

Interfaces it produces for later tasks:

- `func (r *Registry) CreateSnapshot(ctx context.Context, sourceID volume.ID, name string) (*Snapshot, error)`
- `func (r *Registry) DeleteSnapshot(ctx context.Context, snapshotID string) error`
- `func (r *Registry) ListSnapshots(ctx context.Context, sourceID *volume.ID) ([]Snapshot, error)`
- `type Snapshot struct { ID, SourceVolumeID string; SizeBytes int64; CreationTime time.Time; ReadyToUse bool }`
- `func restoreFromSnapshot(ctx context.Context, c *truenas.Client, snapshotID string, target volume.ID, bytes int64, params map[string]string) error`

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestRestoredCloneIsManaged, TestRestoredNFSCloneGetsPermissions, TestDeleteSnapshotWithDependentClone, TestSnapshotNeverPromotes.

## Acceptance criteria

- [x] Write `TestRestoredCloneIsManaged`: assert that after `restoreFromSnapshot` the clone receives an explicit `user_properties_update` stamping the ownership marker and an explicit quota or volsize — a ZFS clone inherits neither from its origin, so without both the volume leaks permanently and misreports its size. Run `go test ./internal/backend/` — expect FAIL with "undefined: restoreFromSnapshot".
- [x] Write `TestRestoredNFSCloneGetsPermissions` asserting `filesystem.setperm` runs on a restored filesystem volume, because the clone carries the snapshot's permissions rather than the new StorageClass's.
- [x] Write `TestDeleteSnapshotWithDependentClone`: fake reports the snapshot has a clone; assert `DeleteSnapshot` returns `codes.FailedPrecondition` and issues no delete.
- [x] Write `TestSnapshotNeverPromotes`: assert no `pool.dataset.promote` is issued anywhere in the restore path — promoting inverts the dependency and makes the SOURCE volume undeletable.
- [x] Implement `CreateSnapshot` calling `pool.snapshot.create` with the source dataset and a name derived from the CSI snapshot name, returning `ReadyToUse: true` immediately since ZFS snapshots are atomic.
- [x] Implement `DeleteSnapshot`: query dependent clones first and return `FailedPrecondition` when any exist; return nil when the snapshot is already absent.
- [x] Implement `restoreFromSnapshot`: `pool.snapshot.clone` to the target dataset, then stamp ownership, then set refquota or volsize, then for filesystem volumes run `SetPerm`, then create the share or iSCSI objects as the protocol requires.
- [x] Run `go test ./internal/backend/` — expect PASS.
- [x] Commit: "backend: snapshots, dependency-checked deletion and stamped restore".

## Evidence

- Snapshots never promote; DeleteSnapshot returns FailedPrecondition while clones exist.
- `go test ./...` green across all 18 packages; `gofmt -l` and `go vet ./...` clean.
- procoder gate: 0 blocking findings. Committed on branch feat/foundation.
