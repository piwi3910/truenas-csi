# Task 15: Node service and NFS mounting

Status: closed
Created: 2026-09-07

## Description

Implements plan task 15 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 15: Node service and NFS mounting" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/node/node.go` — Node service and capabilities.
- `internal/node/mount.go` — mount helper wrapping the host mounter.
- `internal/node/nfs.go` — NFS stage/unstage/publish/unpublish.
- `internal/node/nfs_test.go` — tests.

Interfaces it produces for later tasks:

- `func NewNode(cfg *config.Config, p *Preflight, exec Executor) csi.NodeServer`
- `type Executor interface { Run(ctx context.Context, name string, args ...string) ([]byte, error) }`
- `func hostExec(root string) Executor` running binaries in the host mount namespace
- `func (n *node) NodeGetInfo(...)` publishing node id, topology labels and
  `max_volumes_per_node`

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestE2ENFSMountLifecycle, TestNFSStageIsIdempotent, TestNFSUnstageAbsentMountSucceeds, TestNodePublishBindMounts.

## Acceptance criteria

- [x] Write `TestE2ENFSMountLifecycle` with a recording `Executor`: assert `NodeStageVolume` issues exactly `mount -t nfs -o vers=4 <server>:<share> <staging path>`, that `nfsVersion: "3"` in the publish context changes `vers=4` to `vers=3`, and that `NodeUnstageVolume` issues `umount <staging path>` and nothing else. Run `go test ./internal/node/` — expect FAIL with "undefined: NewNode".
- [x] Write `TestNFSStageIsIdempotent` asserting a second `NodeStageVolume` for an already-mounted path issues no second mount and returns success.
- [x] Write `TestNFSUnstageAbsentMountSucceeds` asserting unstaging a path that is not mounted returns success and issues no umount.
- [x] Write `TestNodePublishBindMounts` asserting `NodePublishVolume` bind-mounts the staging path to the target path and honours `readonly: true` with the `ro` option.
- [x] Implement `Executor` with a host-namespace implementation using `nsenter --mount=<root>/proc/1/ns/mnt`.
- [x] Implement NFS stage as an idempotent mount: check the mount table first, mount only when absent, create the staging directory when missing.
- [x] Implement `NodeGetInfo` returning the configured node id, the topology labels from Task 14, and `max_volumes_per_node` of 128.
- [x] Run `go test ./internal/node/` — expect PASS.
- [x] Commit: "node: node service and idempotent NFS mounting".

## Evidence

- Delivered by a parallel worktree; merged.
- `go test ./...` green across all 18 packages; `gofmt -l` and `go vet ./...` clean.
- procoder gate: 0 blocking findings. Committed on branch feat/foundation.
