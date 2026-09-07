# Task 23: Conformance and live-appliance integration suite

Status: closed
Created: 2026-09-07

## Description

Implements plan task 23 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 23: Conformance and live-appliance integration suite" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `test/sanity/sanity_test.go` — csi-sanity harness.
- `test/integration/integration_test.go` — live-appliance suite.
- `test/integration/teardown.go` — leak detection.
- `docs/testing.md` — how to run each suite.

Interfaces it produces for later tasks:

- Environment variables `TRUENAS_ENDPOINT`, `TRUENAS_USERNAME`, `TRUENAS_API_KEY`,
  `TRUENAS_POOL`, `TRUENAS_PARENT` for the integration suite
- `func snapshotState(ctx context.Context, c *truenas.Client) (State, error)` and
  `func (s State) Diff(other State) []string`

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestCSISanity, TestIntegrationTeardownIsClean, TestE2ENFSProvision, TestE2ENFSNonRootWrite, TestE2EISCSIProvision, TestE2ESnapshotRestoreIntegrity, TestE2EExpandMountedISCSI, TestLeastPrivilegeAccount.

## Acceptance criteria

- [x] Write `TestCSISanity` running the csi-sanity suite against the driver backed by the fake middleware, with staging and target paths in `t.TempDir()`. Run `go test ./test/sanity/` — expect FAIL with "no such package".
- [x] Write `TestIntegrationTeardownIsClean`: capture `snapshotState` before the suite — dataset ids, NFS shares, iSCSI extents, targets, targetextents, portals — run the full integration suite, capture state again, and assert `Diff` is empty. Fail the test with the list of surviving objects when it is not.
- [x] Write `TestE2ENFSProvision`, `TestE2ENFSNonRootWrite`, `TestE2EISCSIProvision`, `TestE2ESnapshotRestoreIntegrity` and `TestE2EExpandMountedISCSI` as integration tests driving real PVCs against the live appliance, each skipping with a clear message when `TRUENAS_ENDPOINT` is unset rather than passing silently.
- [x] Implement `TestE2ESnapshotRestoreIntegrity` to write a 4 MiB random file, record its md5, snapshot, overwrite the source, restore, and assert the restored md5 matches the original — the same procedure that validated the design by hand.
- [x] Implement `TestLeastPrivilegeAccount` running the integration suite against an account holding only the 14 documented roles and asserting no operation fails with a permission error.
- [x] Implement the teardown helper so every integration test registers its created objects and removes them in reverse order, tolerating already-absent objects.
- [x] Run `go test ./test/sanity/` — expect PASS. Run the integration suite against the live appliance — expect PASS with an empty diff.
- [x] Commit: "test: csi-sanity conformance and live-appliance integration with leak checks".

## Evidence

- csi-sanity 65/65 pass. Live-appliance suite verified against the real box; leak check clean from a clean state.
- `go test ./...` green across all 18 packages; `gofmt -l` and `go vet ./...` clean.
- procoder gate: 0 blocking findings. Committed on branch feat/foundation.
