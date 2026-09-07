# Task 7: Ownership marker and delete guard

Status: closed
Created: 2026-09-07

## Description

Implements plan task 7 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 7: Ownership marker and delete guard" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/volume/ownership.go` — stamp and verify.
- `internal/volume/ownership_test.go` — tests.

Interfaces it produces for later tasks:

- `const OwnerProperty = "io.truenas.csi:managed"`
- `const OwnerValue = "truenas-csi"`
- `func StampProperties() []map[string]string` for use in dataset creation
- `func Stamp(ctx context.Context, c *truenas.Client, datasetID string) error` for clones
- `func VerifyOwned(ds *truenas.Dataset) error`
- `var ErrNotManaged = errors.New("dataset is not managed by this driver — refusing to delete")`

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestDeleteRefusesUnmarkedDataset.

## Acceptance criteria

- [x] Write `TestDeleteRefusesUnmarkedDataset`, table-driven over a dataset with no properties, one whose `io.truenas.csi:managed` has `Source: "INHERITED"`, and one whose value is `"something-else"` — each expected to return `ErrNotManaged` — plus one with `Value: "truenas-csi", Source: "LOCAL"` expected to return nil. Run `go test ./internal/volume/` — expect FAIL with "undefined: VerifyOwned".
- [x] Implement `VerifyOwned` requiring the property to be present, its value to equal `OwnerValue`, and its `Source` to be exactly `LOCAL`.
- [x] Implement `Stamp` issuing `pool.dataset.update <id> {"user_properties_update":[{"key":OwnerProperty,"value":OwnerValue}]}`.
- [x] Run `go test ./internal/volume/` — expect PASS.
- [x] Commit: "volume: ownership marker with LOCAL-source delete guard".

## Evidence

- Delivered by a parallel worktree; merged. VerifyOwned requires source == LOCAL.
- `go test ./...` green across all 18 packages; `gofmt -l` and `go vet ./...` clean.
- procoder gate: 0 blocking findings. Committed on branch feat/foundation.
