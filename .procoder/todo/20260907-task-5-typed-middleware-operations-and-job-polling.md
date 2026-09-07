# Task 5: Typed middleware operations and job polling

Status: closed
Created: 2026-09-07

## Description

Implements plan task 5 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 5: Typed middleware operations and job polling" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/truenas/dataset.go` — dataset and snapshot operations.
- `internal/truenas/iscsi.go` — iSCSI object operations.
- `internal/truenas/sharing.go` — NFS share operations.
- `internal/truenas/job.go` — job submission and polling.
- `internal/truenas/job_test.go`, `internal/truenas/dataset_test.go` — tests.

Interfaces it produces for later tasks:

- `func (c *Client) DatasetCreate(ctx context.Context, spec DatasetSpec) (*Dataset, error)`
- `func (c *Client) DatasetQuery(ctx context.Context, id string) (*Dataset, error)` returning
  `(nil, nil)` when absent
- `func (c *Client) DatasetUpdate(ctx context.Context, id string, patch map[string]any) (*Dataset, error)`
- `func (c *Client) DatasetDelete(ctx context.Context, id string, recursive, force bool) error`
- `func (c *Client) SnapshotCreate/SnapshotDelete/SnapshotQuery/SnapshotClone(...)`
- `func (c *Client) SetPerm(ctx context.Context, path string, mode string, uid, gid int) error`
- `type Dataset struct { ID, Type, Mountpoint string; VolSize, RefQuota int64; UserProperties map[string]Property }`
- `type Property struct { Value, Source string }`

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestDatasetQueryAbsentReturnsNil, TestSetPermPollsJobToCompletion, TestSetPermFailsOnJobFailure, TestSetPermTimesOut.

## Acceptance criteria

- [x] Write `TestDatasetQueryAbsentReturnsNil`: fake returns the real observed error shape for a missing dataset — code `-32602`, `errname` `EINVAL`, reason `"[ENOENT] None: PoolDataset x does not exist"` — and assert `DatasetQuery` returns `(nil, nil)` rather than an error, because errname cannot be trusted. Run the test — expect FAIL with "undefined: DatasetQuery".
- [x] Write `TestSetPermPollsJobToCompletion`: fake returns integer job id `9615` from `filesystem.setperm`, then reports state `RUNNING` twice and `SUCCESS`; assert `SetPerm` returns nil and polled at least three times.
- [x] Write `TestSetPermFailsOnJobFailure`: fake reports state `FAILED` with an error string; assert `SetPerm` returns an error containing that string.
- [x] Write `TestSetPermTimesOut`: fake never leaves `RUNNING`; assert `SetPerm` returns a timeout error within the configured bound rather than blocking forever.
- [x] Implement the dataset, snapshot, share and iSCSI wrappers, each marshalling the documented parameter shapes and unmarshalling into typed structs.
- [x] Implement `SetPerm`: call `filesystem.setperm`, decode the integer job id, then poll `core.get_jobs` with filter `[["id","=",jobID]]` every 300ms until state is one of `SUCCESS`, `FAILED`, `ABORTED`, bounded by a 2-minute context.
- [x] Run `go test ./internal/truenas/` — expect PASS.
- [x] Commit: "truenas: typed dataset, share, iscsi operations and job polling".

## Evidence

- Typed dataset/share/iscsi ops and job polling; SetPerm polls core.get_jobs to a terminal state.
- `go test ./...` green across all 18 packages; `gofmt -l` and `go vet ./...` clean.
- procoder gate: 0 blocking findings. Committed on branch feat/foundation.
