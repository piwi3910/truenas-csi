# Task 13: Capacity reporting and volume listing

Status: closed
Created: 2026-09-07

## Description

Implements plan task 13 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 13: Capacity reporting and volume listing" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/csi/capacity.go` — GetCapacity and ListVolumes.
- `internal/csi/capacity_test.go` — tests.

Interfaces it produces for later tasks:

- `func (c *controller) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error)`
- `func (c *controller) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error)`

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestCapacityMatchesPool, TestListVolumesPaginates.

## Acceptance criteria

- [x] Write `TestCapacityMatchesPool`: fake reports a pool with `free` of 44861949222912; assert `GetCapacity` returns exactly that for a StorageClass naming that backend, and that a request naming an unknown backend returns `ErrUnknownBackend`. Run `go test ./internal/csi/ -run TestCapacityMatchesPool` — expect FAIL with "undefined: GetCapacity".
- [x] Write `TestListVolumesPaginates` asserting a 250-dataset fake returns pages honouring `max_entries` and a resumable `next_token`, and that only datasets carrying a `LOCAL` ownership marker are listed.
- [x] Implement `GetCapacity` querying `pool.query` for the named backend and returning `available_capacity` from the pool's free bytes.
- [x] Implement `ListVolumes` querying datasets under the configured parent, filtering to owned ones, and paginating by dataset id.
- [x] Run `go test ./internal/csi/` — expect PASS.
- [x] Commit: "csi: capacity reporting and owned-volume listing".

## Evidence

- GetCapacity from real pool free space; ListVolumes lists only LOCAL-owned datasets and paginates.
- `go test ./...` green across all 18 packages; `gofmt -l` and `go vet ./...` clean.
- procoder gate: 0 blocking findings. Committed on branch feat/foundation.
