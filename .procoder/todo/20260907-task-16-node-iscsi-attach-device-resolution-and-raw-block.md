# Task 16: Node iSCSI attach, device resolution and raw block

Status: closed
Created: 2026-09-07

## Description

Implements plan task 16 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 16: Node iSCSI attach, device resolution and raw block" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/node/iscsi.go` — login, logout, device resolution.
- `internal/node/block.go` — raw block publishing.
- `internal/node/iscsi_test.go` — tests.

Interfaces it produces for later tasks:

- `func resolveDevice(root, naa string) (string, error)` returning the path under
  `/dev/disk/by-id`
- `func iscsiLogin(ctx context.Context, e Executor, portal, iqn string) error`
- `func iscsiLogout(ctx context.Context, e Executor, portal, iqn string) error`
- `var ErrDeviceNotFound = errors.New("iscsi device did not appear after login")`

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestISCSIDeviceResolution, TestISCSICommandsAreScoped, TestE2EUnstageLeavesLonghornIntact, TestBlockVolumeSkipsMkfs, TestFilesystemVolumeFormatsOnce, TestStageFailsWhenFsTypeUnavailable.

## Acceptance criteria

- [x] Write `TestISCSIDeviceResolution`: build a fake host root containing `dev/disk/by-id/scsi-36589cfc000000a960e31390c2657efa7 -> ../../sdc`; assert `resolveDevice(root, "0x6589cfc000000a960e31390c2657efa7")` returns that path with the `0x` stripped and a `scsi-3` prefix applied, and assert the implementation never lists `dev/` itself — scanning races with Longhorn on the same node. Run `go test ./internal/node/ -run TestISCSIDeviceResolution` — expect FAIL with "undefined: resolveDevice".
- [x] Write `TestISCSICommandsAreScoped`, a recording `Executor` asserting every issued `iscsiadm` command contains both `-T <our iqn>` and `-p <our portal>`, and that none contains `--logoutall`, `--op delete` without a target, or `-m session -R`. Longhorn sessions must survive.
- [x] Write `TestE2EUnstageLeavesLonghornIntact`: seed the fake executor with two existing Longhorn sessions; run stage then unstage; assert the recorded commands touch only our target and that both Longhorn sessions remain in the fake's state.
- [x] Write `TestBlockVolumeSkipsMkfs` asserting a volume with `volumeMode: Block` is bind-mounted to a device file target and no `mkfs.*` command is ever issued.
- [x] Write `TestFilesystemVolumeFormatsOnce` asserting `mkfs.ext4` runs only when `blkid` reports no existing filesystem, and never on a second stage.
- [x] Write `TestStageFailsWhenFsTypeUnavailable` asserting a request for `xfs` on a preflight without `CapXFS` returns an error naming `xfsprogs`, not a mount error.
- [x] Implement `iscsiLogin` issuing `iscsiadm -m discovery -t sendtargets -p <portal>` then `iscsiadm -m node -T <iqn> -p <portal> --login`, adding CHAP node options first when the publish context carries credentials.
- [x] Implement `resolveDevice` constructing the by-id path from the NAA and polling for it with a 30-second bound, returning `ErrDeviceNotFound` on timeout.
- [x] Implement staging: resolve device, check `blkid`, format when empty using the requested fsType, then mount. For block volumes skip format and mount entirely.
- [x] Implement `iscsiLogout` issuing `--logout` then `-o delete`, both scoped with `-T` and `-p`.
- [x] Run `go test ./internal/node/` — expect PASS.
- [x] Commit: "node: scoped iscsi attach, deterministic device resolution, raw block".

## Evidence

- Delivered by a parallel worktree; merged. Mutation proved device resolution never scans /dev.
- `go test ./...` green across all 18 packages; `gofmt -l` and `go vet ./...` clean.
- procoder gate: 0 blocking findings. Committed on branch feat/foundation.
