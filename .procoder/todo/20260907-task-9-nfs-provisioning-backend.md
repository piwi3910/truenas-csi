# Task 9: NFS provisioning backend

Status: closed
Created: 2026-09-07

## Description

Implements plan task 9 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 9: NFS provisioning backend" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/backend/nfs/nfs.go` — create, delete, expand, publish context.
- `internal/backend/nfs/nfs_test.go` — tests.

Interfaces it produces for later tasks:

- `func New(c *truenas.Client, pool, parent string) backend.Backend`
- Publish context keys: `server`, `share`, `nfsVersion`
- StorageClass parameters consumed: `nfsVersion` (default `4`), `networks`, `maproot`,
  `mode` (default `0777`), `uid` (default `0`), `gid` (default `0`)

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestNFSCreateSetsRefquota, TestNFSCreateStampsOwnership, TestNFSCreateSetsPermissions, TestNFSCreateIsIdempotent, TestNFSCreateConflictingSize, TestNFSDeleteVerifiesOwnership, TestNFSExpandRejectsShrink.

## Acceptance criteria

- [x] Write `TestNFSCreateSetsRefquota`: assert the create path issues `pool.dataset.create` with `refquota` equal to the requested bytes, and fail the test if `refquota` is absent — without it a pod sees the whole pool rather than its volume. Run `go test ./internal/backend/nfs/` — expect FAIL with "undefined: New".
- [x] Write `TestNFSCreateStampsOwnership` asserting `user_properties` carries `io.truenas.csi:managed` at creation.
- [x] Write `TestNFSCreateSetsPermissions` asserting `filesystem.setperm` is called with the configured mode, uid and gid before the share is created, because a fresh dataset is `root:root 0755` and a non-root pod cannot write to it.
- [x] Write `TestNFSCreateIsIdempotent`: run `Create` twice with identical parameters and assert exactly one `pool.dataset.create` reaches the fake and both calls return the same volume — the second must find the existing dataset by query.
- [x] Write `TestNFSCreateConflictingSize`: existing dataset with a different refquota; assert a `codes.AlreadyExists` error.
- [x] Write `TestNFSDeleteVerifiesOwnership`: fake returns a dataset with no `LOCAL` marker; assert `Delete` returns `volume.ErrNotManaged` and issues no `pool.dataset.delete`.
- [x] Write `TestNFSExpandRejectsShrink` asserting a smaller size returns `codes.InvalidArgument` and issues no update, because middleware silently permits a refquota shrink below current usage.
- [x] Implement `Create`: query for an existing dataset first; if absent create it with `refquota`, the ownership property and `share_type` unset; then `SetPerm`; then create the NFS share with the configured networks and maproot. On any failure after dataset creation, delete the dataset before returning so no unmarked partial remains.
- [x] Implement `Delete`: query the dataset, `VerifyOwned`, delete the NFS share whose path matches the mountpoint, then delete the dataset. Return nil when the dataset is already absent.
- [x] Implement `Expand` rejecting any size below the current refquota, then updating it.
- [x] Implement `PublishContext` returning the server address, export path and nfs version.
- [x] Run `go test ./internal/backend/nfs/` — expect PASS.
- [x] Commit: "backend/nfs: dataset, quota, permissions and share provisioning".

## Evidence

- Delivered by a parallel worktree; merged, with a correction: PublishContext now falls back to the appliance host so a restarted controller can publish existing volumes.
- `go test ./...` green across all 18 packages; `gofmt -l` and `go vet ./...` clean.
- procoder gate: 0 blocking findings. Committed on branch feat/foundation.
