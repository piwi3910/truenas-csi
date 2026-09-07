# Task 1: Repository skeleton and build

Status: closed
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

- [x] Write `internal/driver/driver_test.go` asserting the driver name is exactly `csi.truenas.watteel.com`: `func TestDriverName(t *testing.T) { if DriverName != "csi.truenas.watteel.com" { t.Fatalf("got %q", DriverName) } }` Run `go test ./internal/driver/` — expect FAIL with "no Go files" (package absent).
- [x] Create `go.mod` with `module github.com/piwi3910/truenas-csi` and `go 1.24`.
- [x] Create `internal/driver/driver.go` defining `DriverName` and `Version`.
- [x] Create `cmd/truenas-csi/main.go` parsing `-mode` and rejecting any value other than `controller` or `node` with exit status 1.
- [x] Add `Makefile` with `build`, `test`, `lint` targets; `test` runs `go test ./...`.
- [x] Add Apache 2.0 `LICENSE`.
- [x] Run `go test ./...` — expect PASS. Run `go build ./...` — expect no output.
- [x] Commit: "build: repository skeleton and driver identity".

## Evidence

- Failing test first: `internal/driver/driver_test.go` written before any source;
  `go test ./internal/driver/` failed with "cannot find main module" — the red state.
- `go.mod` created: `module github.com/piwi3910/truenas-csi`, `go 1.24`.
- `internal/driver/driver.go` defines `DriverName = "csi.truenas.watteel.com"` and
  `Version` (default "dev", overridable via ldflags).
- `cmd/truenas-csi/main.go` gates `-mode`: `-mode=controller` exits 0,
  `-mode=bogus` printed `-mode must be "controller" or "node", got "bogus"` and exited 1.
- `Makefile` provides build, test, lint, clean; `test` runs `go test ./...`.
- `LICENSE` is the Apache 2.0 text, 202 lines, fetched from apache.org.
- `go test ./...` → ok github.com/piwi3910/truenas-csi/internal/driver.
  `go build ./...` → exit 0. `go vet ./...` → exit 0.
- Mutation check: changing DriverName to csi.truenas.example.com made
  `TestDriverName` FAIL with `got "csi.truenas.example.com"`; source restored from
  snapshot and the test passes again. The test is not vacuous.
- `procoder check` → 0 blocking findings.
- Committed as d638270 on branch feat/foundation.
