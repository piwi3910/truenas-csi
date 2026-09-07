# Task 1: Repository skeleton and build

Status: open
Created: 2026-09-07

## Description

Implements plan task 1 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 1: Repository skeleton and build" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `go.mod` — module declaration and dependency pins.
- `Makefile` — build, test, lint targets.
- `LICENSE` — Apache 2.0 text.
- `cmd/truenas-csi/main.go` — entrypoint, mode flag only.
- `internal/driver/driver.go` — driver name and version constants.
- `.gitignore` — build output.

Interfaces it produces for later tasks:

- `const DriverName = "csi.truenas.watteel.com"` in `internal/driver`
- `var Version string` in `internal/driver`, set via ldflags
- `func main()` accepting `-mode=controller|node`

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: no new tests — see steps.

## Acceptance criteria

- [ ] Write `internal/driver/driver_test.go` asserting the driver name is exactly `csi.truenas.watteel.com`: `func TestDriverName(t *testing.T) { if DriverName != "csi.truenas.watteel.com" { t.Fatalf("got %q", DriverName) } }` Run `go test ./internal/driver/` — expect FAIL with "no Go files" (package absent).
- [ ] Create `go.mod` with `module github.com/pwatteel/truenas-csi` and `go 1.24`.
- [ ] Create `internal/driver/driver.go` defining `DriverName` and `Version`.
- [ ] Create `cmd/truenas-csi/main.go` parsing `-mode` and rejecting any value other than `controller` or `node` with exit status 1.
- [ ] Add `Makefile` with `build`, `test`, `lint` targets; `test` runs `go test ./...`.
- [ ] Add Apache 2.0 `LICENSE`.
- [ ] Run `go test ./...` — expect PASS. Run `go build ./...` — expect no output.
- [ ] Commit: "build: repository skeleton and driver identity".

## Evidence

