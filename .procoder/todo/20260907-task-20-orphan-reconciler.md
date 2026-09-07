# Task 20: Orphan reconciler

Status: closed
Created: 2026-09-07

## Description

Implements plan task 20 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 20: Orphan reconciler" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/reconcile/orphans.go` — the reporting loop.
- `internal/reconcile/orphans_test.go` — tests.

Interfaces it produces for later tasks:

- `func NewOrphanReconciler(r *backend.Registry, lister PVLister, interval time.Duration) *OrphanReconciler`
- `type PVLister interface { VolumeHandles(ctx context.Context) (map[string]struct{}, error) }`
- `func (o *OrphanReconciler) RunOnce(ctx context.Context) (orphans []string, err error)`

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestOrphanReconcilerReports, TestOrphanReconcilerIgnoresUnowned, TestOrphanReconcilerSkipsOnListerError.

## Acceptance criteria

- [x] Write `TestOrphanReconcilerReports`: fake holds three owned datasets while the lister returns two matching volume handles; assert `RunOnce` returns exactly the third, increments `truenas_csi_orphaned_volumes`, logs its id, and — the important assertion — that no `pool.dataset.delete` reached the fake. Run `go test ./internal/reconcile/` — expect FAIL with "undefined: NewOrphanReconciler".
- [x] Write `TestOrphanReconcilerIgnoresUnowned` asserting a dataset without a `LOCAL` ownership marker is never reported, since it was never ours to begin with.
- [x] Write `TestOrphanReconcilerSkipsOnListerError` asserting that when the PV lister fails, nothing is reported — a partial view must never be read as evidence of orphans.
- [x] Implement `RunOnce` listing owned datasets per backend, subtracting known PV handles, and reporting the remainder through the metric and a warning log.
- [x] Implement the loop calling `RunOnce` on the interval, defaulting to 30 minutes.
- [x] Run `go test ./internal/reconcile/` — expect PASS.
- [x] Commit: "reconcile: report-only orphan detection".

## Evidence

- Report-only orphan detection; mutation: making it delete failed TestOrphanReconcilerReports.
- `go test ./...` green across all 18 packages; `gofmt -l` and `go vet ./...` clean.
- procoder gate: 0 blocking findings. Committed on branch feat/foundation.
