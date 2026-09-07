# Task 10: iSCSI provisioning backend

Status: open
Created: 2026-09-07

## Description

Implements plan task 10 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 10: iSCSI provisioning backend" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/backend/iscsi/iscsi.go` — create, delete, expand, publish context.
- `internal/backend/iscsi/target.go` — shared target, portal, CHAP, initiator ACL.
- `internal/backend/iscsi/lun.go` — LUN id allocation.
- `internal/backend/iscsi/iscsi_test.go`, `internal/backend/iscsi/lun_test.go` — tests.

Interfaces it produces for later tasks:

- `func New(c *truenas.Client, pool, parent string) backend.Backend`
- `func ensureTarget(ctx context.Context, c *truenas.Client, p Params) (targetID int, iqn string, err error)`
- `func ensurePortal(ctx context.Context, c *truenas.Client, p Params) (int, error)`
- `func allocateLUN(ctx context.Context, c *truenas.Client, targetID int) (int, error)`
- Publish context keys: `portal`, `iqn`, `lun`, `naa`, `chapUser`, `chapSecretRef`
- StorageClass parameters consumed: `portalID`, `chap` (default `"true"`),
  `initiatorACL` (default `"true"`), `sparse` (default `"true"`), `volblocksize`,
  `fsType` (default `ext4`), `multipath` (default `"false"`)

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestLUNAllocationUnderConcurrency, TestEnsureTargetIsIdempotent, TestEnsurePortalRespectsOverride, TestCHAPGeneratedPerTarget, TestInitiatorACLRestrictsToNodes, TestISCSICreateIsIdempotent, TestISCSIDeleteVerifiesOwnership, TestISCSIExpandRejectsShrink.

## Acceptance criteria

- [ ] Write `TestLUNAllocationUnderConcurrency`: fake tracks created targetextents; run 20 concurrent `allocateLUN` calls against one target and assert every returned id is distinct and contiguous from 0, then simulate a controller restart by discarding all in-memory state and assert the next allocation continues from the live query rather than restarting at 0. Run `go test ./internal/backend/iscsi/` — expect FAIL with "undefined: allocateLUN".
- [ ] Write `TestEnsureTargetIsIdempotent`: two concurrent `ensureTarget` calls produce exactly one `iscsi.target.create` at the fake.
- [ ] Write `TestEnsurePortalRespectsOverride`: with `portalID` set, assert no `iscsi.portal.create` is issued and the given id is used.
- [ ] Write `TestCHAPGeneratedPerTarget`: assert an `iscsi.auth` entry is created with a generated secret of at least 12 characters, that the secret never appears in any log line captured during the test, and that `PublishContext` references it without inlining it.
- [ ] Write `TestInitiatorACLRestrictsToNodes`: assert the target's initiator group contains the supplied node IQNs, and that setting `initiatorACL: "false"` creates no group.
- [ ] Write `TestISCSICreateIsIdempotent` asserting a repeated `Create` yields one zvol and one extent, and `TestISCSIDeleteVerifiesOwnership` asserting an unmarked zvol is refused.
- [ ] Write `TestISCSIExpandRejectsShrink` asserting a smaller size is rejected by the driver before reaching middleware.
- [ ] Implement `allocateLUN`: under the backend lock, query `iscsi.targetextent.query` filtered by target, collect used ids, and return the lowest free id — derived from the live query every time, never cached.
- [ ] Implement `ensurePortal` and `ensureTarget`: query first, create only when absent, and treat a create that fails because the object already exists as success followed by a re-query.
- [ ] Implement `Create`: query for an existing zvol; if absent create it with `type: VOLUME`, the requested `volsize`, `sparse`, `volblocksize` taken from the parameter or `pool.dataset.recommended_zvol_blocksize`, and the ownership property. Then create the extent with `type: DISK`, `disk: "zvol/<dataset path>"`, capture the returned `naa`, ensure the shared target, allocate a LUN, and create the targetextent. Roll back created objects in reverse on any failure.
- [ ] Implement `Delete`: verify ownership on the zvol, then delete targetextent, extent and zvol in that order, tolerating each already being absent.
- [ ] Implement `Expand` updating `volsize`, rejecting shrink.
- [ ] Run `go test ./internal/backend/iscsi/` — expect PASS.
- [ ] Commit: "backend/iscsi: zvol, extent, shared target, LUN allocation and CHAP".

## Evidence

