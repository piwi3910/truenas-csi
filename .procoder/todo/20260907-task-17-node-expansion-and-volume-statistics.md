# Task 17: Node expansion and volume statistics

Status: closed
Created: 2026-09-07

## Description

Implements plan task 17 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 17: Node expansion and volume statistics" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/node/expand.go` — NodeExpandVolume.
- `internal/node/stats.go` — NodeGetVolumeStats.
- `internal/node/expand_test.go`, `internal/node/stats_test.go` — tests.

Interfaces it produces for later tasks:

- `func (n *node) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error)`
- `func (n *node) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error)`

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestE2EExpandMountedISCSI, TestExpandNFSIsNoOp, TestExpandBlockVolumeIsNoOp, TestE2EVolumeStats.

## Acceptance criteria

- [x] Write `TestE2EExpandMountedISCSI` with a recording `Executor`: assert the node issues `iscsiadm -m node -T <iqn> -p <portal> -R` to rescan before growing, then `resize2fs` for ext4 or `xfs_growfs` for xfs, and that no umount is issued at any point. Run `go test ./internal/node/ -run TestE2EExpandMountedISCSI` — expect FAIL with "undefined: NodeExpandVolume".
- [x] Write `TestExpandNFSIsNoOp` asserting an NFS volume expansion performs no node-side command, since the quota change on the appliance is sufficient.
- [x] Write `TestExpandBlockVolumeIsNoOp` asserting a raw block volume triggers a rescan but no filesystem grow.
- [x] Write `TestE2EVolumeStats` against a temporary directory: assert `NodeGetVolumeStats` returns non-zero total and available bytes and an inode entry, and that a missing path returns `codes.NotFound`.
- [x] Implement `NodeExpandVolume`: for iSCSI rescan the session scoped to target and portal, wait for the block device size to change with a 30-second bound, then run the filesystem-appropriate grow command; for NFS return success without acting.
- [x] Implement `NodeGetVolumeStats` using `statfs` on the volume path, returning both byte and inode usage.
- [x] Run `go test ./internal/node/` — expect PASS.
- [x] Commit: "node: online expansion via rescan and volume statistics".

## Evidence

- Delivered by a parallel worktree; merged.
- `go test ./...` green across all 18 packages; `gofmt -l` and `go vet ./...` clean.
- procoder gate: 0 blocking findings. Committed on branch feat/foundation.
