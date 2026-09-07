# Task 14: Node capability preflight and topology

Status: closed
Created: 2026-09-07

## Description

Implements plan task 14 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 14: Node capability preflight and topology" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/node/preflight.go` — binary and module detection.
- `internal/node/topology.go` — topology labels from detected capabilities.
- `internal/node/preflight_test.go` — tests.

Interfaces it produces for later tasks:

- `type Capability string` with constants `CapNFS`, `CapISCSI`, `CapXFS`, `CapMultipath`
- `type Preflight struct { Found map[Capability]bool; Missing map[Capability][]string }`
- `func Detect(ctx context.Context, root string) (*Preflight, error)` where `root` is the
  host filesystem root, `/host` in the DaemonSet
- `func (p *Preflight) Require(c Capability) error`
- `func (p *Preflight) TopologyLabels() map[string]string`
- `var ErrCapabilityUnavailable = errors.New("node lacks the tooling this volume requires")`

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestPreflightMissingTool, TestPreflightLoadsModules, TestTopologyLabelsReflectCapabilities.

## Acceptance criteria

- [x] Write `TestPreflightMissingTool`: build a temporary fake host root containing `sbin/mkfs.ext4` but not `sbin/mkfs.xfs`; assert `Detect` reports `CapXFS` missing with `"xfsprogs"` named in `Missing`, and that `Require(CapXFS)` returns an error whose message contains `xfsprogs`. Run `go test ./internal/node/` — expect FAIL with "undefined: Detect".
- [x] Write `TestPreflightLoadsModules`: fake root whose `proc/modules` lacks `iscsi_tcp` but whose `lib/modules/<rel>/kernel/drivers/scsi/iscsi_tcp.ko` exists; assert `Detect` records the module as available and invokes the injected modprobe function exactly once, rather than reporting the capability unavailable.
- [x] Write `TestTopologyLabelsReflectCapabilities` asserting a node without `mkfs.xfs` publishes `csi.truenas.watteel.com/xfs="false"` and one with it publishes `"true"`.
- [x] Implement `Detect` searching `sbin`, `usr/sbin`, `bin`, `usr/bin` under `root` for `mount.nfs`, `iscsiadm`, `iscsid`, `mkfs.ext4`, `mkfs.xfs`, `xfs_growfs`, `multipath`, `multipathd`, mapping each missing capability to the package that provides it: xfs to `xfsprogs`, multipath to `multipath-tools`, iscsi to `open-iscsi`.
- [x] Implement module detection reading `proc/modules` under `root`, falling back to a `.ko` search under `lib/modules/<uname -r>`, and calling modprobe when the object exists but is not loaded. Treat available-but-unloaded as recoverable and not-present-at-all as unavailable.
- [x] Implement `TopologyLabels` emitting one label per capability.
- [x] Run `go test ./internal/node/` — expect PASS.
- [x] Commit: "node: capability preflight, module loading and topology labels".

## Evidence

- Delivered by a parallel worktree; merged.
- `go test ./...` green across all 18 packages; `gofmt -l` and `go vet ./...` clean.
- procoder gate: 0 blocking findings. Committed on branch feat/foundation.
