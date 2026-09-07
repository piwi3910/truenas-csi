# Task 12: CSI Identity and Controller services

Status: closed
Created: 2026-09-07

## Description

Implements plan task 12 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 12: CSI Identity and Controller services" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/csi/identity.go` — Identity service.
- `internal/csi/controller.go` — Controller service.
- `internal/csi/locks.go` — per-volume serialisation.
- `internal/csi/controller_test.go`, `internal/csi/locks_test.go` — tests.

Interfaces it produces for later tasks:

- `func NewIdentity(name, version string) csi.IdentityServer`
- `func NewController(r *backend.Registry, cfg *config.Config) csi.ControllerServer`
- `type VolumeLocks struct{ ... }` with
  `func (l *VolumeLocks) TryAcquire(id string) (release func(), ok bool)`

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestConcurrentCreateIsIdempotent, TestConcurrentCreateDelete, TestVolumeLocksReturnsAborted, TestDeleteVolumeAbsentReturnsOK, TestCreateVolumeRejectsUnconfinedID, TestGracefulShutdownDrainsInFlight.

## Acceptance criteria

- [x] Write `TestConcurrentCreateIsIdempotent`: issue 50 concurrent `CreateVolume` calls with the same name and assert exactly one `pool.dataset.create` reaches the fake and all 50 responses carry the same volume id. Run `go test ./internal/csi/` — expect FAIL with "undefined: NewController".
- [x] Write `TestConcurrentCreateDelete`: interleave `CreateVolume` and `DeleteVolume` for one id 100 times and assert the fake ends with either zero or one dataset and no orphaned extent, target-extent or share.
- [x] Write `TestVolumeLocksReturnsAborted` asserting a second concurrent call for the same volume id receives `codes.Aborted` so the sidecar retries rather than racing.
- [x] Write `TestDeleteVolumeAbsentReturnsOK` asserting deleting an already-deleted volume returns success, as the CSI spec requires.
- [x] Write `TestCreateVolumeRejectsUnconfinedID` asserting a request whose resolved dataset escapes the parent returns `codes.InvalidArgument` before any middleware call.
- [x] Implement `VolumeLocks` as a mutex-guarded set of in-flight volume ids.
- [x] Implement `NewController` wiring `CreateVolume`, `DeleteVolume`, `ControllerExpandVolume`, `ValidateVolumeCapabilities`, `ControllerGetCapabilities`, `CreateSnapshot`, `DeleteSnapshot`, `ListSnapshots`, each acquiring the volume lock and returning `codes.Aborted` when it is held.
- [x] Implement `NewIdentity` advertising `DriverName`, `Version`, and the `CONTROLLER_SERVICE` and `VOLUME_ACCESSIBILITY_CONSTRAINTS` capabilities.
- [x] Write `TestGracefulShutdownDrainsInFlight`: start the gRPC server, begin a `CreateVolume` the fake holds open, signal shutdown, and assert the call completes with a real response rather than being cancelled, that no new call is accepted after the signal, and that the process exits within the 30-second drain bound.
- [x] Implement graceful shutdown: on SIGTERM stop accepting new RPCs, wait for in-flight calls bounded at 30 seconds, then close each backend client.
- [x] Run `go test ./internal/csi/` — expect PASS.
- [x] Commit: "csi: identity and controller services with per-volume locking".

## Evidence

- Per-volume locking; 50 concurrent identical creates yield one dataset. Mutations on the lock and on Confine both failed the suite.
- `go test ./...` green across all 18 packages; `gofmt -l` and `go vet ./...` clean.
- procoder gate: 0 blocking findings. Committed on branch feat/foundation.
