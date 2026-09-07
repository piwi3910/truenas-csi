# Task 18: Multipath support with single-path degradation

Status: open
Created: 2026-09-07

## Description

Implements plan task 18 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 18: Multipath support with single-path degradation" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/node/multipath.go` — multipath detection and device mapping.
- `internal/node/multipath_test.go` — tests.

Interfaces it produces for later tasks:

- `func multipathDevice(ctx context.Context, e Executor, naa string) (string, bool, error)`
  returning the mapper path and whether multipath is in use

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestMultipathDegrades, TestMultipathUsesMapperDevice.

## Acceptance criteria

- [ ] Write `TestMultipathDegrades`: preflight without `CapMultipath`; assert staging still succeeds using the plain by-id device and that a warning containing `multipath-tools` is logged exactly once. Run `go test ./internal/node/ -run TestMultipathDegrades` — expect FAIL with "undefined: multipathDevice".
- [ ] Write `TestMultipathUsesMapperDevice`: preflight with `CapMultipath` and a fake `multipath -l` output naming the NAA; assert the staged device is the `/dev/mapper/<wwid>` path rather than the raw `sd` device.
- [ ] Implement `multipathDevice` invoking `multipath -l <wwid>` and parsing the mapper name, returning `false` when the capability is absent so the caller falls back.
- [ ] Implement the fallback in the staging path, logging the warning once per node start rather than per volume.
- [ ] Run `go test ./internal/node/` — expect PASS.
- [ ] Commit: "node: multipath device mapping with single-path fallback".

## Evidence

